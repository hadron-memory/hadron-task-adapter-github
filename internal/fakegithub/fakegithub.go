// Package fakegithub is an in-memory stand-in for the slice of the GitHub API
// the adapter uses: the GraphQL issue list and issue read, and the REST label,
// comment, rate-limit and repository endpoints. It exists for the adapter's
// own tests and its conformance suite; it is not a general GitHub fake.
package fakegithub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Issue is one issue of a fake repository.
type Issue struct {
	Number    int
	Title     string
	Body      string
	State     string // OPEN or CLOSED
	CreatedAt time.Time
	Labels    []string
}

// Comment is a comment the adapter posted.
type Comment struct {
	Repo   string // owner/name as the request spelled it
	Number int
	Body   string
}

// Server is a fake GitHub. Its methods are safe for concurrent use.
type Server struct {
	*httptest.Server

	// Token is the only credential the fake accepts ("Bearer <Token>").
	Token string

	mu sync.Mutex
	// repos is keyed by the lower-cased "owner/name"; canon keeps GitHub's
	// own spelling, which is what an issue URL carries.
	repos map[string][]*Issue
	canon map[string]string
	fail  map[string]int // lower-cased "owner/name" -> HTTP status for any read of it
	// repoLabels are the labels defined in a repository (not on an issue).
	repoLabels map[string][]string
	echo       string // when set, error bodies carry this text (to test redaction)
	// cursorPad lengthens every issue cursor to at least this many bytes
	// (GitHub's cursors are opaque and their length is not promised).
	cursorPad int
	// urlSuffix is appended to every issue URL in a list (a misbehaving proxy).
	urlSuffix string
	// commentFail, when set, answers every comment POST with this status
	// (the label POST before it still succeeds).
	commentFail int
	Comments    []Comment
	Requests    []string // "METHOD path" or "GRAPHQL op", in order
}

// New starts a fake GitHub that accepts token.
func New(token string) *Server {
	s := &Server{Token: token, repos: map[string][]*Issue{}, canon: map[string]string{}, fail: map[string]int{}, repoLabels: map[string][]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// AddRepo creates a repository spelled as GitHub spells it ("Owner/Name").
func (s *Server) AddRepo(full string, issues ...Issue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.ToLower(full)
	s.canon[k] = full
	for i := range issues {
		is := issues[i]
		if is.State == "" {
			is.State = "OPEN"
		}
		s.repos[k] = append(s.repos[k], &is)
	}
}

// DefineLabel creates a label in a repository's label list.
func (s *Server) DefineLabel(full, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.ToLower(full)
	s.repoLabels[k] = append(s.repoLabels[k], label)
}

// Fail makes every read of a repository answer with status.
func (s *Server) Fail(full string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[strings.ToLower(full)] = status
}

// FailComments makes every comment POST answer with status.
func (s *Server) FailComments(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commentFail = status
}

// EchoInErrors makes error bodies include text, as a misbehaving proxy might.
func (s *Server) EchoInErrors(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.echo = text
}

// PadCursors makes every issue cursor at least n bytes long.
func (s *Server) PadCursors(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursorPad = n
}

// SuffixURLs appends text to every issue URL a list returns.
func (s *Server) SuffixURLs(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.urlSuffix = text
}

// Labels returns an issue's current labels.
func (s *Server) Labels(full string, number int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is := s.find(strings.ToLower(full), number); is != nil {
		return append([]string(nil), is.Labels...)
	}
	return nil
}

// SetLabels replaces an issue's labels (a teammate relabelling by hand).
func (s *Server) SetLabels(full string, number int, labels ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is := s.find(strings.ToLower(full), number); is != nil {
		is.Labels = append([]string(nil), labels...)
	}
}

// IssueURL is the canonical issue URL GitHub would print.
func (s *Server) IssueURL(full string, number int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("https://github.com/%s/issues/%d", s.canon[strings.ToLower(full)], number)
}

func (s *Server) find(k string, number int) *Issue {
	for _, is := range s.repos[k] {
		if is.Number == number {
			return is
		}
	}
	return nil
}

func (s *Server) record(r string) {
	s.Requests = append(s.Requests, r)
}

func (s *Server) writeErr(w http.ResponseWriter, status int, msg string) {
	if s.echo != "" {
		msg += " " + s.echo
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		s.record("UNAUTHORIZED " + r.URL.Path)
		s.writeErr(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/graphql" {
		s.graphql(w, r)
		return
	}
	s.record(r.Method + " " + r.URL.Path)
	if r.URL.Path == "/rate_limit" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"resources": map[string]any{"core": map[string]int{"remaining": 4999}}})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "repos" {
		s.writeErr(w, 404, "Not Found")
		return
	}
	k := strings.ToLower(parts[1] + "/" + parts[2])
	if st := s.fail[k]; st != 0 {
		s.writeErr(w, st, "Server Error")
		return
	}
	if _, ok := s.canon[k]; !ok {
		s.writeErr(w, 404, "Not Found")
		return
	}
	if len(parts) == 3 && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"full_name": s.canon[k]})
		return
	}
	if len(parts) == 5 && parts[3] == "labels" && r.Method == http.MethodGet {
		for _, l := range s.repoLabels[k] {
			if strings.EqualFold(l, parts[4]) {
				writeJSON(w, 200, map[string]string{"name": l})
				return
			}
		}
		s.writeErr(w, 404, "Not Found")
		return
	}
	if len(parts) < 6 || parts[3] != "issues" {
		s.writeErr(w, 404, "Not Found")
		return
	}
	n, err := strconv.Atoi(parts[4])
	is := s.find(k, n)
	if err != nil || is == nil {
		s.writeErr(w, 404, "Not Found")
		return
	}
	switch {
	case parts[5] == "labels" && len(parts) == 6 && r.Method == http.MethodGet:
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if per <= 0 {
			per = 30
		}
		if page <= 0 {
			page = 1
		}
		out := []map[string]string{}
		for i := (page - 1) * per; i < len(is.Labels) && i < page*per; i++ {
			out = append(out, map[string]string{"name": is.Labels[i]})
		}
		writeJSON(w, 200, out)
	case parts[5] == "labels" && len(parts) == 6 && r.Method == http.MethodPost:
		var body struct {
			Labels []string `json:"labels"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			s.writeErr(w, 422, "Invalid request")
			return
		}
		for _, l := range body.Labels {
			if !hasFold(is.Labels, l) {
				is.Labels = append(is.Labels, l)
			}
		}
		writeJSON(w, 200, labelObjs(is.Labels))
	case parts[5] == "labels" && len(parts) == 7 && r.Method == http.MethodDelete:
		name := parts[6]
		kept := is.Labels[:0:0]
		found := false
		for _, l := range is.Labels {
			if strings.EqualFold(l, name) {
				found = true
				continue
			}
			kept = append(kept, l)
		}
		if !found {
			s.writeErr(w, 404, "Label does not exist")
			return
		}
		is.Labels = kept
		writeJSON(w, 200, labelObjs(is.Labels))
	case parts[5] == "comments" && len(parts) == 6 && r.Method == http.MethodPost:
		if s.commentFail != 0 {
			s.writeErr(w, s.commentFail, "comment refused")
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		b, _ := io.ReadAll(r.Body)
		if json.Unmarshal(b, &body) != nil || body.Body == "" {
			s.writeErr(w, 422, "Invalid request")
			return
		}
		s.Comments = append(s.Comments, Comment{Repo: parts[1] + "/" + parts[2], Number: n, Body: body.Body})
		writeJSON(w, 201, map[string]any{"id": len(s.Comments)})
	default:
		s.writeErr(w, 404, "Not Found")
	}
}

func hasFold(list []string, v string) bool {
	for _, l := range list {
		if strings.EqualFold(l, v) {
			return true
		}
	}
	return false
}

func labelObjs(ls []string) []map[string]string {
	out := []map[string]string{}
	for _, l := range ls {
		out = append(out, map[string]string{"name": l})
	}
	return out
}

func (s *Server) cursor(number int) string {
	raw := "cursor:v2:" + strconv.Itoa(number)
	if s.cursorPad > 0 {
		raw += "|"
		for len(base64.StdEncoding.EncodeToString([]byte(raw))) < s.cursorPad {
			raw += "x"
		}
	}
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

type gqlReq struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func labelConn(labels []string, first int) map[string]any {
	nodes := []map[string]string{}
	for i := 0; i < len(labels) && i < first; i++ {
		nodes = append(nodes, map[string]string{"name": labels[i]})
	}
	return map[string]any{"nodes": nodes, "pageInfo": map[string]bool{"hasNextPage": len(labels) > first}}
}

func (s *Server) graphql(w http.ResponseWriter, r *http.Request) {
	var q gqlReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		s.writeErr(w, 400, "Problems parsing JSON")
		return
	}
	s.record("GRAPHQL " + q.OperationName)
	owner, name := str(q.Variables["owner"]), str(q.Variables["name"])
	k := strings.ToLower(owner + "/" + name)
	if st := s.fail[k]; st != 0 {
		s.writeErr(w, st, "Server Error")
		return
	}
	full, ok := s.canon[k]
	if !ok {
		writeJSON(w, 200, map[string]any{"data": map[string]any{"repository": nil},
			"errors": []map[string]string{{"type": "NOT_FOUND", "message": fmt.Sprintf("Could not resolve to a Repository with the name '%s'.", owner+"/"+name)}}})
		return
	}
	labelsFirst := num(q.Variables["labelsFirst"])
	if labelsFirst <= 0 {
		labelsFirst = 100
	}
	switch q.OperationName {
	case "ListOpenIssues":
		open := []*Issue{}
		for _, is := range s.repos[k] {
			if is.State == "OPEN" {
				open = append(open, is)
			}
		}
		sort.SliceStable(open, func(i, j int) bool {
			if !open[i].CreatedAt.Equal(open[j].CreatedAt) {
				return open[i].CreatedAt.Before(open[j].CreatedAt)
			}
			return open[i].Number < open[j].Number
		})
		start := 0
		if after := str(q.Variables["after"]); after != "" {
			raw, err := base64.StdEncoding.DecodeString(after)
			pos, _, _ := strings.Cut(strings.TrimPrefix(string(raw), "cursor:v2:"), "|")
			n, err2 := strconv.Atoi(pos)
			if err != nil || err2 != nil {
				writeJSON(w, 200, map[string]any{"errors": []map[string]string{{"message": "`" + after + "` does not appear to be a valid cursor."}}})
				return
			}
			// GitHub's cursor names a position, not an issue: resume after
			// the issue with that number in the current order.
			start = len(open)
			for i, is := range open {
				if is.Number == n {
					start = i + 1
					break
				}
			}
		}
		first := num(q.Variables["first"])
		edges := []map[string]any{}
		end := start
		for i := start; i < len(open) && len(edges) < first; i++ {
			is := open[i]
			edges = append(edges, map[string]any{
				"cursor": s.cursor(is.Number),
				"node": map[string]any{
					"number": is.Number, "title": is.Title,
					"url":       fmt.Sprintf("https://github.com/%s/issues/%d", full, is.Number) + s.urlSuffix,
					"createdAt": is.CreatedAt.UTC().Format(time.RFC3339),
					"labels":    labelConn(is.Labels, labelsFirst),
				},
			})
			end = i + 1
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"repository": map[string]any{"issues": map[string]any{
			"edges":    edges,
			"pageInfo": map[string]any{"hasNextPage": end < len(open)},
		}}}})
	case "GetIssue":
		is := s.find(k, num(q.Variables["number"]))
		if is == nil {
			writeJSON(w, 200, map[string]any{"data": map[string]any{"repository": map[string]any{"issue": nil}},
				"errors": []map[string]string{{"type": "NOT_FOUND", "message": "Could not resolve to an Issue."}}})
			return
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{
			"number": is.Number, "title": is.Title, "body": is.Body, "state": is.State,
			"url":    fmt.Sprintf("https://github.com/%s/issues/%d", full, is.Number),
			"labels": labelConn(is.Labels, labelsFirst),
		}}}})
	default:
		writeJSON(w, 200, map[string]any{"errors": []map[string]string{{"message": "unknown operation " + q.OperationName}}})
	}
}
