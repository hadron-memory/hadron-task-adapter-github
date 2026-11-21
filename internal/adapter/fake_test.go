package adapter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeIssue is one issue on the fake tracker.
type fakeIssue struct {
	Number    int
	Title     string
	Body      string
	State     string // OPEN or CLOSED
	CreatedAt string
	UpdatedAt string
	Labels    []string
	PR        bool
	// ManyLabels makes the labels connection claim another page.
	ManyLabels bool
	Comments   []Comment
}

// fakeGitHub serves the slice of GitHub the adapter uses.
type fakeGitHub struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	repos    map[string][]*fakeIssue // "owner/name" -> issues
	Writes   []string                // "METHOD path" of every write
	Requests []string
	Token    string // the token the fake accepts ("" accepts any)
	Status   int    // when non-zero, every request answers it
	nextCID  int64
	// RateLimited answers 403 with the exhausted-limit header.
	RateLimited bool
	// Login is the account the token acts as (GET /user, and the author of the
	// comments the adapter writes); "-" makes GET /user answer 404 (an
	// installation token).
	Login string
	// FailNext makes the next request whose "METHOD path-suffix" matches answer 500, once.
	FailNext map[string]bool
	// RateLimitStatus overrides /rate_limit's status when non-zero.
	RateLimitStatus int
	// Moved makes an issue lookup report another repository in its html_url,
	// as GitHub does for a renamed or transferred repository.
	Moved map[string]string
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, repos: map[string][]*fakeIssue{"acme/app": nil, "acme/lib": nil}, nextCID: 1000, Login: "adapter-bot", FailNext: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) client(token string) *Client {
	return &Client{API: f.srv.URL, GraphQL: f.srv.URL + "/graphql", Token: token, HTTP: f.srv.Client()}
}

func (f *fakeGitHub) add(repo string, is *fakeIssue) *fakeIssue {
	if is.State == "" {
		is.State = "OPEN"
	}
	if is.UpdatedAt == "" {
		is.UpdatedAt = is.CreatedAt
	}
	f.repos[repo] = append(f.repos[repo], is)
	return is
}

func (f *fakeGitHub) find(repo string, n int) *fakeIssue {
	for _, is := range f.repos[repo] {
		if is.Number == n {
			return is
		}
	}
	return nil
}

func (f *fakeGitHub) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Writes...)
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, r.Method+" "+r.URL.Path)
	if f.Status != 0 {
		w.WriteHeader(f.Status)
		_, _ = io.WriteString(w, `{"message":"boom `+f.Token+`"}`)
		return
	}
	for k := range f.FailNext {
		if m, suffix, _ := strings.Cut(k, " "); m == r.Method && strings.HasSuffix(r.URL.Path, suffix) {
			delete(f.FailNext, k)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	if r.URL.Path == "/user" {
		if f.Login == "-" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"login":"`+f.Login+`"}`)
		return
	}
	if f.RateLimited {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if f.Token != "" && r.URL.Path != "/rate_limit" && r.Header.Get("Authorization") != "Bearer "+f.Token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/rate_limit" {
		if f.RateLimitStatus != 0 {
			w.WriteHeader(f.RateLimitStatus)
			return
		}
		if f.Token != "" && r.Header.Get("Authorization") != "" && r.Header.Get("Authorization") != "Bearer "+f.Token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"rate":{"limit":60}}`)
		return
	}
	if r.URL.Path == "/graphql" {
		f.graphql(w, r)
		return
	}
	f.rest(w, r)
}

func (f *fakeGitHub) graphql(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	repo := fmt.Sprint(req.Variables["owner"]) + "/" + fmt.Sprint(req.Variables["name"])
	issues, ok := f.repos[repo]
	if !ok {
		_, _ = io.WriteString(w, `{"data":{"repository":null}}`)
		return
	}
	states := map[string]bool{}
	if ss, ok := req.Variables["states"].([]any); ok {
		for _, s := range ss {
			states[fmt.Sprint(s)] = true
		}
	}
	since, _ := req.Variables["since"].(string)
	updated := strings.Contains(req.Query, "UPDATED_AT")
	var list []*fakeIssue
	for _, is := range issues {
		if is.PR || !states[is.State] || (since != "" && is.UpdatedAt < since) {
			continue
		}
		list = append(list, is)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if updated {
			return list[i].UpdatedAt < list[j].UpdatedAt
		}
		return list[i].CreatedAt < list[j].CreatedAt
	})
	start := 0
	if a, ok := req.Variables["after"].(string); ok && a != "" {
		start, _ = strconv.Atoi(a)
	}
	end := start + 100
	if end > len(list) {
		end = len(list)
	}
	type node struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		URL       string `json:"url"`
		State     string `json:"state"`
		CreatedAt string `json:"createdAt"`
		UpdatedAt string `json:"updatedAt"`
		Labels    struct {
			Nodes    []map[string]string `json:"nodes"`
			PageInfo map[string]bool     `json:"pageInfo"`
		} `json:"labels"`
	}
	var nodes []node
	for _, is := range list[start:end] {
		n := node{Number: is.Number, Title: is.Title, URL: "https://github.com/" + f.shownRepo(repo) + "/issues/" + strconv.Itoa(is.Number), State: is.State, CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt}
		for _, l := range is.Labels {
			n.Labels.Nodes = append(n.Labels.Nodes, map[string]string{"name": l})
		}
		n.Labels.PageInfo = map[string]bool{"hasNextPage": is.ManyLabels}
		nodes = append(nodes, n)
	}
	out := map[string]any{"data": map[string]any{"repository": map[string]any{"issues": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": end < len(list), "endCursor": strconv.Itoa(end)},
		"nodes":    nodes,
	}}}}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeGitHub) rest(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // repos owner name issues N [labels [name] | comments]
	if len(parts) >= 5 && parts[0] == "repos" && parts[3] == "issues" && parts[4] == "comments" && len(parts) == 6 {
		// PATCH /repos/o/n/issues/comments/ID
		repo := parts[1] + "/" + parts[2]
		id, _ := strconv.ParseInt(parts[5], 10, 64)
		f.Writes = append(f.Writes, r.Method+" "+r.URL.Path)
		var b struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		for _, is := range f.repos[repo] {
			for i := range is.Comments {
				if is.Comments[i].ID == id {
					if r.Method == http.MethodDelete {
						is.Comments = append(is.Comments[:i], is.Comments[i+1:]...)
						w.WriteHeader(http.StatusNoContent)
						return
					}
					is.Comments[i].Body = b.Body
					_, _ = io.WriteString(w, `{}`)
					return
				}
			}
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if len(parts) < 5 || parts[0] != "repos" || parts[3] != "issues" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	repo := parts[1] + "/" + parts[2]
	n, _ := strconv.Atoi(parts[4])
	is := f.find(repo, n)
	if is == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	rest := parts[5:]
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		type lab struct {
			Name string `json:"name"`
		}
		o := map[string]any{"number": is.Number, "title": is.Title, "html_url": "https://github.com/" + f.shownRepo(repo) + "/issues/" + parts[4], "state": strings.ToLower(is.State), "body": is.Body, "created_at": is.CreatedAt, "updated_at": is.UpdatedAt}
		var ls []lab
		for _, l := range is.Labels {
			ls = append(ls, lab{l})
		}
		o["labels"] = ls
		if is.PR {
			o["pull_request"] = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(o)
	case len(rest) >= 1 && rest[0] == "labels" && r.Method == http.MethodPost:
		f.Writes = append(f.Writes, r.Method+" "+r.URL.Path)
		var b struct{ Labels []string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		for _, l := range b.Labels { // GitHub adds a label idempotently
			has := false
			for _, e := range is.Labels {
				has = has || e == l
			}
			if !has {
				is.Labels = append(is.Labels, l)
			}
		}
		_, _ = io.WriteString(w, `[]`)
	case len(rest) == 2 && rest[0] == "labels" && r.Method == http.MethodDelete:
		f.Writes = append(f.Writes, r.Method+" "+r.URL.Path)
		name := rest[1]
		for i, l := range is.Labels {
			if l == name {
				is.Labels = append(is.Labels[:i], is.Labels[i+1:]...)
				_, _ = io.WriteString(w, `[]`)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case len(rest) == 1 && rest[0] == "comments" && r.Method == http.MethodGet:
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		lo, hi := (page-1)*100, page*100
		if lo > len(is.Comments) {
			lo = len(is.Comments)
		}
		if hi > len(is.Comments) {
			hi = len(is.Comments)
		}
		out := append([]Comment{}, is.Comments[lo:hi]...) // GitHub answers [], never null
		_ = json.NewEncoder(w).Encode(out)
	case len(rest) == 1 && rest[0] == "comments" && r.Method == http.MethodPost:
		f.Writes = append(f.Writes, r.Method+" "+r.URL.Path)
		var b struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.nextCID++
		c := Comment{ID: f.nextCID, Body: b.Body}
		c.User.Login = f.Login
		is.Comments = append(is.Comments, c)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(c)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeGitHub) shownRepo(repo string) string {
	if m, ok := f.Moved[repo]; ok {
		return m
	}
	return repo
}
