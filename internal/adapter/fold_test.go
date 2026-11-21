package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---- the credential travels only to https, and only to the endpoint's own host ----

func TestNewClientRefusesAnEndpointThatWouldSendTheTokenInPlainText(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for name, m := range map[string]map[string]string{
		"http api":          {"GITHUB_API_URL": "http://ghe.example.com/api/v3"},
		"http graphql":      {"GITHUB_API_URL": "https://ghe.example.com/api/v3", "GITHUB_GRAPHQL_URL": "http://ghe.example.com/api/graphql"},
		"userinfo":          {"GITHUB_API_URL": "https://u:p@ghe.example.com/api/v3"},
		"no host":           {"GITHUB_API_URL": "https:///api"},
		"not a url":         {"GITHUB_API_URL": "ghe"},
		"ftp":               {"GITHUB_API_URL": "ftp://ghe.example.com"},
		"graphql no scheme": {"GITHUB_API_URL": "https://ghe.example.com/api/v3", "GITHUB_GRAPHQL_URL": "ghe.example.com/graphql"},
	} {
		if _, err := NewClient(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c, err := NewClient(env(map[string]string{"GITHUB_TOKEN": "t"}))
	if err != nil || c.API != "https://api.github.com" || c.GraphQL != "https://api.github.com/graphql" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = NewClient(env(map[string]string{"GITHUB_API_URL": "https://ghe.example.com/api/v3/"}))
	if err != nil || c.GraphQL != "https://ghe.example.com/api/graphql" {
		t.Fatalf("enterprise: %+v %v", c, err)
	}
}

func TestRedirectsMustStayOnTheOriginOverHTTPS(t *testing.T) {
	first, _ := http.NewRequest("GET", "https://api.example.com/x", nil)
	for name, tc := range map[string]struct {
		to string
		ok bool
	}{
		"same host":           {"https://api.example.com/y", true},
		"same host, any case": {"https://API.example.com/y", true},
		"downgrade":           {"http://api.example.com/y", false},
		"another host":        {"https://evil.example.net/y", false},
	} {
		next, _ := http.NewRequest("GET", tc.to, nil)
		if err := sameOriginHTTPS(next, []*http.Request{first}); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	via := make([]*http.Request, 5)
	for i := range via {
		via[i] = first
	}
	next, _ := http.NewRequest("GET", "https://api.example.com/y", nil)
	if sameOriginHTTPS(next, via) == nil {
		t.Error("an endless redirect chain must stop")
	}
}

// The credential never reaches a plain-http host a redirect points at.
func TestTokenIsNotSentWhereARedirectDowngrades(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/steal", http.StatusFound)
	}))
	defer tls.Close()
	hc := tls.Client()
	hc.CheckRedirect = sameOriginHTTPS
	c := &Client{API: tls.URL, GraphQL: tls.URL + "/graphql", Token: token, HTTP: hc}
	_, err := c.GetIssue(context.Background(), "acme/app", 1)
	var r *Refusal
	if !errors.As(err, &r) || strings.Contains(r.Msg, token) {
		t.Fatalf("the redirect must be refused without the token in the message: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Fatalf("the plain-http host was reached with %v", seen)
	}
}

// ---- a comment is the adapter's own only if its author and its marker line say so ----

func TestAnotherAuthorsMarkerCommentIsNeitherEditedNorBlocking(t *testing.T) {
	f := newFake(t)
	foreign := Comment{ID: 5, Body: commentMarker + "evil -->\nforeign text"}
	foreign.User.Login = "someone-else"
	malformed := Comment{ID: 6, Body: commentMarker + " not a marker line"}
	malformed.User.Login = f.Login
	is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", Comments: []Comment{foreign, malformed}})
	cfg := baseConfig()
	cfg.Comment = true
	a := newAdapter(t, f, cfg)
	mustCall(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1)))
	if is.Comments[0].Body != foreign.Body || is.Comments[1].Body != malformed.Body {
		t.Fatalf("a comment that is not the adapter's was edited: %+v", is.Comments)
	}
	if len(is.Comments) != 3 || !strings.HasPrefix(is.Comments[2].Body, commentMarker+"t-1 -->") {
		t.Fatalf("the adapter must write its own comment: %+v", is.Comments)
	}
	// Releasing touches only its own comment.
	mustCall(t, a, "untrack", fmt.Sprintf(`{"externalId":%q}`, issueURL("acme/app", 1)))
	if is.Comments[0].Body != foreign.Body || is.Comments[1].Body != malformed.Body || !strings.Contains(is.Comments[2].Body, "No longer tracked") {
		t.Fatalf("%+v", is.Comments)
	}
}

func TestCommentModeNeedsAKnownAccount(t *testing.T) {
	f := newFake(t)
	f.Login = "-" // an installation token: GET /user answers 404
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	cfg := baseConfig()
	cfg.Comment = true
	if _, r := call(t, newAdapter(t, f, cfg), "mark-tracked", markRequest(issueURL("acme/app", 1))); r == nil || r.Code != ExitRefused || !strings.Contains(r.Msg, "commentAuthor") {
		t.Fatalf("%+v", r)
	}
	// commentAuthor names the account, so no GET /user is needed.
	f2 := newFake(t)
	f2.Login = "-"
	is := f2.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	cfg.CommentAuthor = "my-app[bot]"
	f2.Login = "my-app[bot]"
	a := newAdapter(t, f2, cfg)
	mustCall(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1)))
	if len(is.Comments) != 1 {
		t.Fatalf("%+v", is.Comments)
	}
}

// ---- writes go only to issues that exist; a missing item is never an absent label ----

func TestWritesRefusePullRequestsAndMissingItems(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 3, Title: "a pull request", PR: true, CreatedAt: "2026-01-01T00:00:00Z", Labels: []string{"hadron:tracked"}})
	cfg := baseConfig()
	cfg.TrackedLabel, cfg.Comment = "hadron:tracked", true
	cfg.Report = map[string]ReportAction{"FINISHED": {AddLabels: []string{"done"}}}
	a := newAdapter(t, f, cfg)
	pr := issueURL("acme/app", 3)
	for name, tc := range map[string]struct{ call, req string }{
		"mark-tracked": {"mark-tracked", markRequest(pr)},
		"untrack":      {"untrack", fmt.Sprintf(`{"externalId":%q}`, pr)},
		"report":       {"report", fmt.Sprintf(`{"externalId":%q,"task":{"id":"t"},"state":"FINISHED"}`, pr)},
	} {
		if _, r := call(t, a, tc.call, tc.req); r == nil || r.Code != ExitUsage {
			t.Errorf("%s on a pull request: %+v", name, r)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a pull request was written to: %v", w)
	}
	// A missing issue: a label-only untrack must not report success from a 404.
	missing := issueURL("acme/app", 99)
	cfg.Comment = false
	if _, r := call(t, a, "untrack", fmt.Sprintf(`{"externalId":%q}`, missing)); r == nil || r.Code != ExitNotFound {
		t.Fatalf("untrack of a missing issue: %+v", r)
	}
	if _, r := call(t, a, "mark-tracked", markRequest(missing)); r == nil || r.Code != ExitNotFound {
		t.Fatalf("mark of a missing issue: %+v", r)
	}
}

// ---- the comment scan is complete or it refuses ----

func TestACommentScanThatCannotBeCompleteRefuses(t *testing.T) {
	f := newFake(t)
	var cs []Comment
	for i := 1; i <= 1000; i++ { // exactly the ten pages the scan reads: the last page is full
		c := Comment{ID: int64(i), Body: "human"}
		c.User.Login = "someone"
		cs = append(cs, c)
	}
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", Comments: cs})
	cfg := baseConfig()
	cfg.Comment = true
	_, r := call(t, newAdapter(t, f, cfg), "mark-tracked", markRequest(issueURL("acme/app", 1)))
	if r == nil || r.Code != ExitRefused || !strings.Contains(r.Msg, "comments") {
		t.Fatalf("%+v", r)
	}
	for _, w := range f.writes() {
		if strings.Contains(w, "POST") && strings.HasSuffix(w, "/comments") {
			t.Fatalf("a comment was created without proving the adapter had none: %v", f.writes())
		}
	}
}

// ---- partial writes repair on retry; simultaneous marks end with one comment ----

func TestAFailureBetweenTheLabelAndTheCommentIsRepairedByARetry(t *testing.T) {
	f := newFake(t)
	is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	cfg := baseConfig()
	cfg.TrackedLabel, cfg.Comment = "hadron:tracked", true
	a := newAdapter(t, f, cfg)
	f.FailNext["POST /comments"] = true
	if _, r := call(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1))); r == nil || r.Code != ExitRefused {
		t.Fatalf("the comment failure must be reported: %+v", r)
	}
	if fmt.Sprint(is.Labels) != "[hadron:tracked]" || len(is.Comments) != 0 {
		t.Fatalf("labels %v comments %+v: the partial state is what a retry must repair", is.Labels, is.Comments)
	}
	mustCall(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1)))
	if len(is.Labels) != 1 || len(is.Comments) != 1 {
		t.Fatalf("after the retry: labels %v comments %+v", is.Labels, is.Comments)
	}
}

func TestSimultaneousMarksEndWithOneComment(t *testing.T) {
	f := newFake(t)
	is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	cfg := baseConfig()
	cfg.Comment = true
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := newAdapter(t, f, cfg)
			if _, err := a.Handle(context.Background(), "mark-tracked", strings.NewReader(withVersion(markRequest(issueURL("acme/app", 1))))); err != nil {
				t.Errorf("%v", err)
			}
		}()
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	own := 0
	for _, c := range is.Comments {
		if strings.HasPrefix(c.Body, commentMarker) {
			own++
		}
	}
	if own != 1 {
		t.Fatalf("%d adapter comments after simultaneous marks: %+v", own, is.Comments)
	}
}

// ---- a request is one JSON object of this contract version, within bounds ----

func TestMalformedRequestsAreRefusedBeforeAnyAction(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	cfg := baseConfig()
	cfg.TrackedLabel = "hadron:tracked"
	a := newAdapter(t, f, cfg)
	good := markRequest(issueURL("acme/app", 1))
	for name, req := range map[string]string{
		"null":                  `null`,
		"a number":              `1`,
		"a string":              `"x"`,
		"two objects":           good + " " + good,
		"trailing text":         good + "x",
		"trailing past the cap": good + strings.Repeat(" ", maxRequest),
		"no contractVersion":    `{"externalId":"` + issueURL("acme/app", 1) + `"}`,
		"a worker that is 5":    strings.Replace(good, `"worker":{`, `"worker":5,"x":{`, 1),
		"a worker as a list":    strings.Replace(good, `"worker":{`, `"worker":[],"x":{`, 1),
		"over the size cap":     strings.Replace(good, `{`, `{"pad":"`+strings.Repeat("a", maxRequest)+`",`, 1),
	} {
		out, err := a.Handle(context.Background(), "mark-tracked", strings.NewReader(req))
		var r *Refusal
		if err == nil || !errors.As(err, &r) || r.Code != ExitUsage {
			t.Errorf("%s: %v %v", name, out, err)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a malformed request wrote: %v", w)
	}
}

// ---- check says only what it knows ----

func TestCheckNeedsAffirmativeEvidenceOfAnAcceptedCredential(t *testing.T) {
	for name, tc := range map[string]struct {
		status       int
		valid, reach bool
	}{
		"ok":  {0, true, true},
		"404": {404, false, true},
		"500": {500, false, true},
		"401": {401, false, true},
		"403": {403, false, true},
	} {
		f := newFake(t)
		f.RateLimitStatus = tc.status
		a := &Adapter{Config: baseConfig(), Client: f.client(token)}
		m := mustCall(t, a, "check", `{}`)
		if m["credentialValid"] != tc.valid || m["reachable"] != tc.reach || m["credentialPresent"] != true {
			t.Errorf("%s: %v", name, m)
		}
	}
}

func TestCheckRunsWithoutAConfigAndSaysWhy(t *testing.T) {
	f := newFake(t)
	a := &Adapter{Client: f.client(token), ConfigError: "the config /x/c.json is not valid: repositories is empty"}
	m := mustCall(t, a, "check", `{}`)
	if m["configValid"] != false || !strings.Contains(fmt.Sprint(m["configDetail"]), "repositories is empty") || m["reachable"] != true {
		t.Fatalf("%v", m)
	}
}

// ---- report states are compared without regard to case, on both sides ----

func TestReportStatesAreCaseInsensitiveAndCollisionsRefused(t *testing.T) {
	f := newFake(t)
	is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	c := &Config{Repositories: []string{"acme/app"}, Report: map[string]ReportAction{"finished": {AddLabels: []string{"done"}}}}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	a := newAdapter(t, f, c)
	mustCall(t, a, "report", fmt.Sprintf(`{"externalId":%q,"task":{"id":"t"},"state":"FINISHED"}`, issueURL("acme/app", 1)))
	if fmt.Sprint(is.Labels) != "[done]" {
		t.Fatalf("a config key in lower case must match an upper-case state: %v", is.Labels)
	}
	dup := &Config{Repositories: []string{"acme/app"}, Report: map[string]ReportAction{"FINISHED": {}, "finished": {}}}
	if err := dup.normalize(); err == nil {
		t.Fatal("two report keys that differ only in case must be refused")
	}
}

// Joost F1: a request that cannot even be built must not print the credential,
// in any spelling Go's own error might give it.
func TestARequestThatCannotBeBuiltDoesNotEchoTheToken(t *testing.T) {
	const tok = "ghp_SYNTH3TIC"
	c := &Client{API: "https://h.example/" + tok + "\x7f", GraphQL: "https://h.example/" + tok + "\x7f", Token: tok, HTTP: http.DefaultClient}
	_, err1 := c.GetIssue(context.Background(), "a/b", 1)
	_, _, err2 := c.Ping(context.Background())
	for _, err := range []error{err1, err2} {
		if err == nil || strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), "SYNTH3TIC") {
			t.Errorf("error must exist and must not hold the token: %v", err)
		}
	}
}

func TestScrubSecretCoversTheSpellingsAnErrorMayUse(t *testing.T) {
	const tok = "ghp_a b\x7f"
	for _, text := range []string{"x " + tok, "x " + strconv.Quote(tok), "x " + url.QueryEscape(tok), "x " + url.PathEscape(tok)} {
		if got := ScrubSecret(text, tok); strings.Contains(got, "ghp_") {
			t.Errorf("%q -> %q", text, got)
		}
	}
	if ScrubSecret("abc", "") != "abc" {
		t.Error("an empty secret must change nothing")
	}
}

func TestANonHeaderTokenIsRefusedWithoutEchoing(t *testing.T) {
	_, err := NewClient(func(k string) string {
		if k == "GITHUB_TOKEN" {
			return "ghp_SYNTH3TIC\nX"
		}
		return ""
	})
	if err == nil || strings.Contains(err.Error(), "SYNTH3TIC") {
		t.Fatalf("%v", err)
	}
}

// ---- fold 3 (blind F1-F6, QA flake) ----

func TestCheckDoesNotPrintTheTokenInItsConfigDetail(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, baseConfig())
	a.ConfigError = "cannot read the config /home/" + token + "/c.json"
	m := mustCall(t, a, "check", `{}`)
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), token) {
		t.Fatalf("check printed the token: %s", b)
	}
}

func TestAWriteToAMovedRepositoryIsRefusedAndNothingIsWritten(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	f.Moved = map[string]string{"acme/app": "elsewhere/app"}
	a := newAdapter(t, f, &Config{Repositories: []string{"acme/app"}, TrackedLabel: "hadron-tracked"})
	_ = a.Config.normalize()
	if _, r := call(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1))); r == nil {
		t.Fatal("a moved repository was written")
	}
	if len(f.Writes) != 0 {
		t.Fatalf("writes: %v", f.Writes)
	}
}

func TestAWriteIsNeverFollowedThroughARedirect(t *testing.T) {
	via := []*http.Request{{Method: http.MethodPost, URL: &url.URL{Scheme: "https", Host: "h"}}}
	req := &http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: "https", Host: "h"}}
	if err := sameOriginHTTPS(req, via); err == nil {
		t.Fatal("a redirect on a POST must be refused")
	}
	via[0].Method = http.MethodGet
	if err := sameOriginHTTPS(req, via); err != nil {
		t.Fatalf("a same-origin read redirect is allowed: %v", err)
	}
}

func TestATaskIDOutsideTheMarkerAlphabetIsRefusedBeforeAnyWrite(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	a := newAdapter(t, f, &Config{Repositories: []string{"acme/app"}, Comment: true})
	_ = a.Config.normalize()
	req := strings.Replace(markRequest(issueURL("acme/app", 1)), `"id":"t-1"`, `"id":"t 1/é"`, 1)
	for i := 0; i < 2; i++ {
		if _, r := call(t, a, "mark-tracked", req); r == nil || r.Code != ExitUsage {
			t.Fatalf("want a usage refusal, got %v", r)
		}
	}
	if len(f.Writes) != 0 {
		t.Fatalf("writes: %v", f.Writes)
	}
}

func TestDescribeRefusesAMalformedRequest(t *testing.T) {
	a := &Adapter{Host: "github.com"}
	for _, in := range []string{``, `not json`, `{"contractVersion":2}`, `{"contractVersion":1} {}`} {
		if _, err := a.Handle(context.Background(), "describe", strings.NewReader(in)); err == nil {
			t.Errorf("describe accepted %q", in)
		}
	}
}

func TestAConfigWithAStrayClosingDelimiterIsRefused(t *testing.T) {
	for _, tail := range []string{"}", "]", " ]", "\n}"} {
		p := filepath.Join(t.TempDir(), "c.json")
		_ = os.WriteFile(p, []byte(`{"repositories":["a/b"]}`+tail), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("accepted trailing %q", tail)
		}
	}
}

func TestAMarkRemovesTheDuplicateCommentsAConcurrentMarkLeft(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	cfg := &Config{Repositories: []string{"acme/app"}, Comment: true}
	_ = cfg.normalize()
	a := newAdapter(t, f, cfg)
	// The extra comment a concurrent mark created: seed it, then mark.
	for i := 0; i < 2; i++ {
		c := Comment{ID: int64(500 + i), Body: trackedComment(taskIn{ID: "t-1"}, workerIn{Name: "Iris"})}
		c.User.Login = f.Login
		f.repos["acme/app"][0].Comments = append(f.repos["acme/app"][0].Comments, c)
	}
	mustCall(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1)))
	got := f.repos["acme/app"][0].Comments
	if len(got) != 1 || got[0].ID != 500 {
		t.Fatalf("want only the oldest comment (500) to survive, got %+v", got)
	}
}

// ---- fold 4 (blind round 3: F1-F7) ----

func commentCfg() *Config {
	c := &Config{Repositories: []string{"acme/app"}, Comment: true}
	_ = c.normalize()
	return c
}

func TestAMarkThatPostsTheThousandthCommentSucceeds(t *testing.T) { // F1
	f := newFake(t)
	is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	for i := 0; i < 999; i++ {
		is.Comments = append(is.Comments, Comment{ID: int64(i + 1), Body: "chatter"})
	}
	mustCall(t, newAdapter(t, f, commentCfg()), "mark-tracked", markRequest(issueURL("acme/app", 1)))
	marked := 0
	for _, c := range is.Comments {
		if markerLine.MatchString(c.Body) {
			marked++
		}
	}
	if marked != 1 || len(is.Comments) != 1000 {
		t.Fatalf("marked %d of %d comments", marked, len(is.Comments))
	}
}

func TestAWrongCommentAuthorWritesNothing(t *testing.T) { // F2, a token that names its account
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	cfg := commentCfg()
	cfg.CommentAuthor = "someone-else"
	a := newAdapter(t, f, cfg)
	if _, r := call(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1))); r == nil {
		t.Fatal("a mismatched commentAuthor was accepted")
	}
	if len(f.Writes) != 0 {
		t.Fatalf("writes: %v", f.Writes)
	}
}

func TestAWrongCommentAuthorOnAnInstallationTokenRemovesThePostedComment(t *testing.T) { // F2, no GET /user
	f := newFake(t)
	f.Login = "-" // GET /user answers 404, and the fake posts as "-"
	is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	cfg := commentCfg()
	cfg.CommentAuthor = "wrong-bot"
	a := newAdapter(t, f, cfg)
	if _, r := call(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1))); r == nil {
		t.Fatal("a comment posted by another account was accepted")
	}
	if len(is.Comments) != 0 {
		t.Fatalf("the unrecognisable comment was left: %+v", is.Comments)
	}
}

func TestAnIssueThatMovedOutOfScopeIsNeitherListedNorNormalized(t *testing.T) { // F3
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	f.Moved = map[string]string{"acme/app": "elsewhere/app"}
	a := newAdapter(t, f, &Config{Repositories: []string{"acme/app"}})
	_ = a.Config.normalize()
	if _, r := call(t, a, "list-untracked", `{}`); r == nil {
		t.Error("list advertised an issue outside the configured scope")
	}
	if _, r := call(t, a, "normalize", `{"reference":"acme/app#1"}`); r == nil {
		t.Error("normalize named an issue outside the configured scope")
	}
}

func TestACheckOfABadSetupStillJudgesTheRequestAndTheConfigSeparately(t *testing.T) { // F5
	cfg := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(cfg, []byte(`{"repositories":["a/b"]}`), 0o600)
	a := &Adapter{Host: "github.com", ClientError: "GITHUB_API_URL is not usable", CredentialAsked: true}
	if _, err := a.Handle(context.Background(), "check", strings.NewReader(`not json`)); err == nil {
		t.Error("a malformed request must be refused even when the setup is bad")
	}
	out, err := a.Handle(context.Background(), "check", strings.NewReader(`{"contractVersion":1}`))
	m := out.(map[string]any)
	if err != nil || m["configValid"] != false || m["credentialPresent"] != true {
		t.Fatalf("%v %v", m, err)
	}
	a.Config = baseConfig()
	out, _ = a.Handle(context.Background(), "check", strings.NewReader(`{"contractVersion":1}`))
	if out.(map[string]any)["configValid"] != true {
		t.Errorf("a valid config was reported invalid: %v", out)
	}
}

func TestAnIncompleteSuccessfulAnswerIsNotAnAnswer(t *testing.T) { // F6
	for name, body := range map[string]string{"empty": ``, "null": `null`, "no url": `{"number":1,"title":"t","state":"open"}`, "other number": `{"number":2,"html_url":"https://github.com/a/b/issues/2"}`} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		c := &Client{API: srv.URL, Token: token, HTTP: srv.Client()}
		if _, err := c.GetIssue(context.Background(), "a/b", 1); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := c.ListComments(context.Background(), "a/b", 1, 10); err == nil && (name == "empty" || name == "null") {
			t.Errorf("%s: a blank comment list counted as no comments", name)
		}
		srv.Close()
	}
}

func TestALabelRemovalThatAnsweredNotFoundIsCheckedAgainstTheIssue(t *testing.T) { // F7
	serve := func(issue string, status int) *Client {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(issue))
		}))
		t.Cleanup(srv.Close)
		return &Client{API: srv.URL, Token: token, HTTP: srv.Client()}
	}
	ctx := context.Background()
	const absent = `{"number":1,"html_url":"https://github.com/a/b/issues/1","labels":[]}`
	const present = `{"number":1,"html_url":"https://github.com/a/b/issues/1","labels":[{"name":"tracked"}]}`
	if err := serve(absent, 200).RemoveLabel(ctx, "a/b", 1, "tracked"); err != nil {
		t.Errorf("an absent label on a live issue is fine: %v", err)
	}
	if err := serve(present, 200).RemoveLabel(ctx, "a/b", 1, "tracked"); err == nil {
		t.Error("a 404 for a label the issue still carries was taken as success")
	}
	if err := serve(`{}`, 404).RemoveLabel(ctx, "a/b", 1, "tracked"); err == nil {
		t.Error("a 404 on a vanished issue was taken as success")
	}
}

// ---- fold 5 (LOCAL P1/P2, QA P2 at 59c86479) ----

func TestACleanupThatFailedIsNotReportedAsRemoved(t *testing.T) {
	f := newFake(t)
	f.Login = "-"
	is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	f.FailNext["DELETE /repos/acme/app/issues/comments/1001"] = true
	cfg := commentCfg()
	cfg.CommentAuthor = "wrong-bot"
	a := newAdapter(t, f, cfg)
	_, r := call(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1)))
	if r == nil || !strings.Contains(r.Msg, "could not be removed") || !strings.Contains(r.Msg, "1001") || strings.Contains(r.Msg, "it was removed") {
		t.Fatalf("the failed cleanup must be said, with the comment to delete by hand: %v", r)
	}
	if len(is.Comments) != 1 {
		t.Fatalf("the comment is still there, got %d", len(is.Comments))
	}
}

func TestCheckWithoutAClientNeverPrintsTheTokenInEitherDetail(t *testing.T) {
	a := &Adapter{Host: "github.com", Secret: "ghp_SYNTH3TIC", CredentialAsked: true,
		ClientError: "bad endpoint ghp_SYNTH3TIC", ConfigError: "cannot read the config /x/ghp_SYNTH3TIC.json"}
	out, err := a.Handle(context.Background(), "check", strings.NewReader(`{"contractVersion":1}`))
	b, _ := json.Marshal(out)
	if err != nil || strings.Contains(string(b), "SYNTH3TIC") {
		t.Fatalf("%v %s", err, b)
	}
}
