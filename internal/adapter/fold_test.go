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
		login        string
		valid, reach bool
	}{
		"ok":                                 {0, "", true, true},
		"404 and the account is readable":    {404, "", true, true}, // GHES with rate limiting off: judged by GET /user
		"404 and the credential has no user": {404, "-", false, true},
		"500":                                {500, "", false, true},
		"401":                                {401, "", false, true},
		"403":                                {403, "", false, true},
	} {
		f := newFake(t)
		if tc.login != "" {
			f.Login = tc.login
		}
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

// ---- fold 6 (blind round 4: F1 P1, F3-F5) ----

func TestAConfiguredIdentityThatIsNotTheCredentialsEditsAndReleasesNothing(t *testing.T) { // F1
	f := newFake(t)
	is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	other := Comment{ID: 7, Body: trackedComment(taskIn{ID: "t-1"}, workerIn{Name: "Iris"})}
	other.User.Login = "someone-else"
	is.Comments = []Comment{other}
	cfg := &Config{Repositories: []string{"acme/app"}, Comment: true, CommentAuthor: "someone-else"}
	_ = cfg.normalize()
	a := newAdapter(t, f, cfg)
	before := is.Comments[0].Body
	if _, r := call(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1))); r == nil {
		t.Error("mark-tracked edited another account's comment")
	}
	if _, r := call(t, a, "untrack", `{"externalId":"`+issueURL("acme/app", 1)+`"}`); r == nil {
		t.Error("untrack released another account's comment")
	}
	if is.Comments[0].Body != before || len(f.Writes) != 0 {
		t.Fatalf("something was written: %v %q", f.Writes, is.Comments[0].Body)
	}
}

func TestGetAndChangedSinceRefuseAnIssueOutsideTheConfiguredScope(t *testing.T) { // F3
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z"})
	f.Moved = map[string]string{"acme/app": "elsewhere/app"}
	a := newAdapter(t, f, &Config{Repositories: []string{"acme/app"}})
	_ = a.Config.normalize()
	if _, r := call(t, a, "get", `{"externalId":"`+issueURL("acme/app", 1)+`"}`); r == nil {
		t.Error("get returned an issue outside the configured scope")
	}
	if _, r := call(t, a, "changed-since", `{"since":"2025-01-01T00:00:00Z"}`); r == nil {
		t.Error("changed-since returned an issue outside the configured scope")
	}
}

func TestAnIncompleteGraphQLAnswerIsNotAnEmptyResult(t *testing.T) { // F4
	for name, body := range map[string]string{
		"no issues":    `{"data":{"repository":{}}}`,
		"no page info": `{"data":{"repository":{"issues":{"nodes":[]}}}}`,
		"no nodes":     `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false}}}}}`,
		"null nodes":   `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false},"nodes":null}}}}`,
		"no hasNext":   `{"data":{"repository":{"issues":{"pageInfo":{},"nodes":[]}}}}`,
	} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		c := &Client{API: srv.URL, GraphQL: srv.URL, Token: token, HTTP: srv.Client()}
		if got, err := c.ReadIssues(context.Background(), "a/b", []string{"OPEN"}, "", 5); err == nil {
			t.Errorf("%s: accepted as %v", name, got)
		}
		srv.Close()
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}`))
	}))
	defer srv.Close()
	c := &Client{API: srv.URL, GraphQL: srv.URL, Token: token, HTTP: srv.Client()}
	if got, err := c.ReadIssues(context.Background(), "a/b", []string{"OPEN"}, "", 5); err != nil || len(got) != 0 {
		t.Errorf("a complete empty page is fine: %v %v", got, err)
	}
}

func TestAnEndpointWithAQueryOrFragmentIsRefused(t *testing.T) { // F5
	for _, u := range []string{"https://ghe.example.com/api/v3?x=1", "https://ghe.example.com/api/v3#f", "https://ghe.example.com/api/v3?"} {
		if _, err := NewClient(func(k string) string {
			if k == "GITHUB_API_URL" {
				return u
			}
			return ""
		}); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}

// ---- fold 7 (LOCAL/BLIND: GET /user failing must not mean "trust the config") ----

func TestAnIdentityLookupThatFailsTouchesNothing(t *testing.T) {
	for _, op := range []string{"mark-tracked", "untrack"} {
		f := newFake(t)
		is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
		other := Comment{ID: 7, Body: trackedComment(taskIn{ID: "t-1"}, workerIn{Name: "Iris"})}
		other.User.Login = "someone-else"
		is.Comments = []Comment{other}
		cfg := &Config{Repositories: []string{"acme/app"}, Comment: true, CommentAuthor: "someone-else"}
		_ = cfg.normalize()
		a := newAdapter(t, f, cfg)
		f.FailNext["GET /user"] = true // HTTP 500
		req := markRequest(issueURL("acme/app", 1))
		if op == "untrack" {
			req = `{"externalId":"` + issueURL("acme/app", 1) + `"}`
		}
		if _, r := call(t, a, op, req); r == nil {
			t.Errorf("%s trusted the configured author when GitHub could not say who the credential is", op)
		}
		if len(f.Writes) != 0 || is.Comments[0].Body != other.Body {
			t.Fatalf("%s wrote: %v", op, f.Writes)
		}
	}
}

func TestOnlyGitHubSayingTheCredentialHasNoAccountTrustsTheConfiguredAuthor(t *testing.T) {
	type resp struct {
		status int
		header map[string]string
		body   string
	}
	trust := map[string]resp{
		"404":                {404, nil, `{"message":"Not Found"}`},
		"403 not accessible": {403, nil, `{"message":"Resource not accessible by integration"}`},
	}
	refuse := map[string]resp{
		"401":                      {401, nil, `{"message":"Bad credentials"}`},
		"403 plain":                {403, nil, `{"message":"Forbidden"}`},
		"403 empty":                {403, nil, ``},
		"403 secondary rate limit": {403, map[string]string{"Retry-After": "60", "X-RateLimit-Remaining": "4000"}, `{"message":"You have exceeded a secondary rate limit"}`},
		"403 limit named in text":  {403, nil, `{"message":"API rate limit exceeded"}`},
		"429":                      {429, nil, ``},
		"500":                      {500, nil, ``},
	}
	run := func(r resp) (string, error) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range r.header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(r.status)
			_, _ = w.Write([]byte(r.body))
		}))
		defer srv.Close()
		a := &Adapter{Config: &Config{CommentAuthor: "my-app[bot]"}, Client: &Client{API: srv.URL, Token: token, HTTP: srv.Client()}}
		return a.ownLogin(context.Background())
	}
	for name, r := range trust {
		if got, err := run(r); err != nil || got != "my-app[bot]" {
			t.Errorf("%s: want the configured author, got %q %v", name, got, err)
		}
	}
	for name, r := range refuse {
		if got, err := run(r); err == nil {
			t.Errorf("%s: trusted %q though GitHub did not say the credential has no account", name, got)
		}
	}
}

// ---- fold 8 (blind round 5: F2-F7) ----

func TestIdentityIsResolvedBeforeAnyLabelWrite(t *testing.T) { // F2
	for name, setup := range map[string]func(f *fakeGitHub, cfg *Config){
		"a commentAuthor that is not the credential": func(f *fakeGitHub, cfg *Config) { cfg.CommentAuthor = "someone-else" },
		"an identity lookup that fails":              func(f *fakeGitHub, cfg *Config) { f.FailNext["GET /user"] = true },
	} {
		for _, op := range []string{"mark-tracked", "untrack"} {
			f := newFake(t)
			f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", Labels: []string{"hadron-tracked"}})
			cfg := &Config{Repositories: []string{"acme/app"}, TrackedLabel: "hadron-tracked", Comment: true}
			_ = cfg.normalize()
			setup(f, cfg)
			a := newAdapter(t, f, cfg)
			req := markRequest(issueURL("acme/app", 1))
			if op == "untrack" {
				req = `{"externalId":"` + issueURL("acme/app", 1) + `"}`
			}
			if _, r := call(t, a, op, req); r == nil {
				t.Errorf("%s/%s: accepted", name, op)
			}
			if len(f.Writes) != 0 {
				t.Errorf("%s/%s changed the issue before refusing: %v", name, op, f.Writes)
			}
		}
	}
}

func TestA404OnDeleteIsGoneOnlyIfTheListNoLongerHasTheComment(t *testing.T) { // F3
	serve := func(list string, listStatus int) *Client {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(listStatus)
			_, _ = w.Write([]byte(list))
		}))
		t.Cleanup(srv.Close)
		return &Client{API: srv.URL, Token: token, HTTP: srv.Client()}
	}
	ctx := context.Background()
	if err := serve(`[]`, 200).DeleteComment(ctx, "a/b", 1, 7); err != nil {
		t.Errorf("a comment that is really gone is fine: %v", err)
	}
	if err := serve(`[{"id":7,"body":"x","user":{"login":"u"}}]`, 200).DeleteComment(ctx, "a/b", 1, 7); err == nil {
		t.Error("a 404 for a comment still on the issue was taken as removed")
	}
	if err := serve(`{}`, 404).DeleteComment(ctx, "a/b", 1, 7); err == nil {
		t.Error("a 404 whose absence cannot be confirmed was taken as removed")
	}
}

func TestAnIssueWithoutLabelDataIsRefusedNotTreatedAsUnlabelled(t *testing.T) { // F4
	for name, labels := range map[string]string{
		"no labels":      ``,
		"null labels":    `"labels":null,`,
		"null nodes":     `"labels":{"nodes":null,"pageInfo":{"hasNextPage":false}},`,
		"no page info":   `"labels":{"nodes":[]},`,
		"no hasNextPage": `"labels":{"nodes":[],"pageInfo":{}},`,
	} {
		body := `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false},"nodes":[{` + labels + `"number":1,"title":"t","url":"https://github.com/a/b/issues/1","state":"OPEN","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}]}}}}`
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		c := &Client{API: srv.URL, GraphQL: srv.URL, Token: token, HTTP: srv.Client()}
		if got, err := c.ReadIssues(context.Background(), "a/b", []string{"OPEN"}, "", 5); err == nil {
			t.Errorf("%s: accepted as %v", name, got)
		}
		srv.Close()
	}
}

func TestAWriteLookupOnTheWrongHostIsNotWritten(t *testing.T) { // F5
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"number":1,"title":"t","html_url":"https://evil.example/acme/app/issues/1","state":"open","labels":[]}`))
	}))
	defer srv.Close()
	cfg := &Config{Repositories: []string{"acme/app"}, TrackedLabel: "x"}
	_ = cfg.normalize()
	a := &Adapter{Config: cfg, Host: "github.com", Client: &Client{API: srv.URL, Token: token, HTTP: srv.Client()}}
	if _, _, err := a.writeTarget(context.Background(), issueURL("acme/app", 1)); err == nil {
		t.Fatal("a lookup answered from another host was accepted for a write")
	}
}

func TestCursorsAreCheckedAndBelongToTheirCall(t *testing.T) { // F6
	good := cursorKey{Time: "2026-01-01T00:00:00Z", Repo: "acme/app", Number: 1}
	if _, err := decodeCursor(encodeCursor("list-untracked", good), "list-untracked"); err != nil {
		t.Fatalf("its own cursor: %v", err)
	}
	if _, err := decodeCursor(encodeCursor("list-untracked", good), "changed-since"); err == nil {
		t.Error("a list-untracked cursor was accepted by changed-since")
	}
	for name, k := range map[string]cursorKey{
		"a malformed time": {Time: "yesterday", Repo: "acme/app", Number: 1},
		"a bad repository": {Time: "2026-01-01T00:00:00Z", Repo: "not a repo", Number: 1},
	} {
		if _, err := decodeCursor(encodeCursor("changed-since", k), "changed-since"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestALeadWithSpacesInTheConfigMatchesTheRequest(t *testing.T) { // F7
	c := &Config{Repositories: []string{"acme/app"}, Teams: []Team{{Label: " team-a ", Lead: " Iris "}}}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if l, ok := c.teamLabelFor("Iris"); !ok || l != "team-a" {
		t.Fatalf("%q %v", l, ok)
	}
}

// ---- fold 9 (blind round 7: F1, F2 P2 and cheap P3s) ----

func TestACommentScanThatCannotCompleteRefusesBeforeAnyLabelWrite(t *testing.T) { // F1
	for _, op := range []string{"mark-tracked", "untrack"} {
		f := newFake(t)
		is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", Labels: []string{"hadron-tracked"}})
		for i := 0; i < 1000; i++ {
			is.Comments = append(is.Comments, Comment{ID: int64(i + 1), Body: "chatter"})
		}
		cfg := &Config{Repositories: []string{"acme/app"}, TrackedLabel: "hadron-tracked", Comment: true}
		_ = cfg.normalize()
		a := newAdapter(t, f, cfg)
		req := markRequest(issueURL("acme/app", 1))
		if op == "untrack" {
			req = `{"externalId":"` + issueURL("acme/app", 1) + `"}`
		}
		if _, r := call(t, a, op, req); r == nil {
			t.Errorf("%s: a scan that cannot complete was accepted", op)
		}
		if len(f.Writes) != 0 {
			t.Errorf("%s wrote before refusing: %v", op, f.Writes)
		}
	}
}

func TestALoginFailureIsNotAdvisedAsAMissingCommentAuthor(t *testing.T) { // F6
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	a := newAdapter(t, f, commentCfg())
	f.FailNext["GET /user"] = true
	_, r := call(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1)))
	if r == nil || strings.Contains(r.Msg, "set commentAuthor") || !strings.Contains(r.Msg, "retry") {
		t.Fatalf("an outage must say retry, not 'set commentAuthor': %v", r)
	}
	f2 := newFake(t)
	f2.Login = "-"
	f2.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	a2 := newAdapter(t, f2, commentCfg())
	if _, r := call(t, a2, "mark-tracked", markRequest(issueURL("acme/app", 1))); r == nil || !strings.Contains(r.Msg, "set commentAuthor") {
		t.Fatalf("no account must say set commentAuthor: %v", r)
	}
}

func TestLabelsAreTrimmedAndDotRepositoriesRefused(t *testing.T) { // F15, F16
	c := &Config{Repositories: []string{"a/b"}, ExcludeLabels: []string{" wontfix "}, ReadyLabel: " ready ", TrackedLabel: " tracked "}
	if err := c.normalize(); err != nil || c.ExcludeLabels[0] != "wontfix" || c.ReadyLabel != "ready" || c.TrackedLabel != "tracked" {
		t.Fatalf("%v %+v", err, c)
	}
	for _, r := range []string{"./b", "a/.", "../b", "a/.."} {
		if validRepo(r) {
			t.Errorf("%q accepted", r)
		}
	}
}

// ---- fold 10 (blind delta round: F1 P2 honesty, F3 P2 cause, F4 P2 control) ----

func TestTheCommentIdentityIsAskedOncePerCallSoNoLookupFollowsAWrite(t *testing.T) { // F1
	for _, op := range []string{"mark-tracked", "untrack"} {
		f := newFake(t)
		f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
		cfg := &Config{Repositories: []string{"acme/app"}, TrackedLabel: "hadron-tracked", Comment: true}
		_ = cfg.normalize()
		a := newAdapter(t, f, cfg)
		req := markRequest(issueURL("acme/app", 1))
		if op == "untrack" {
			req = `{"externalId":"` + issueURL("acme/app", 1) + `"}`
		}
		mustCall(t, a, op, req)
		n, firstUser, firstWrite := 0, -1, -1
		for i, r := range f.Requests {
			if r == "GET /user" {
				n++
				if firstUser < 0 {
					firstUser = i
				}
			}
			if firstWrite < 0 && (strings.HasPrefix(r, "POST ") || strings.HasPrefix(r, "PATCH ") || strings.HasPrefix(r, "DELETE ") || strings.HasPrefix(r, "PUT ")) {
				firstWrite = i
			}
		}
		// Once, and BEFORE the first write: one lookup after the label write also
		// counts one, which is exactly the original failure.
		if n != 1 || firstWrite < 0 || firstUser < 0 || firstUser >= firstWrite {
			t.Errorf("%s: %d identity lookups (first at %d), first write at %d, want exactly one, before any write: %v", op, n, firstUser, firstWrite, f.Requests)
		}
	}
}

// (For the label-then-refuse scenario with a failing GET /user, see
// TestIdentityIsResolvedBeforeAnyLabelWrite: label+comment, mark and untrack, zero writes.)
func TestACommentScanJustBelowTheBoundStillMarksAndUntracksWithTheLabel(t *testing.T) { // F4 control
	for _, op := range []string{"mark-tracked", "untrack"} {
		f := newFake(t)
		is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
		for i := 0; i < 999; i++ { // 999 existing: the edge just below the 1,000 bound
			is.Comments = append(is.Comments, Comment{ID: int64(i + 1), Body: "chatter"})
		}
		cfg := &Config{Repositories: []string{"acme/app"}, TrackedLabel: "hadron-tracked", Comment: true}
		_ = cfg.normalize()
		a := newAdapter(t, f, cfg)
		req := markRequest(issueURL("acme/app", 1))
		if op == "untrack" {
			req = `{"externalId":"` + issueURL("acme/app", 1) + `"}`
			is.Labels = []string{"hadron-tracked"}
		}
		mustCall(t, a, op, req)
		if len(f.Writes) == 0 {
			t.Errorf("%s: below the bound it must write", op)
		}
	}
}

func TestCheckSaysWhyWhen404OnRateLimitAndTheUserLookupFailsToo(t *testing.T) { // F3
	f := newFake(t)
	f.Login = "-"
	f.RateLimitStatus = 404
	a := &Adapter{Config: baseConfig(), Client: f.client(token)}
	m := mustCall(t, a, "check", `{}`)
	d, _ := m["detail"].(string)
	if m["credentialValid"] != false || !strings.Contains(d, "/rate_limit") || !strings.Contains(d, "GET /user answered") || !strings.Contains(d, "GITHUB_API_URL") {
		t.Fatalf("%v", m)
	}
}

// ---- ids that hold no tracker item: a #fragment sub-claim, a pull request (AT0.9506) ----

func mustNormalize(t *testing.T, c *Config) *Config {
	t.Helper()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAWriteCallForAnIdWithNoTrackerItemChangesNothingAndSucceedsWithTheEmptyAnswer(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", Labels: []string{"hadron-tracked"}})
	for cfgName, cfg := range map[string]*Config{
		"a write-enabled config": mustNormalize(t, &Config{Repositories: []string{"acme/app"}, TrackedLabel: "hadron-tracked", Comment: true, Report: map[string]ReportAction{"CANCELLED": {AddLabels: []string{"x"}}}}),
		"a read-only config":     mustNormalize(t, &Config{Repositories: []string{"acme/app"}}),
	} {
		a := newAdapter(t, f, cfg)
		for idName, id := range map[string]string{
			"a #fragment sub-claim":             issueURL("acme/app", 1) + "#build",
			"a #fragment sub-claim, any case":   "https://github.com/ACME/App/issues/1#Build",
			"a pull request":                    "https://github.com/acme/app/pull/7",
			"a pull request with a fragment":    "https://github.com/acme/app/pull/7#build",
			"a pull request that may not exist": "https://github.com/acme/app/pull/999999",
		} {
			for name, req := range map[string]string{
				"mark-tracked": markRequest(id),
				"untrack":      `{"externalId":"` + id + `"}`,
				"report":       `{"externalId":"` + id + `","state":"cancelled"}`,
			} {
				m, r := call(t, a, name, req)
				if r != nil {
					t.Errorf("%s / %s of %s was refused: %v", cfgName, name, idName, r)
				} else if len(m) != 0 {
					t.Errorf("%s / %s of %s answered %v, want the empty object", cfgName, name, idName, m)
				}
			}
		}
	}
	if len(f.Writes) != 0 || len(f.Requests) != 0 {
		t.Fatalf("such an id must not touch the tracker at all (no write, no read): writes %v requests %v", f.Writes, f.Requests)
	}
}

// The guard comes after the request's own validation in mark-tracked (a bad
// task or worker is a usage error whatever the id), and report keeps its
// "an unknown state does nothing" answer.
func TestTheNoTrackerItemGuardKeepsTheCallsOwnInputChecks(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	a := newAdapter(t, f, mustNormalize(t, &Config{Repositories: []string{"acme/app"}, TrackedLabel: "x"}))
	id := issueURL("acme/app", 1) + "#build"
	if _, r := call(t, a, "mark-tracked", `{"externalId":"`+id+`","task":{"id":""}}`); r == nil || r.Code != ExitUsage {
		t.Errorf("a missing task id must be a usage error even for a sub-claim: %v", r)
	}
	if _, r := call(t, a, "mark-tracked", `{"externalId":"`+id+`","task":{"id":"t-1"},"worker":"not an object"}`); r == nil || r.Code != ExitUsage {
		t.Errorf("a malformed worker must be a usage error even for a sub-claim: %v", r)
	}
	if _, r := call(t, a, "report", `{"externalId":"https://github.com/acme/app/pull/3","state":"no-such-state"}`); r != nil {
		t.Errorf("an unknown state on a PR id does nothing, as for any id: %v", r)
	}
}

// The adapter writes only to a bare canonical issue URL in a listed repository
// (what it issues). Every other id the CLI's reconcile hands it, including the
// ids of other systems, is a no-op with no request at all: a refusal would hold
// the CLI's reconcile cursor. The ids below that are not GitHub issues at all
// are real ones from the live team data.
func TestAnyIdThatIsNotABareListedIssueIsANoOpForEveryWriteCall(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", Labels: []string{"hadron-tracked"}})
	cfg := mustNormalize(t, &Config{Repositories: []string{"acme/app"}, TrackedLabel: "hadron-tracked", Comment: true, Report: map[string]ReportAction{"CANCELLED": {AddLabels: []string{"x"}}}})
	a := newAdapter(t, f, cfg)
	for name, id := range map[string]string{
		"an id of another system (CI run)":          "andi:4192:ci:aff802b39",
		"an id of another system (ci-)":             "ci-4156-90b2a849",
		"an id of another system (fit:)":            "fit:task-handles-s6",
		"an issue in a repository the config lacks": "https://github.com/hadron-memory/hadron-client/issues/24",
		"an issue in an unlisted repository":        issueURL("acme/other", 1),
		"an issue on another host":                  "https://example.com/acme/app/issues/1",
		"an http issue URL":                         "http://github.com/acme/app/issues/1",
		"a pull request in an unlisted repository":  "https://github.com/acme/other/pull/1",
		"a pull request on another host":            "https://example.com/acme/app/pull/1",
		"a pull request number that is not one":     "https://github.com/acme/app/pull/x",
		"pull/0":                                    "https://github.com/acme/app/pull/0",
		"a leading-zero number":                     "https://github.com/acme/app/pull/07",
		"a signed number":                           "https://github.com/acme/app/pull/+7",
		"a trailing slash":                          "https://github.com/acme/app/pull/7/",
		"a path below the pull request":             "https://github.com/acme/app/pull/7/files",
		"a query":                                   "https://github.com/acme/app/pull/7?x=1",
		"an empty fragment":                         "https://github.com/acme/app/pull/7#",
		"two fragments":                             "https://github.com/acme/app/pull/7#a#b",
		"an encoded path":                           "https://github.com/acme/app/pull/%37",
		"a fragment on an unlisted repository":      issueURL("acme/other", 1) + "#build",
		"a fragment on another host":                "https://example.com/acme/app/issues/1#build",
		"a path that is neither":                    "https://github.com/acme/app/discussions/1",
		"an issue with a query":                     issueURL("acme/app", 1) + "?x=1",
		"an issue with a trailing slash":            issueURL("acme/app", 1) + "/",
		"an issue with an empty fragment":           issueURL("acme/app", 1) + "#",
		"an issue with two fragments":               issueURL("acme/app", 1) + "#a#b",
		"an issue with a leading-zero number":       "https://github.com/acme/app/issues/01",
	} {
		for call2, req := range map[string]string{
			"mark-tracked": markRequest(id),
			"untrack":      `{"externalId":"` + id + `"}`,
			"report":       `{"externalId":"` + id + `","state":"cancelled"}`,
		} {
			if m, r := call(t, a, call2, req); r != nil || len(m) != 0 {
				t.Errorf("%s / %s: want the empty success, got %v %v", name, call2, m, r)
			}
		}
	}
	if len(f.Writes) != 0 || len(f.Requests) != 0 {
		t.Fatalf("no id of that kind may reach the tracker: writes %v requests %v", f.Writes, f.Requests)
	}
}

// What is still refused is an id the platform's own rules reject, which no task
// can carry (whitespace, control characters, userinfo, over-long).
func TestAnIdThatFailsThePlatformIDRulesIsStillRefusedByEveryWriteCall(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	a := newAdapter(t, f, baseConfig())
	for name, id := range map[string]string{
		"a trailing space": issueURL("acme/app", 1) + " ",
		"a control char":   "ci-1\u0007x",
		"userinfo":         "https://u:p@github.com/acme/app/issues/1",
		"a fragment space": issueURL("acme/app", 1) + "#a b",
		"over-long":        "ci-" + strings.Repeat("x", 600),
	} {
		for call2, req := range map[string]string{
			"mark-tracked": markRequest(id),
			"untrack":      `{"externalId":` + strconv.Quote(id) + `}`,
			"report":       `{"externalId":` + strconv.Quote(id) + `,"state":"cancelled"}`,
		} {
			if _, r := call(t, a, call2, req); r == nil {
				t.Errorf("%s / %s: accepted", name, call2)
			}
		}
	}
}

func TestABareIssueURLStillBehavesAsBeforeForEveryWriteCall(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", Labels: []string{"hadron-tracked"}})
	cfg := mustNormalize(t, &Config{Repositories: []string{"acme/app"}, TrackedLabel: "hadron-tracked", Report: map[string]ReportAction{"CANCELLED": {AddLabels: []string{"cancelled"}}}})
	a := newAdapter(t, f, cfg)
	id := issueURL("acme/app", 1)
	mustCall(t, a, "mark-tracked", markRequest(id))
	n1 := len(f.Writes)
	mustCall(t, a, "untrack", `{"externalId":"`+id+`"}`)
	n2 := len(f.Writes)
	mustCall(t, a, "report", `{"externalId":"`+id+`","state":"cancelled"}`)
	if n1 == 0 || n2 <= n1 || len(f.Writes) <= n2 {
		t.Fatalf("mark-tracked, untrack and report of a bare issue URL must each write: %v", f.Writes)
	}
}

func TestAWriteCallRecognisesAHostCaseWwwAndALowercasedRepositoryAndOnlyTheConfiguredHost(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", Labels: []string{"hadron-tracked"}})
	a := newAdapter(t, f, mustNormalize(t, &Config{Repositories: []string{"acme/app"}, TrackedLabel: "hadron-tracked"}))
	for _, id := range []string{"https://www.github.com/ACME/App/issues/1", "https://GitHub.com/acme/app/issues/1"} {
		before := len(f.Writes)
		mustCall(t, a, "untrack", `{"externalId":"`+id+`"}`)
		if len(f.Writes) <= before {
			t.Errorf("%s is the bare listed issue and must be written", id)
		}
	}
	// With another host configured, a github.com URL is not this adapter's item: no request.
	a.Host = "ghe.example.com"
	before, reqs := len(f.Writes), len(f.Requests)
	mustCall(t, a, "untrack", `{"externalId":"https://github.com/acme/app/issues/1"}`)
	if len(f.Writes) != before || len(f.Requests) != reqs {
		t.Errorf("a github.com id is not the configured host's item, so nothing may be sent")
	}
}
