package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultAPI     = "https://api.github.com"
	defaultGraphQL = "https://api.github.com/graphql"
	issuesPerPage  = 100
	labelsPerIssue = 100
	// maxBody bounds one GitHub answer; the adapter's own stdout is capped far lower.
	maxBody = 8 << 20
)

// Client talks to GitHub with the team's own credential.
type Client struct {
	API     string // REST base, no trailing slash
	GraphQL string
	Token   string
	HTTP    *http.Client
}

// NewClient builds a client from the environment: GITHUB_TOKEN (the team's
// credential), GITHUB_API_URL and GITHUB_GRAPHQL_URL (GitHub Enterprise).
func NewClient(getenv func(string) string) *Client {
	api := strings.TrimRight(getenv("GITHUB_API_URL"), "/")
	if api == "" {
		api = defaultAPI
	}
	gql := getenv("GITHUB_GRAPHQL_URL")
	if gql == "" {
		if api == defaultAPI {
			gql = defaultGraphQL
		} else {
			gql = strings.TrimSuffix(api, "/v3") + "/graphql"
		}
	}
	return &Client{API: api, GraphQL: gql, Token: getenv("GITHUB_TOKEN"), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Issue is one tracker item as the adapter reads it.
type Issue struct {
	Repo      string
	Number    int
	Title     string
	URL       string
	State     string // "open" or "closed"
	Body      string
	CreatedAt string
	UpdatedAt string
	Labels    []string
}

// scrub removes the credential from any text that could reach stderr.
func (c *Client) scrub(s string) string {
	if c.Token != "" {
		s = strings.ReplaceAll(s, c.Token, "[token]")
	}
	return s
}

func (c *Client) do(ctx context.Context, method, endpoint string, body any, out any) (int, error) {
	if c.Token == "" {
		return 0, &Refusal{Code: ExitRefused, Msg: "GITHUB_TOKEN is not set: the adapter uses the team's own GitHub credential"}
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "hadron-task-adapter-github")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, &Refusal{Code: ExitRefused, Msg: "GitHub is unreachable: " + c.scrub(err.Error())}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return resp.StatusCode, &Refusal{Code: ExitRefused, Msg: "GitHub answer could not be read: " + c.scrub(err.Error())}
	}
	if len(raw) > maxBody {
		return resp.StatusCode, &Refusal{Code: ExitRefused, Msg: "GitHub answer is larger than the adapter will read"}
	}
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return resp.StatusCode, &Refusal{Code: ExitRefused, Msg: "GitHub rate limit reached; retry after the limit resets"}
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("GitHub answered HTTP %d for %s %s", resp.StatusCode, method, c.scrubURL(endpoint))}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, &Refusal{Code: ExitRefused, Msg: "GitHub answered something that is not the expected JSON"}
		}
	}
	return resp.StatusCode, nil
}

func (c *Client) scrubURL(u string) string { return c.scrub(strings.TrimPrefix(u, c.API)) }

const issuesQuery = `query($owner:String!,$name:String!,$after:String,$states:[IssueState!]%s){
  repository(owner:$owner,name:$name){
    issues(states:$states,first:%d,after:$after%s,orderBy:{field:%s,direction:ASC}){
      pageInfo{hasNextPage endCursor}
      nodes{
        number title url state createdAt updatedAt
        labels(first:%d){nodes{name} pageInfo{hasNextPage}}
      }
    }
  }
}`

type rawIssue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	State     string `json:"state"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Labels    struct {
		Nodes    []struct{ Name string } `json:"nodes"`
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
	} `json:"labels"`
}

// ReadIssues reads the issues of one repository in the given states
// (OPEN, CLOSED), oldest created first; with since set it reads those updated
// since then, ordered by update. It refuses, never returns a part, when the
// repository has more than maxPages pages or an issue's labels run past one page.
func (c *Client) ReadIssues(ctx context.Context, repo string, states []string, since string, maxPages int) ([]Issue, error) {
	owner, name, _ := strings.Cut(repo, "/")
	var out []Issue
	var after *string
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("%s has more than %d pages of issues; narrow the repositories or raise maxPagesPerRepository", repo, maxPages)}
		}
		varDecl, filter, order := "", "", "CREATED_AT"
		vars := map[string]any{"owner": owner, "name": name, "after": after, "states": states}
		if since != "" {
			varDecl, filter, order = ",$since:DateTime", ",filterBy:{since:$since}", "UPDATED_AT"
			vars["since"] = since
		}
		q := fmt.Sprintf(issuesQuery, varDecl, issuesPerPage, filter, order, labelsPerIssue)
		var resp struct {
			Data struct {
				Repository *struct {
					Issues struct {
						PageInfo struct {
							HasNextPage bool    `json:"hasNextPage"`
							EndCursor   *string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []rawIssue `json:"nodes"`
					} `json:"issues"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if _, err := c.do(ctx, http.MethodPost, c.GraphQL, map[string]any{"query": q, "variables": vars}, &resp); err != nil {
			return nil, err
		}
		if len(resp.Errors) > 0 {
			return nil, &Refusal{Code: ExitRefused, Msg: "GitHub refused the issues read for " + repo + ": " + c.scrub(resp.Errors[0].Message)}
		}
		if resp.Data.Repository == nil {
			return nil, &Refusal{Code: ExitRefused, Msg: repo + " was not found or the credential cannot read it"}
		}
		is := resp.Data.Repository.Issues
		for _, n := range is.Nodes {
			if n.Labels.PageInfo.HasNextPage {
				return nil, &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("%s#%d has more than %d labels, so its filters cannot be decided", repo, n.Number, labelsPerIssue)}
			}
			labels := make([]string, 0, len(n.Labels.Nodes))
			for _, l := range n.Labels.Nodes {
				labels = append(labels, l.Name)
			}
			out = append(out, Issue{Repo: repo, Number: n.Number, Title: n.Title, URL: n.URL, State: strings.ToLower(n.State), CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, Labels: labels})
		}
		if !is.PageInfo.HasNextPage {
			return out, nil
		}
		if is.PageInfo.EndCursor == nil {
			return nil, &Refusal{Code: ExitRefused, Msg: "GitHub did not say where the next page of " + repo + " starts"}
		}
		after = is.PageInfo.EndCursor
	}
}

func issuePath(repo string, n int) string { return fmt.Sprintf("/repos/%s/issues/%d", repo, n) }

// GetIssue reads one issue, with its body.
func (c *Client) GetIssue(ctx context.Context, repo string, n int) (Issue, error) {
	var r struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		HTMLURL   string `json:"html_url"`
		State     string `json:"state"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
		PR        any    `json:"pull_request"`
		Labels    []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	code, err := c.do(ctx, http.MethodGet, c.API+issuePath(repo, n), nil, &r)
	if code == http.StatusNotFound {
		return Issue{}, &Refusal{Code: ExitNotFound, Msg: fmt.Sprintf("%s#%d was not found", repo, n)}
	}
	if err != nil {
		return Issue{}, err
	}
	if r.PR != nil {
		return Issue{}, &Refusal{Code: ExitUsage, Msg: fmt.Sprintf("%s#%d is a pull request, not an issue", repo, n)}
	}
	iss := Issue{Repo: repo, Number: r.Number, Title: r.Title, URL: r.HTMLURL, State: r.State, Body: r.Body, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	for _, l := range r.Labels {
		iss.Labels = append(iss.Labels, l.Name)
	}
	return iss, nil
}

// AddLabels adds labels to an issue (GitHub creates a missing label).
func (c *Client) AddLabels(ctx context.Context, repo string, n int, labels ...string) error {
	if len(labels) == 0 {
		return nil
	}
	_, err := c.do(ctx, http.MethodPost, c.API+issuePath(repo, n)+"/labels", map[string]any{"labels": labels}, nil)
	return err
}

// RemoveLabel removes one label; a label the issue does not carry is not an error.
func (c *Client) RemoveLabel(ctx context.Context, repo string, n int, label string) error {
	code, err := c.do(ctx, http.MethodDelete, c.API+issuePath(repo, n)+"/labels/"+url.PathEscape(label), nil, nil)
	if code == http.StatusNotFound {
		return nil
	}
	return err
}

// Comment is one issue comment.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// ListComments reads at most pages pages of an issue's comments.
func (c *Client) ListComments(ctx context.Context, repo string, n, pages int) ([]Comment, error) {
	var all []Comment
	for p := 1; p <= pages; p++ {
		var page []Comment
		if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s%s/comments?per_page=100&page=%d", c.API, issuePath(repo, n), p), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 100 {
			break
		}
	}
	return all, nil
}

// CreateComment posts a comment.
func (c *Client) CreateComment(ctx context.Context, repo string, n int, body string) error {
	_, err := c.do(ctx, http.MethodPost, c.API+issuePath(repo, n)+"/comments", map[string]any{"body": body}, nil)
	return err
}

// UpdateComment edits a comment by id.
func (c *Client) UpdateComment(ctx context.Context, repo string, id int64, body string) error {
	_, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/repos/%s/issues/comments/%d", c.API, repo, id), map[string]any{"body": body}, nil)
	return err
}

// Ping asks GitHub for the rate limit, which needs no scope. It reports whether
// GitHub answered at all and whether the credential (when there is one) was
// accepted. It sends the token only when one is set.
func (c *Client) Ping(ctx context.Context) (reachable, credentialAccepted bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.API+"/rate_limit", nil)
	if err != nil {
		return false, false, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "hadron-task-adapter-github")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, false, errors.New(c.scrub(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return true, false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, c.Token != "", nil
	}
	return true, c.Token != "", fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
}
