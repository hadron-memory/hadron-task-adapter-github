package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxResponseBytes = 16 << 20
	requestTimeout   = 20 * time.Second
	userAgent        = "hadron-task-adapter-github"
)

// githubClient calls GitHub with the team's token. The token is only ever
// put in the Authorization header: no error message, log line or answer
// carries it, and GitHub's own error text is scrubbed of it before it is
// repeated.
type githubClient struct {
	http     *http.Client
	api, gql string
	token    string
}

func newGitHub(c *Config, token string) *githubClient {
	return &githubClient{http: &http.Client{Timeout: requestTimeout}, api: c.APIURL, gql: c.GraphQLURL, token: token}
}

// apiError is a GitHub answer the adapter will not use.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	if e.Status == 0 {
		return "GitHub: " + e.Message
	}
	return fmt.Sprintf("GitHub answered %d: %s", e.Status, e.Message)
}

func (g *githubClient) scrub(s string) string {
	return g.clip(s, 200)
}

// clip quotes untrusted text on one line, cut to max runes, with the
// credential scrubbed FIRST: a cut that lands inside the credential would
// otherwise leave a prefix that no later scrub can recognise.
func (g *githubClient) clip(s string, max int) string {
	if g.token != "" {
		s = strings.ReplaceAll(s, g.token, "[redacted]")
	}
	return oneLine(s, max)
}

func (g *githubClient) do(ctx context.Context, method, endpoint string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		// A transport error can quote the URL, never the header; scrub anyway.
		return 0, nil, &apiError{Message: g.scrub(err.Error())}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, &apiError{Status: resp.StatusCode, Message: "the answer could not be read"}
	}
	if len(b) > maxResponseBytes {
		return 0, nil, &apiError{Status: resp.StatusCode, Message: "the answer is larger than the adapter reads"}
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
			return resp.StatusCode, b, &apiError{Status: resp.StatusCode, Message: "rate limited; try again later"}
		}
	}
	return resp.StatusCode, b, nil
}

func (g *githubClient) errorFrom(status int, b []byte) error {
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(b, &m)
	msg := m.Message
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &apiError{Status: status, Message: g.scrub(msg)}
}

// rest makes one REST call. ok lists the statuses that are an answer; any
// other is an error. out may be nil.
func (g *githubClient) rest(ctx context.Context, method, path string, body any, out any, ok ...int) (int, error) {
	status, b, err := g.do(ctx, method, g.api+path, body)
	if err != nil {
		return status, err
	}
	for _, s := range ok {
		if status == s {
			if out != nil && len(b) > 0 {
				if err := json.Unmarshal(b, out); err != nil {
					return status, &apiError{Status: status, Message: "an answer the adapter does not understand"}
				}
			}
			return status, nil
		}
	}
	return status, g.errorFrom(status, b)
}

// graphql makes one GraphQL query. Any error in the answer fails the call:
// a partial answer is never read as a whole one.
func (g *githubClient) graphql(ctx context.Context, op, query string, vars map[string]any, out any) error {
	status, b, err := g.do(ctx, http.MethodPost, g.gql, map[string]any{"query": query, "operationName": op, "variables": vars})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return g.errorFrom(status, b)
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return &apiError{Status: status, Message: "an answer the adapter does not understand"}
	}
	if len(env.Errors) > 0 {
		e := env.Errors[0]
		if e.Type == "NOT_FOUND" {
			return &notFoundError{msg: g.scrub(e.Message)}
		}
		return &apiError{Status: status, Message: g.scrub(e.Message)}
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return &apiError{Status: status, Message: "an answer with no data"}
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return &apiError{Status: status, Message: "an answer the adapter does not understand"}
	}
	return nil
}

type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return "GitHub: " + e.msg }

func escapePath(s string) string { return url.PathEscape(s) }
