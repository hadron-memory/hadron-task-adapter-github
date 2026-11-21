package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

const token = "ghp_SENTINEL_TOKEN_123"

func baseConfig() *Config {
	c := &Config{Repositories: []string{"acme/app", "acme/lib"}}
	_ = c.normalize()
	return c
}

func newAdapter(t *testing.T, f *fakeGitHub, c *Config) *Adapter {
	t.Helper()
	return &Adapter{Config: c, Client: f.client(token), Host: "github.com"}
}

// call runs one call and returns the answer as a map, or the *Refusal.
func call(t *testing.T, a *Adapter, name, req string) (map[string]any, *Refusal) {
	t.Helper()
	req = withVersion(req)
	out, err := a.Handle(context.Background(), name, strings.NewReader(req))
	if err != nil {
		var r *Refusal
		if !errors.As(err, &r) {
			t.Fatalf("%s: not a refusal: %v", name, err)
		}
		if strings.Contains(r.Msg, token) {
			t.Fatalf("%s: the refusal message carries the credential: %q", name, r.Msg)
		}
		return nil, r
	}
	b, _ := json.Marshal(out)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m, nil
}

// withVersion adds "contractVersion":1 to a JSON object request that lacks it
// (the CLI puts it in every request); anything else is passed through untouched.
func withVersion(req string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(req), &m) != nil || m == nil {
		return req
	}
	if _, ok := m["contractVersion"]; !ok {
		m["contractVersion"] = json.RawMessage("1")
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func mustCall(t *testing.T, a *Adapter, name, req string) map[string]any {
	t.Helper()
	m, r := call(t, a, name, req)
	if r != nil {
		t.Fatalf("%s refused (%d): %s", name, r.Code, r.Msg)
	}
	return m
}

func ids(page map[string]any) []string {
	var out []string
	for _, it := range page["items"].([]any) {
		out = append(out, it.(map[string]any)["externalId"].(string))
	}
	return out
}

func issueURL(repo string, n int) string {
	return fmt.Sprintf("https://github.com/%s/issues/%d", repo, n)
}

func TestDescribeNeedsNoConfigOrCredential(t *testing.T) {
	a := &Adapter{}
	m := mustCall(t, a, "describe", `{"contractVersion":1}`)
	if m["name"] != AdapterName || m["contractVersion"].(float64) != 1 || m["order"] != "oldest-first" {
		t.Fatalf("describe: %v", m)
	}
	calls := fmt.Sprint(m["calls"])
	for _, c := range []string{"list-untracked", "mark-tracked", "untrack", "get", "report", "changed-since", "normalize", "check"} {
		if !strings.Contains(calls, c) {
			t.Fatalf("describe lacks %s: %v", c, m["calls"])
		}
	}
}

func TestRequestMustBeOneObjectOfThisContractVersion(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, baseConfig())
	for name, req := range map[string]string{"empty": ``, "not json": `nope`, "array": `[]`, "other version": `{"contractVersion":2}`} {
		if _, r := call(t, a, "list-untracked", req); r == nil || r.Code != ExitUsage {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if _, r := call(t, a, "frobnicate", `{"contractVersion":1}`); r == nil || r.Code != ExitUsage {
		t.Errorf("unknown call: %+v", r)
	}
}

func TestListIsOldestFirstAcrossRepositoriesAndExcludesPullRequests(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 5, Title: "five", CreatedAt: "2026-01-03T00:00:00Z"})
	f.add("acme/lib", &fakeIssue{Number: 1, Title: "lib one", CreatedAt: "2026-01-01T00:00:00Z"})
	f.add("acme/app", &fakeIssue{Number: 2, Title: "two", CreatedAt: "2026-01-02T00:00:00Z"})
	f.add("acme/app", &fakeIssue{Number: 9, Title: "a pull request", CreatedAt: "2026-01-01T12:00:00Z", PR: true})
	f.add("acme/app", &fakeIssue{Number: 7, Title: "closed", CreatedAt: "2026-01-01T00:30:00Z", State: "CLOSED"})
	got := ids(mustCall(t, newAdapter(t, f, baseConfig()), "list-untracked", `{"contractVersion":1,"limit":100}`))
	want := []string{issueURL("acme/lib", 1), issueURL("acme/app", 2), issueURL("acme/app", 5)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestListTiesBreakByRepositoryThenNumber(t *testing.T) {
	f := newFake(t)
	same := "2026-01-01T00:00:00Z"
	f.add("acme/lib", &fakeIssue{Number: 1, Title: "l1", CreatedAt: same})
	f.add("acme/app", &fakeIssue{Number: 3, Title: "a3", CreatedAt: same})
	f.add("acme/app", &fakeIssue{Number: 2, Title: "a2", CreatedAt: same})
	got := ids(mustCall(t, newAdapter(t, f, baseConfig()), "list-untracked", `{"limit":100}`))
	want := []string{issueURL("acme/app", 2), issueURL("acme/app", 3), issueURL("acme/lib", 1)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestLabelModel(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "plain", CreatedAt: "2026-01-01T00:00:00Z"})
	f.add("acme/app", &fakeIssue{Number: 2, Title: "excluded", CreatedAt: "2026-01-02T00:00:00Z", Labels: []string{"Wontfix"}})
	f.add("acme/app", &fakeIssue{Number: 3, Title: "alpha ready", CreatedAt: "2026-01-03T00:00:00Z", Labels: []string{"team:alpha", "READY"}})
	f.add("acme/app", &fakeIssue{Number: 4, Title: "beta", CreatedAt: "2026-01-04T00:00:00Z", Labels: []string{"team:beta"}})
	cfg := baseConfig()
	cfg.ExcludeLabels = []string{"wontfix"}
	cfg.ReadyLabel = "ready"
	cfg.Teams = []Team{{Label: "Team:Alpha", Lead: "Iris"}, {Label: "team:beta", Lead: "Rufus"}}
	a := newAdapter(t, f, cfg)
	cases := map[string]struct {
		req  string
		want []int
	}{
		"no filter, exclude applies": {`{"limit":100}`, []int{1, 3, 4}},
		"for the alpha lead":         {`{"limit":100,"for":"iris"}`, []int{3}},
		"for the beta lead":          {`{"limit":100,"for":"Rufus"}`, []int{4}},
		"an empty for is no team":    {`{"limit":100,"for":""}`, []int{1, 3, 4}},
	}
	for name, tc := range cases {
		var want []string
		for _, n := range tc.want {
			want = append(want, issueURL("acme/app", n))
		}
		if got := ids(mustCall(t, a, "list-untracked", tc.req)); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	if _, r := call(t, a, "list-untracked", `{"for":"Nobody"}`); r == nil || r.Code != ExitUsage {
		t.Errorf("a for that leads no team must be refused: %+v", r)
	}
	cfg.RequireReady = true
	if got := ids(mustCall(t, a, "list-untracked", `{"limit":100}`)); fmt.Sprint(got) != fmt.Sprint([]string{issueURL("acme/app", 3)}) {
		t.Errorf("requireReady: %v", got)
	}
}

func TestPagingIsByOpaqueCursorAndSurvivesTheSetShrinking(t *testing.T) {
	f := newFake(t)
	for i := 1; i <= 7; i++ {
		f.add("acme/app", &fakeIssue{Number: i, Title: fmt.Sprint(i), CreatedAt: fmt.Sprintf("2026-01-%02dT00:00:00Z", i)})
	}
	a := newAdapter(t, f, baseConfig())
	p1 := mustCall(t, a, "list-untracked", `{"limit":3}`)
	if got := ids(p1); len(got) != 3 || got[0] != issueURL("acme/app", 1) {
		t.Fatalf("page 1: %v", got)
	}
	cur, _ := p1["nextCursor"].(string)
	if !strings.HasPrefix(cur, "v1.") {
		t.Fatalf("cursor: %v", p1["nextCursor"])
	}
	// The set shrinks between pages: the first item closes, and the CLI claims
	// the last item of page 1 (it leaves the list). Page 2 must neither repeat
	// nor skip: it is exactly the three after the cursor.
	f.find("acme/app", 1).State = "CLOSED"
	f.find("acme/app", 3).State = "CLOSED"
	p2 := mustCall(t, a, "list-untracked", fmt.Sprintf(`{"limit":3,"cursor":%q}`, cur))
	want := []string{issueURL("acme/app", 4), issueURL("acme/app", 5), issueURL("acme/app", 6)}
	if got := ids(p2); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("page 2: got %v want %v", got, want)
	}
	cur2 := p2["nextCursor"].(string)
	p3 := mustCall(t, a, "list-untracked", fmt.Sprintf(`{"limit":3,"cursor":%q}`, cur2))
	if got := ids(p3); fmt.Sprint(got) != fmt.Sprint([]string{issueURL("acme/app", 7)}) {
		t.Fatalf("page 3: %v", got)
	}
	if p3["nextCursor"] != nil {
		t.Fatalf("the last page has no cursor: %v", p3["nextCursor"])
	}
	// An exactly-full last page ends too.
	f2 := newFake(t)
	for i := 1; i <= 2; i++ {
		f2.add("acme/app", &fakeIssue{Number: i, Title: "t", CreatedAt: fmt.Sprintf("2026-01-%02dT00:00:00Z", i)})
	}
	if m := mustCall(t, newAdapter(t, f2, baseConfig()), "list-untracked", `{"limit":2}`); m["nextCursor"] != nil {
		t.Fatalf("a list that ends on the page boundary has no cursor: %v", m["nextCursor"])
	}
}

func TestBadLimitsAndCursorsAreUsageErrors(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, baseConfig())
	for _, req := range []string{`{"limit":0}`, `{"limit":101}`, `{"limit":-1}`, `{"cursor":"7"}`, `{"cursor":"v1.!!!"}`, `{"cursor":"v1.e30"}`} {
		if _, r := call(t, a, "list-untracked", req); r == nil || r.Code != ExitUsage {
			t.Errorf("%s: %+v", req, r)
		}
	}
}

func TestLargeRepositoryPagesThroughGitHub(t *testing.T) {
	f := newFake(t)
	for i := 1; i <= 230; i++ {
		f.add("acme/app", &fakeIssue{Number: i, Title: "t", CreatedAt: time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339)})
	}
	got := ids(mustCall(t, newAdapter(t, f, baseConfig()), "list-untracked", `{"limit":100}`))
	if len(got) != 100 || got[0] != issueURL("acme/app", 1) {
		t.Fatalf("%d items, first %v", len(got), got[:1])
	}
}

func TestReadRefusalsNeverCarryTheCredentialOrAPartialList(t *testing.T) {
	t.Run("past the page bound", func(t *testing.T) {
		f := newFake(t)
		for i := 1; i <= 150; i++ {
			f.add("acme/app", &fakeIssue{Number: i, Title: "t", CreatedAt: time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339)})
		}
		cfg := baseConfig()
		cfg.MaxPagesPerRepository = 1
		if _, r := call(t, newAdapter(t, f, cfg), "list-untracked", `{}`); r == nil || r.Code != ExitRefused || !strings.Contains(r.Msg, "maxPagesPerRepository") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("an issue with too many labels", func(t *testing.T) {
		f := newFake(t)
		f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", ManyLabels: true})
		if _, r := call(t, newAdapter(t, f, baseConfig()), "list-untracked", `{}`); r == nil || r.Code != ExitRefused {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("an unknown repository", func(t *testing.T) {
		f := newFake(t)
		f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
		cfg := baseConfig()
		cfg.Repositories = []string{"acme/app", "acme/ghost"}
		if _, r := call(t, newAdapter(t, f, cfg), "list-untracked", `{}`); r == nil || r.Code != ExitRefused || !strings.Contains(r.Msg, "acme/ghost") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a server error that echoes the credential", func(t *testing.T) {
		f := newFake(t)
		f.Status, f.Token = 500, token
		if _, r := call(t, newAdapter(t, f, baseConfig()), "list-untracked", `{}`); r == nil || r.Code != ExitRefused || !strings.Contains(r.Msg, "500") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a rate limit", func(t *testing.T) {
		f := newFake(t)
		f.RateLimited = true
		if _, r := call(t, newAdapter(t, f, baseConfig()), "list-untracked", `{}`); r == nil || !strings.Contains(r.Msg, "rate limit") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("no credential", func(t *testing.T) {
		f := newFake(t)
		a := &Adapter{Config: baseConfig(), Client: f.client("")}
		if _, r := call(t, a, "list-untracked", `{}`); r == nil || !strings.Contains(r.Msg, "GITHUB_TOKEN") {
			t.Fatalf("%+v", r)
		}
		if len(f.Requests) != 0 {
			t.Fatalf("no request may be made without a credential: %v", f.Requests)
		}
	})
}

func TestTitlesAreCleanedAndTheIDIsTheURLAsGitHubGaveIt(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "\x1b[2Jevil‮title\nnext", CreatedAt: "2026-01-01T00:00:00Z"})
	m := mustCall(t, newAdapter(t, f, baseConfig()), "list-untracked", `{}`)
	it := m["items"].([]any)[0].(map[string]any)
	if it["externalId"] != issueURL("acme/app", 1) {
		t.Fatalf("%v", it)
	}
	if title := it["title"].(string); strings.ContainsAny(title, "\x1b‮\n") {
		t.Fatalf("title not cleaned: %q", title)
	}
}

func markRequest(id string) string {
	return fmt.Sprintf(`{"contractVersion":1,"externalId":%q,"task":{"id":"t-1","handle":"AT1.2","portalUrl":"https://hadronmemory.com/app/t/1"},"worker":{"id":"w1","name":"Iris","role":"cli-engineer","model":"x"}}`, id)
}

func TestMarkTrackedDefaultConfigWritesNothing(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	mustCall(t, newAdapter(t, f, baseConfig()), "mark-tracked", markRequest(issueURL("acme/app", 1)))
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a default config must not write to the tracker: %v", w)
	}
}

func TestMarkTrackedLabelAndOneUpdatedComment(t *testing.T) {
	f := newFake(t)
	is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", Comments: []Comment{{ID: 1, Body: "a human comment"}}})
	cfg := baseConfig()
	cfg.TrackedLabel, cfg.Comment = "hadron:tracked", true
	a := newAdapter(t, f, cfg)
	mustCall(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1)))
	if fmt.Sprint(is.Labels) != "[hadron:tracked]" {
		t.Fatalf("labels %v", is.Labels)
	}
	if len(is.Comments) != 2 || !strings.HasPrefix(is.Comments[1].Body, commentMarker+"t-1 -->") || !strings.Contains(is.Comments[1].Body, "Iris (cli-engineer)") || !strings.Contains(is.Comments[1].Body, "[AT1.2](https://hadronmemory.com/app/t/1)") {
		t.Fatalf("comments %+v", is.Comments)
	}
	// A re-mark (a reassignment) updates the same comment, never adds one.
	req := strings.ReplaceAll(markRequest(issueURL("acme/app", 1)), "Iris", "Rufus")
	mustCall(t, a, "mark-tracked", req)
	if len(is.Comments) != 2 || !strings.Contains(is.Comments[1].Body, "Rufus") || is.Comments[0].Body != "a human comment" {
		t.Fatalf("comments %+v", is.Comments)
	}
}

func TestMarkTrackedRefusals(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	a := newAdapter(t, f, baseConfig())
	for name, req := range map[string]string{
		"a trailing space":   markRequest(issueURL("acme/app", 1) + " "),
		"a pull request URL": markRequest("https://github.com/acme/app/pull/1"),
		"another host":       markRequest("https://example.com/acme/app/issues/1"),
		"userinfo":           markRequest("https://u:p@github.com/acme/app/issues/1"),
		"an unlisted repo":   markRequest(issueURL("acme/other", 1)),
		"no task":            `{"externalId":"` + issueURL("acme/app", 1) + `"}`,
		"not a number":       markRequest("https://github.com/acme/app/issues/x"),
		"http":               markRequest("http://github.com/acme/app/issues/1"),
	} {
		if _, r := call(t, a, "mark-tracked", req); r == nil || r.Code != ExitUsage {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a refused mark wrote: %v", w)
	}
}

func TestUntrackRemovesTheLabelAndReleasesTheComment(t *testing.T) {
	f := newFake(t)
	is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	cfg := baseConfig()
	cfg.TrackedLabel, cfg.Comment = "hadron:tracked", true
	a := newAdapter(t, f, cfg)
	mustCall(t, a, "mark-tracked", markRequest(issueURL("acme/app", 1)))
	mustCall(t, a, "untrack", fmt.Sprintf(`{"externalId":%q}`, issueURL("acme/app", 1)))
	if len(is.Labels) != 0 || !strings.Contains(is.Comments[0].Body, "No longer tracked") || !strings.HasPrefix(is.Comments[0].Body, commentMarker) {
		t.Fatalf("labels %v comments %+v", is.Labels, is.Comments)
	}
	// Untracking again is quiet: the label is already gone.
	mustCall(t, a, "untrack", fmt.Sprintf(`{"externalId":%q}`, issueURL("acme/app", 1)))
}

func TestReportAppliesTheConfiguredActionAndIgnoresOtherStates(t *testing.T) {
	f := newFake(t)
	is := f.add("acme/app", &fakeIssue{Number: 1, Title: "t", CreatedAt: "2026-01-01T00:00:00Z", Labels: []string{"in-progress"}})
	cfg := baseConfig()
	cfg.Report = map[string]ReportAction{"FINISHED": {AddLabels: []string{"done"}, RemoveLabels: []string{"in-progress", "never-had"}}}
	a := newAdapter(t, f, cfg)
	mustCall(t, a, "report", fmt.Sprintf(`{"externalId":%q,"task":{"id":"t"},"state":"STARTED"}`, issueURL("acme/app", 1)))
	if len(f.writes()) != 0 {
		t.Fatalf("an unconfigured state must not write: %v", f.writes())
	}
	mustCall(t, a, "report", fmt.Sprintf(`{"externalId":%q,"task":{"id":"t"},"state":"finished"}`, issueURL("acme/app", 1)))
	if fmt.Sprint(is.Labels) != "[done]" {
		t.Fatalf("labels %v", is.Labels)
	}
}

func TestGet(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "T", Body: "body text", State: "CLOSED", CreatedAt: "2026-01-01T00:00:00Z"})
	f.add("acme/app", &fakeIssue{Number: 2, Title: "big", Body: strings.Repeat("é", maxBodyRunes+5), CreatedAt: "2026-01-01T00:00:00Z"})
	f.add("acme/app", &fakeIssue{Number: 3, Title: "pr", PR: true, CreatedAt: "2026-01-01T00:00:00Z"})
	a := newAdapter(t, f, baseConfig())
	m := mustCall(t, a, "get", fmt.Sprintf(`{"externalId":%q}`, issueURL("acme/app", 1)))
	if m["title"] != "T" || m["body"] != "body text" || m["state"] != "closed" || m["url"] != issueURL("acme/app", 1) || m["externalId"] != issueURL("acme/app", 1) || m["bodyTruncated"] != false {
		t.Fatalf("%v", m)
	}
	big := mustCall(t, a, "get", fmt.Sprintf(`{"externalId":%q}`, issueURL("acme/app", 2)))
	if big["bodyTruncated"] != true || len([]rune(big["body"].(string))) != maxBodyRunes {
		t.Fatalf("truncation: %v", big["bodyTruncated"])
	}
	if _, r := call(t, a, "get", fmt.Sprintf(`{"externalId":%q}`, issueURL("acme/app", 99))); r == nil || r.Code != ExitNotFound {
		t.Fatalf("missing: %+v", r)
	}
	if _, r := call(t, a, "get", fmt.Sprintf(`{"externalId":%q}`, issueURL("acme/app", 3))); r == nil || r.Code != ExitUsage {
		t.Fatalf("pull request: %+v", r)
	}
}

func TestNormalize(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 12, Title: "t", CreatedAt: "2026-01-01T00:00:00Z"})
	cfg := baseConfig()
	cfg.DefaultRepository = "acme/app"
	a := newAdapter(t, f, cfg)
	want := issueURL("acme/app", 12)
	for _, ref := range []string{want, want + "/", "http://github.com/acme/app/issues/12", "https://www.github.com/acme/app/issues/12", want + "?x=1#issuecomment-5", "ACME/App#12", "acme/app#12", "#12", " #12 "} {
		m, r := call(t, a, "normalize", fmt.Sprintf(`{"reference":%q}`, ref))
		if r != nil || m["externalId"] != want {
			t.Errorf("%q: %v %+v", ref, m, r)
		}
	}
	for _, ref := range []string{"", "12", "#0", "#x", "acme/other#1", "https://github.com/acme/app/pull/12", "https://u@github.com/acme/app/issues/12", "https://example.com/acme/app/issues/12"} {
		if _, r := call(t, a, "normalize", fmt.Sprintf(`{"reference":%q}`, ref)); r == nil || r.Code != ExitUsage {
			t.Errorf("%q: %+v", ref, r)
		}
	}
	cfg.DefaultRepository = ""
	if _, r := call(t, a, "normalize", `{"reference":"#12"}`); r == nil || r.Code != ExitUsage {
		t.Errorf("bare #N without a default repository: %+v", r)
	}
}

func TestCheck(t *testing.T) {
	f := newFake(t)
	f.Token = token
	m := mustCall(t, newAdapter(t, f, baseConfig()), "check", `{}`)
	if m["credentialPresent"] != true || m["credentialValid"] != true || m["reachable"] != true {
		t.Fatalf("%v", m)
	}
	bad := &Adapter{Config: baseConfig(), Client: f.client("ghp_WRONG")}
	m = mustCall(t, bad, "check", `{}`)
	if m["credentialPresent"] != true || m["credentialValid"] != false || m["reachable"] != true {
		t.Fatalf("%v", m)
	}
	none := &Adapter{Config: baseConfig(), Client: f.client("")}
	m = mustCall(t, none, "check", `{}`)
	if m["credentialPresent"] != false || m["credentialValid"] != false {
		t.Fatalf("%v", m)
	}
	for _, v := range m {
		if s, ok := v.(string); ok && strings.Contains(s, token) {
			t.Fatalf("the check carries the credential: %v", m)
		}
	}
}

func TestChangedSince(t *testing.T) {
	f := newFake(t)
	f.add("acme/app", &fakeIssue{Number: 1, Title: "old", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"})
	f.add("acme/app", &fakeIssue{Number: 2, Title: "updated late", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-09T00:00:00Z"})
	f.add("acme/lib", &fakeIssue{Number: 3, Title: "closed", State: "CLOSED", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-05T00:00:00Z"})
	f.add("acme/app", &fakeIssue{Number: 4, Title: "newest", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-10T00:00:00Z"})
	a := newAdapter(t, f, baseConfig())
	p1 := mustCall(t, a, "changed-since", `{"since":"2026-01-02T00:00:00Z","limit":2}`)
	if got := ids(p1); fmt.Sprint(got) != fmt.Sprint([]string{issueURL("acme/lib", 3), issueURL("acme/app", 2)}) {
		t.Fatalf("page 1: %v", got)
	}
	if first := p1["items"].([]any)[0].(map[string]any); first["state"] != "closed" || first["updatedAt"] != "2026-01-05T00:00:00Z" {
		t.Fatalf("%v", first)
	}
	cur := p1["nextCursor"].(string)
	p2 := mustCall(t, a, "changed-since", fmt.Sprintf(`{"since":"2026-01-02T00:00:00Z","limit":2,"cursor":%q}`, cur))
	if got := ids(p2); fmt.Sprint(got) != fmt.Sprint([]string{issueURL("acme/app", 4)}) || p2["nextCursor"] != nil {
		t.Fatalf("page 2: %v %v", got, p2["nextCursor"])
	}
	for _, req := range []string{`{}`, `{"since":"yesterday"}`, `{"since":"2026-01-02T00:00:00Z","limit":0}`} {
		if _, r := call(t, a, "changed-since", req); r == nil || r.Code != ExitUsage {
			t.Errorf("%s: %+v", req, r)
		}
	}
}

// roundTripFunc lets a test make the transport fail with text that carries the
// credential, as a proxy error or a URL-bearing error can.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportErrorTextNeverCarriesTheCredential(t *testing.T) {
	c := &Client{API: "https://api.example.test", GraphQL: "https://api.example.test/graphql", Token: token, HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("proxy rejected Bearer " + token)
	})}}
	a := &Adapter{Config: baseConfig(), Client: c}
	for _, call := range []string{"list-untracked", "get"} {
		req := `{"externalId":"` + issueURL("acme/app", 1) + `"}`
		_, r := callWithoutTokenCheck(t, a, call, req)
		if r == nil || !strings.Contains(r.Msg, "unreachable") || strings.Contains(r.Msg, token) {
			t.Fatalf("%s: %+v", call, r)
		}
	}
	m := mustCall(t, a, "check", `{}`)
	if strings.Contains(fmt.Sprint(m), token) || m["reachable"] != false {
		t.Fatalf("check: %v", m)
	}
}

// callWithoutTokenCheck is call without the helper's own credential assertion,
// so the test can show the refusal text itself.
func callWithoutTokenCheck(t *testing.T, a *Adapter, name, req string) (any, *Refusal) {
	t.Helper()
	out, err := a.Handle(context.Background(), name, strings.NewReader(withVersion(req)))
	var r *Refusal
	if err != nil && !errors.As(err, &r) {
		t.Fatal(err)
	}
	return out, r
}
