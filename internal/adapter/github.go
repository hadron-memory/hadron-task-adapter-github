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
	"strconv"
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
// credential), GITHUB_API_URL and GITHUB_GRAPHQL_URL (GitHub Enterprise). The
// credential travels only to an https endpoint with a host and no userinfo, and
// a redirect that downgrades to http or leaves the endpoint's host is refused:
// plain http is for test clients built directly, never for the executable.
func NewClient(getenv func(string) string) (*Client, error) {
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
	for _, e := range []struct{ name, u string }{{"GITHUB_API_URL", api}, {"GITHUB_GRAPHQL_URL", gql}} { // a fixed order: the refusal names the same one every run
		name, u := e.name, e.u
		if err := checkEndpoint(u); err != nil {
			return nil, &Refusal{Code: ExitRefused, Msg: name + " is not usable: " + err.Error()}
		}
	}
	tok := getenv("GITHUB_TOKEN")
	if !validToken(tok) {
		return nil, &Refusal{Code: ExitRefused, Msg: "GITHUB_TOKEN holds characters that cannot be sent in an Authorization header (it must be visible ASCII without spaces)"}
	}
	return &Client{API: api, GraphQL: gql, Token: tok, HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: sameOriginHTTPS}}, nil
}

// validToken reports whether a token can travel in an Authorization header:
// empty (none) or visible ASCII with no space. A token with a control or other
// character would make Go's own header error quote it, so it is refused first.
func validToken(t string) bool {
	for i := 0; i < len(t); i++ {
		if t[i] <= ' ' || t[i] >= 0x7f {
			return false
		}
	}
	return true
}

// ScrubSecret removes a secret, and the spellings an error message may give it
// (quoted, URL-escaped), from text that is about to be printed.
func ScrubSecret(text, secret string) string {
	if secret == "" {
		return text
	}
	q := strconv.Quote(secret)
	for _, v := range []string{secret, q[1 : len(q)-1], url.QueryEscape(secret), url.PathEscape(secret)} {
		if v != "" {
			text = strings.ReplaceAll(text, v, "[token]")
		}
	}
	return text
}

// checkEndpoint requires an https URL with a host and no userinfo.
func checkEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("it is not a URL with a host")
	}
	if u.Scheme != "https" {
		return errors.New("it must be https, so the credential is never sent in plain text")
	}
	if u.User != nil {
		return errors.New("it must not carry userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("it must be a base URL without a query or fragment")
	}
	return nil
}

// sameOriginHTTPS refuses a redirect that leaves https or the first request's
// host, and any redirect of a request that is not a GET (this includes the
// read-only GraphQL POST: it is never replayed).
func sameOriginHTTPS(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if via[0].Method != http.MethodGet {
		return errors.New("a redirect on a write is refused, so a write is never replayed as a read")
	}
	if req.URL.Scheme != "https" {
		return errors.New("a redirect to a non-https URL is refused")
	}
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return errors.New("a redirect to another host is refused")
	}
	return nil
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
func (c *Client) scrub(s string) string { return ScrubSecret(s, c.Token) }

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
		return 0, &Refusal{Code: ExitRefused, Msg: "the GitHub request could not be built (the endpoint is not a valid URL)"}
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
	// A rate limit is a 429, or a 403 that says so: the primary limit (remaining 0),
	// a secondary limit (Retry-After) or a message naming one.
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" || strings.Contains(strings.ToLower(string(raw)), "rate limit"))) {
		return resp.StatusCode, &Refusal{Code: ExitRefused, Msg: "GitHub rate limit reached; retry after the limit resets"}
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, statusError{&Refusal{Code: ExitRefused, Msg: fmt.Sprintf("GitHub answered HTTP %d for %s %s", resp.StatusCode, method, c.scrubURL(endpoint))}, resp.StatusCode, string(raw)}
	}
	if out != nil {
		if t := bytes.TrimSpace(raw); len(t) == 0 || string(t) == "null" { // a read needs its answer: nothing is not "empty"
			return resp.StatusCode, &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("GitHub answered %s %s with no content", method, c.scrubURL(endpoint))}
		}
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
	Labels    *struct {
		Nodes    []struct{ Name string } `json:"nodes"`
		PageInfo *struct {
			HasNextPage *bool `json:"hasNextPage"`
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
					Issues *struct {
						PageInfo *struct {
							HasNextPage *bool   `json:"hasNextPage"`
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
			return nil, &Refusal{Code: ExitRefused, Msg: "GitHub refused the issues read for " + repo + ": " + cleanText(c.scrub(resp.Errors[0].Message), 300)}
		}
		if resp.Data.Repository == nil {
			return nil, &Refusal{Code: ExitRefused, Msg: repo + " was not found or the credential cannot read it"}
		}
		is := resp.Data.Repository.Issues
		// An absent connection, page info or node list is an incomplete answer,
		// never an empty result.
		if is == nil || is.PageInfo == nil || is.PageInfo.HasNextPage == nil || is.Nodes == nil {
			return nil, &Refusal{Code: ExitRefused, Msg: "GitHub's issues answer for " + repo + " is incomplete"}
		}
		for _, n := range is.Nodes {
			// Labels decide the filters: a missing or null label connection is an
			// incomplete issue, never "no labels".
			if n.Labels == nil || n.Labels.Nodes == nil || n.Labels.PageInfo == nil || n.Labels.PageInfo.HasNextPage == nil {
				return nil, &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("GitHub's answer for %s#%d has no label data, so its filters cannot be decided", repo, n.Number)}
			}
			if *n.Labels.PageInfo.HasNextPage {
				return nil, &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("%s#%d has more than %d labels, so its filters cannot be decided", repo, n.Number, labelsPerIssue)}
			}
			labels := make([]string, 0, len(n.Labels.Nodes))
			for _, l := range n.Labels.Nodes {
				labels = append(labels, l.Name)
			}
			out = append(out, Issue{Repo: repo, Number: n.Number, Title: n.Title, URL: n.URL, State: strings.ToLower(n.State), CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, Labels: labels})
		}
		if !*is.PageInfo.HasNextPage {
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
	if r.HTMLURL == "" || r.Number != n {
		return Issue{}, &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("GitHub's answer for %s#%d is incomplete (no URL or a different number)", repo, n)}
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
		// A 404 is "the label is not on the issue" only if the issue is still
		// readable and does not carry it; otherwise the target changed under us.
		is, gerr := c.GetIssue(ctx, repo, n)
		if gerr != nil {
			return gerr
		}
		for _, l := range is.Labels {
			if strings.EqualFold(l, label) {
				return &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("GitHub answered 404 removing the label from %s#%d, which still carries it", repo, n)}
			}
		}
		return nil
	}
	return err
}

// Comment is one issue comment.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// ListComments reads an issue's comments, at most pages pages of 100. When the
// last page read was full it refuses: the scan cannot say a comment is absent.
// This refusal at the bound is deliberate and accepted (see the note in
// upsertComment and adapter issue #5 for the Link-header refinement).
func (c *Client) ListComments(ctx context.Context, repo string, n, pages int) ([]Comment, error) {
	var all []Comment
	for p := 1; p <= pages; p++ {
		var page []Comment
		if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s%s/comments?per_page=100&page=%d", c.API, issuePath(repo, n), p), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 100 {
			return all, nil
		}
	}
	return nil, commentLimit{&Refusal{Code: ExitRefused, Msg: fmt.Sprintf("%s#%d has more than %d comments, so the adapter cannot tell whether its own comment exists", repo, n, pages*100)}}
}

// commentLimit is the refusal for a comment list longer than the scan bound.
type commentLimit struct{ *Refusal }

func (e commentLimit) Unwrap() error { return e.Refusal }

// DeleteComment deletes a comment by id; one already gone is not an error.
func (c *Client) DeleteComment(ctx context.Context, repo string, n int, id int64) error {
	code, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/repos/%s/issues/comments/%d", c.API, repo, id), nil, nil)
	if code == http.StatusNotFound {
		// 404 is also what GitHub says when access is lost: it counts as "already
		// gone" only if the issue's readable comment list no longer has it.
		all, lerr := c.ListComments(ctx, repo, n, 10)
		if lerr != nil {
			return &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("GitHub answered 404 deleting comment %d and its absence could not be confirmed: %s", id, cleanText(c.scrub(lerr.Error()), 200))}
		}
		for _, x := range all {
			if x.ID == id {
				return &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("GitHub answered 404 deleting comment %d, which is still on %s#%d", id, repo, n)}
			}
		}
		return nil
	}
	return err
}

// ErrNoIdentity is Login's answer when GitHub says the credential has no
// account (GET /user is refused or absent for it, as for an installation
// token): the one case in which a configured commentAuthor is trusted.
var ErrNoIdentity = errors.New("the credential names no account")

// Login reads the authenticated account's login (a personal or fine-grained
// token; an installation token has none, see Config.CommentAuthor).
func (c *Client) Login(ctx context.Context) (string, error) {
	var r struct {
		Login string `json:"login"`
	}
	_, err := c.do(ctx, http.MethodGet, c.API+"/user", nil, &r)
	// Only GitHub's own statement that this credential has no account counts as
	// "no account": a 404, or the 403 an installation token gets ("Resource not
	// accessible by integration"). A rate limit, a plain 403, a 5xx or a
	// transport failure is a failure to ask, never an answer.
	var se statusError
	if errors.As(err, &se) && (se.Status == http.StatusNotFound || (se.Status == http.StatusForbidden && strings.Contains(strings.ToLower(se.Body), "not accessible by integration"))) {
		return "", ErrNoIdentity
	}
	if err != nil {
		return "", err // a failure to ask is not "no account"
	}
	if r.Login == "" {
		return "", errors.New("GitHub did not name the account")
	}
	return r.Login, nil
}

// CreateComment posts a comment.
func (c *Client) CreateComment(ctx context.Context, repo string, n int, body string) (Comment, error) {
	var created Comment
	if _, err := c.do(ctx, http.MethodPost, c.API+issuePath(repo, n)+"/comments", map[string]any{"body": body}, &created); err != nil {
		return Comment{}, err
	}
	if created.ID == 0 {
		return Comment{}, &Refusal{Code: ExitRefused, Msg: "GitHub's answer to the comment does not name it"}
	}
	return created, nil
}

// UpdateComment edits a comment by id.
func (c *Client) UpdateComment(ctx context.Context, repo string, id int64, body string) error {
	_, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/repos/%s/issues/comments/%d", c.API, repo, id), map[string]any{"body": body}, nil)
	return err
}

// Ping asks GitHub for the rate limit, which needs no scope. reachable means
// GitHub answered; accepted is true only when a credential was sent and GitHub
// answered 2xx. A 401 or 403 means the credential was refused; any other status
// proves nothing about it, so accepted stays false and err says what came back.
func (c *Client) Ping(ctx context.Context) (reachable, accepted bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.API+"/rate_limit", nil)
	if err != nil {
		return false, false, errors.New("the GitHub request could not be built (the endpoint is not a valid URL)")
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
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return true, false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, c.Token != "", nil
	}
	return true, false, fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
}

// statusError is a refusal for a non-2xx answer that keeps the status and the
// body, so a caller can tell what GitHub actually said.
type statusError struct {
	*Refusal
	Status int
	Body   string
}

func (e statusError) Unwrap() error { return e.Refusal }
