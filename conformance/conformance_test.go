package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hadron-memory/hadron-task-adapter-github/internal/fakegithub"
)

// The suite runs the BUILT adapter the way the hadron-cli runner does:
// `<adapter> <call>`, no shell, one JSON object on stdin carrying
// contractVersion, the answer one JSON object on stdout with exit 0.

const token = "ghp_FAKEconformanceTOKEN0000000000000000"

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "adapter-conformance-")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "hadron-task-adapter-github")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "../cmd/hadron-task-adapter-github")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building the adapter failed:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t      *testing.T
	gh     *fakegithub.Server
	config string
	vars   []string
}

func newEnv(t *testing.T, cfg map[string]any) *env {
	t.Helper()
	gh := fakegithub.New(token)
	t.Cleanup(gh.Close)
	cfg["apiUrl"] = gh.URL
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, gh: gh, config: p, vars: []string{"GITHUB_TOKEN=" + token}}
}

type result struct {
	code           int
	stdout, stderr []byte
}

// call runs one adapter call with the runner's request shape.
func (e *env) call(name string, req map[string]any) result {
	e.t.Helper()
	body := map[string]any{"contractVersion": ContractVersion}
	for k, v := range req {
		body[k] = v
	}
	stdin, _ := json.Marshal(body)
	return e.raw([]string{name}, stdin)
}

func (e *env) raw(args []string, stdin []byte) result {
	e.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + e.t.TempDir(), "TASK_ADAPTER_GITHUB_CONFIG=" + e.config}, e.vars...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return result{code: code, stdout: out.Bytes(), stderr: errb.Bytes()}
}

func (e *env) ok(name string, req map[string]any, out any) {
	e.t.Helper()
	r := e.call(name, req)
	if r.code != 0 {
		e.t.Fatalf("%s exited %d: %s", name, r.code, r.stderr)
	}
	if err := OneObject(r.stdout, out); err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
	e.noToken(r)
}

func (e *env) noToken(r result) {
	e.t.Helper()
	if bytes.Contains(r.stdout, []byte(token)) || bytes.Contains(r.stderr, []byte(token)) {
		e.t.Fatalf("the GitHub credential was printed:\nstdout %s\nstderr %s", r.stdout, r.stderr)
	}
}

// walk pages list-untracked as the runner's Walk does, applying its checks:
// a page bound, no repeated cursor, and an item seen once is not offered
// again. between runs after each page, before the next is asked for.
func (e *env) walk(forWorker string, limit int, between func([]Item)) []Item {
	e.t.Helper()
	seen := map[string]bool{"": true}
	got := []Item{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages >= MaxPages {
			e.t.Fatalf("the list did not end within %d pages", MaxPages)
		}
		req := map[string]any{"limit": limit}
		if cursor != "" {
			req["cursor"] = cursor
		}
		if forWorker != "" {
			req["for"] = forWorker
		}
		var p Page
		e.ok("list-untracked", req, &p)
		if err := CheckPage(p); err != nil {
			e.t.Fatal(err)
		}
		if len(p.Items) > limit {
			e.t.Fatalf("asked for %d, got %d", limit, len(p.Items))
		}
		got = append(got, p.Items...)
		if between != nil {
			between(p.Items)
		}
		if p.NextCursor == nil {
			return got
		}
		if seen[*p.NextCursor] {
			e.t.Fatalf("the adapter repeated a cursor: %q", *p.NextCursor)
		}
		seen[*p.NextCursor] = true
		cursor = *p.NextCursor
	}
}

func ids(items []Item) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.ExternalID)
	}
	return out
}

func day(n int) time.Time { return time.Date(2026, 11, n, 12, 0, 0, 0, time.UTC) }

func TestDescribeConforms(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}})
	var d Describe
	e.ok("describe", nil, &d)
	if err := CheckDescribe(d); err != nil {
		t.Fatal(err)
	}
}

// describe answers even with no configuration and no credential: the runner
// asks it first, to check the contract version, before anything else.
func TestDescribeNeedsNoConfigOrCredential(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}})
	e.config = filepath.Join(t.TempDir(), "missing.json")
	e.vars = nil
	var d Describe
	e.ok("describe", nil, &d)
	if err := CheckDescribe(d); err != nil {
		t.Fatal(err)
	}
}

// The walk lists every eligible open issue of every repository exactly once,
// oldest first across repositories (then repository, then number), and
// leaves out excluded, not-ready and already-marked issues.
func TestWalkListsEveryEligibleIssueOnceOldestFirst(t *testing.T) {
	e := newEnv(t, map[string]any{
		"repositories":  []string{"acme/app", "Acme/Lib"},
		"readyLabel":    "cleared",
		"excludeLabels": []string{"epic"},
	})
	e.gh.AddRepo("acme/app",
		fakegithub.Issue{Number: 1, Title: "app one", CreatedAt: day(1), Labels: []string{"Cleared"}},
		fakegithub.Issue{Number: 2, Title: "not ready", CreatedAt: day(2)},
		fakegithub.Issue{Number: 3, Title: "an epic", CreatedAt: day(3), Labels: []string{"cleared", "EPIC"}},
		fakegithub.Issue{Number: 4, Title: "already marked", CreatedAt: day(4), Labels: []string{"cleared", "hadron-tracked"}},
		fakegithub.Issue{Number: 5, Title: "closed", CreatedAt: day(5), State: "CLOSED", Labels: []string{"cleared"}},
		fakegithub.Issue{Number: 6, Title: "app six", CreatedAt: day(8), Labels: []string{"cleared"}},
	)
	e.gh.AddRepo("Acme/Lib",
		fakegithub.Issue{Number: 9, Title: "lib nine", CreatedAt: day(2), Labels: []string{"cleared"}},
		fakegithub.Issue{Number: 7, Title: "lib seven", CreatedAt: day(6), Labels: []string{"cleared"}},
	)
	for _, limit := range []int{1, 2, 100} {
		got := ids(e.walk("", limit, nil))
		want := []string{
			e.gh.IssueURL("acme/app", 1), e.gh.IssueURL("Acme/Lib", 9), e.gh.IssueURL("Acme/Lib", 7), e.gh.IssueURL("acme/app", 6),
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("limit %d:\n got %v\nwant %v", limit, got, want)
		}
	}
	// The external id is GitHub's own canonical URL, in GitHub's spelling.
	if got := e.gh.IssueURL("Acme/Lib", 9); got != "https://github.com/Acme/Lib/issues/9" {
		t.Fatalf("fixture: %s", got)
	}
}

// The set shrinks while others claim: items marked tracked between pages
// must not make the walk skip anything that is still untracked.
func TestMarksBetweenPagesSkipNothing(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}})
	issues := []fakegithub.Issue{}
	for i := 1; i <= 7; i++ {
		issues = append(issues, fakegithub.Issue{Number: i, Title: fmt.Sprint("t", i), CreatedAt: day(i)})
	}
	e.gh.AddRepo("acme/app", issues...)
	got := e.walk("", 2, func(page []Item) {
		for _, it := range page {
			var ans map[string]any
			e.ok("mark-tracked", map[string]any{"externalId": it.ExternalID, "task": map[string]any{"id": "t-" + it.ExternalID}, "worker": map[string]any{"name": "W"}}, &ans)
		}
	})
	if len(got) != 7 {
		t.Fatalf("a walk with marks between pages listed %d of 7: %v", len(got), ids(got))
	}
	if left := e.walk("", 100, nil); len(left) != 0 {
		t.Fatalf("marked items are listed again: %v", ids(left))
	}
}

func TestMarkThenUntrackRoundTrip(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}, "trackedLabel": "in-hadron"})
	e.gh.AddRepo("acme/app", fakegithub.Issue{Number: 4, Title: "x", CreatedAt: day(1), Labels: []string{"bug"}})
	id := e.gh.IssueURL("acme/app", 4)
	var ans map[string]any
	e.ok("mark-tracked", map[string]any{"externalId": id,
		"task":   map[string]any{"id": "01abc", "handle": "AT1.2", "portalUrl": "https://hadronmemory.com/app/u/x"},
		"worker": map[string]any{"id": "w1", "name": "Jonas", "role": "cli-engineer", "model": "m"}}, &ans)
	if l := e.gh.Labels("acme/app", 4); strings.Join(l, ",") != "bug,in-hadron" {
		t.Fatalf("labels after mark: %v", l)
	}
	if len(e.gh.Comments) != 0 {
		t.Fatalf("a comment was posted without comment: true in the config: %v", e.gh.Comments)
	}
	// A second mark (a re-mark after a reassignment) is not refused: the
	// mark never decides a claim.
	e.ok("mark-tracked", map[string]any{"externalId": id, "task": map[string]any{"id": "01abc"}, "worker": map[string]any{"name": "Other"}}, &ans)
	if got := e.walk("", 20, nil); len(got) != 0 {
		t.Fatalf("a marked item is listed: %v", ids(got))
	}
	e.ok("untrack", map[string]any{"externalId": id}, &ans)
	if l := e.gh.Labels("acme/app", 4); strings.Join(l, ",") != "bug" {
		t.Fatalf("labels after untrack: %v", l)
	}
	// untrack of an item that carries no mark is not an error.
	e.ok("untrack", map[string]any{"externalId": id}, &ans)
	if got := ids(e.walk("", 20, nil)); len(got) != 1 || got[0] != id {
		t.Fatalf("an untracked item is listed again: %v", got)
	}
}

func TestMarkCommentIsOptInAndCarriesTheServersURL(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}, "comment": true})
	e.gh.AddRepo("acme/app", fakegithub.Issue{Number: 4, Title: "x", CreatedAt: day(1)})
	var ans map[string]any
	e.ok("mark-tracked", map[string]any{"externalId": e.gh.IssueURL("acme/app", 4),
		"task":   map[string]any{"id": "01abc", "handle": "AT1.2", "portalUrl": "https://hadronmemory.com/app/u/hrn:agenttask:x"},
		"worker": map[string]any{"name": "Jonas\n@everyone", "role": "cli-engineer"}}, &ans)
	if len(e.gh.Comments) != 1 {
		t.Fatalf("comments: %v", e.gh.Comments)
	}
	c := e.gh.Comments[0].Body
	if !strings.Contains(c, "https://hadronmemory.com/app/u/hrn:agenttask:x") || !strings.Contains(c, "AT1.2") {
		t.Fatalf("comment does not name the task as the server printed it: %q", c)
	}
	// Text from Hadron is quoted as code, on one line, so a name cannot
	// mention anyone or break the comment's layout.
	if !strings.Contains(c, "`Jonas @everyone`") || strings.Contains(c, "Jonas\n") {
		t.Fatalf("worker text reached the comment unneutralised: %q", c)
	}
}

// The label is the mark; the comment is decoration. A comment that fails
// after the label landed must not turn the call into a refusal: the runner
// would read the issue as unmarked while it no longer lists.
func TestMarkCommentFailureAfterTheLabelStillAnswersMarked(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}, "comment": true})
	e.gh.AddRepo("acme/app", fakegithub.Issue{Number: 4, Title: "x", CreatedAt: day(1)})
	e.gh.FailComments(403)
	e.gh.EchoInErrors(token)
	var ans map[string]any
	e.ok("mark-tracked", map[string]any{"externalId": e.gh.IssueURL("acme/app", 4),
		"task": map[string]any{"id": "01abc"}, "worker": map[string]any{"name": "W"}}, &ans)
	if ans["marked"] != true || ans["commented"] != false {
		t.Fatalf("answer after a failed comment: %v", ans)
	}
	if msg, _ := ans["commentError"].(string); msg == "" || strings.Contains(msg, token) {
		t.Fatalf("commentError missing or unscrubbed: %v", ans)
	}
	if l := e.gh.Labels("acme/app", 4); strings.Join(l, ",") != "hadron-tracked" {
		t.Fatalf("labels after mark: %v", l)
	}
}

func TestGetAndNormalize(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}})
	e.gh.AddRepo("Acme/App", fakegithub.Issue{Number: 12, Title: "the title", Body: "the body", CreatedAt: day(1), Labels: []string{"hadron-tracked"}})
	id := e.gh.IssueURL("acme/app", 12)
	var got struct {
		Item struct {
			ExternalID, Title, Body, State, URL string
			Tracked                             bool
		}
	}
	e.ok("get", map[string]any{"externalId": id}, &got)
	if got.Item.ExternalID != id || got.Item.Title != "the title" || got.Item.Body != "the body" || got.Item.State != "open" || got.Item.URL != id || !got.Item.Tracked {
		t.Fatalf("get: %+v", got.Item)
	}
	for _, ref := range []string{"acme/app#12", "ACME/APP#12", "https://github.com/acme/app/issues/12/", "#12"} {
		var n struct{ ExternalID string }
		e.ok("normalize", map[string]any{"ref": ref}, &n)
		if n.ExternalID != id {
			t.Fatalf("normalize %q = %q, want GitHub's canonical %q", ref, n.ExternalID, id)
		}
	}
}

func TestForSelectsTheMappedTeam(t *testing.T) {
	e := newEnv(t, map[string]any{
		"repositories": []string{"acme/app"},
		"teams": []map[string]any{
			{"for": []string{"Ada", "hrn:worker:example.com:team:ada"}, "label": "team-ada"},
			{"for": []string{"Xan"}, "label": "team-xan"},
			{"for": []string{"Bo"}, "noTeam": true},
		},
	})
	e.gh.AddRepo("acme/app",
		fakegithub.Issue{Number: 1, Title: "a", CreatedAt: day(1), Labels: []string{"team-ada"}},
		fakegithub.Issue{Number: 2, Title: "x", CreatedAt: day(2), Labels: []string{"Team-Xan"}},
		fakegithub.Issue{Number: 3, Title: "none", CreatedAt: day(3), Labels: []string{"bug"}},
	)
	cases := map[string][]string{
		"Ada":                             {e.gh.IssueURL("acme/app", 1)},
		"ada":                             {e.gh.IssueURL("acme/app", 1)},
		"hrn:worker:example.com:team:ada": {e.gh.IssueURL("acme/app", 1)},
		"Xan":                             {e.gh.IssueURL("acme/app", 2)},
		"Bo":                              {e.gh.IssueURL("acme/app", 3)},
		"":                                {e.gh.IssueURL("acme/app", 1), e.gh.IssueURL("acme/app", 2), e.gh.IssueURL("acme/app", 3)},
	}
	for who, want := range cases {
		if got := ids(e.walk(who, 20, nil)); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("for %q: got %v want %v", who, got, want)
		}
	}
	r := e.call("list-untracked", map[string]any{"limit": 20, "for": "Nobody"})
	if r.code == 0 {
		t.Fatalf("an unmapped --for listed instead of refusing: %s", r.stdout)
	}
}

// Labels past the first page of 100 are still read: they decide the filters.
func TestLabelsPastOnePageDecideTheFilters(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}, "excludeLabels": []string{"wontfix"}})
	many := []string{}
	for i := 0; i < 130; i++ {
		many = append(many, fmt.Sprint("l", i))
	}
	e.gh.AddRepo("acme/app",
		fakegithub.Issue{Number: 1, Title: "excluded late", CreatedAt: day(1), Labels: append(append([]string{}, many...), "wontfix")},
		fakegithub.Issue{Number: 2, Title: "fine", CreatedAt: day(2), Labels: many},
	)
	if got := ids(e.walk("", 20, nil)); len(got) != 1 || got[0] != e.gh.IssueURL("acme/app", 2) {
		t.Fatalf("got %v", got)
	}
}

// A tracker failure refuses the call: never a partial list presented as whole.
func TestTrackerFailureRefusesTheCall(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app", "acme/lib"}})
	e.gh.AddRepo("acme/app", fakegithub.Issue{Number: 1, Title: "a", CreatedAt: day(1)})
	e.gh.AddRepo("acme/lib", fakegithub.Issue{Number: 1, Title: "b", CreatedAt: day(2)})
	e.gh.Fail("acme/lib", 502)
	e.gh.EchoInErrors(token)
	r := e.call("list-untracked", map[string]any{"limit": 20})
	if r.code == 0 {
		t.Fatalf("a failing repository still produced an answer: %s", r.stdout)
	}
	if len(bytes.TrimSpace(r.stdout)) != 0 {
		t.Fatalf("a refused call wrote stdout: %s", r.stdout)
	}
	e.noToken(r)
}

func TestCredentialIsTheTeamsNeverHadrons(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}})
	e.gh.AddRepo("acme/app", fakegithub.Issue{Number: 1, Title: "a", CreatedAt: day(1)})
	e.vars = []string{"HADRON_TOKEN=" + token}
	r := e.call("list-untracked", map[string]any{"limit": 20})
	if r.code == 0 {
		t.Fatalf("listed with no GitHub credential (only a Hadron one): %s", r.stdout)
	}
	if n := len(e.gh.Requests); n != 0 {
		t.Fatalf("GitHub was called without the team's credential: %v", e.gh.Requests)
	}
	e.noToken(r)
	// A credential named by tokenEnv is used.
	e2 := newEnv(t, map[string]any{"repositories": []string{"acme/app"}, "tokenEnv": "ACME_GH"})
	e2.gh.AddRepo("acme/app", fakegithub.Issue{Number: 1, Title: "a", CreatedAt: day(1)})
	e2.vars = []string{"ACME_GH=" + token}
	if got := e2.walk("", 20, nil); len(got) != 1 {
		t.Fatalf("tokenEnv credential: %v", ids(got))
	}
}

func TestRequestDisciplineIsRefused(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}})
	// Every repository and issue the cases name exists and is readable, so
	// each refusal is the adapter's own, not GitHub's.
	e.gh.AddRepo("acme/app", fakegithub.Issue{Number: 1, Title: "a", CreatedAt: day(1)})
	e.gh.AddRepo("other/repo", fakegithub.Issue{Number: 1, Title: "b", CreatedAt: day(1)})
	var sane Page
	e.ok("list-untracked", map[string]any{"limit": 20}, &sane)
	var got map[string]any
	e.ok("get", map[string]any{"externalId": e.gh.IssueURL("acme/app", 1)}, &got)
	cases := map[string]struct {
		args  []string
		stdin string
	}{
		"unknown call":      {[]string{"list-everything"}, `{"contractVersion":1}`},
		"no call":           {nil, `{"contractVersion":1}`},
		"two arguments":     {[]string{"list-untracked", "--all"}, `{"contractVersion":1}`},
		"other version":     {[]string{"list-untracked"}, `{"contractVersion":2}`},
		"no version":        {[]string{"list-untracked"}, `{}`},
		"not json":          {[]string{"list-untracked"}, `hello`},
		"two documents":     {[]string{"list-untracked"}, `{"contractVersion":1}{"contractVersion":1}`},
		"limit out of rng":  {[]string{"list-untracked"}, `{"contractVersion":1,"limit":101}`},
		"foreign repo mark": {[]string{"mark-tracked"}, `{"contractVersion":1,"externalId":"https://github.com/other/repo/issues/1","task":{"id":"t"},"worker":{}}`},
		"not an issue url":  {[]string{"get"}, `{"contractVersion":1,"externalId":"https://github.com/acme/app/pull/1"}`},
		"forged cursor":     {[]string{"list-untracked"}, `{"contractVersion":1,"cursor":"v1.bm90IGEgY3Vyc29y"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := e.raw(c.args, []byte(c.stdin))
			if r.code == 0 {
				t.Fatalf("accepted: %s", r.stdout)
			}
			if len(bytes.TrimSpace(r.stdout)) != 0 {
				t.Fatalf("a refusal wrote stdout: %s", r.stdout)
			}
			if len(r.stderr) == 0 {
				t.Fatal("a refusal gave no reason on stderr")
			}
		})
	}
}

// A cursor carries its query: one issued for another --for, or before the
// configuration changed, is refused rather than resumed somewhere wrong.
func TestCursorIsBoundToItsQuery(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}, "teams": []map[string]any{{"for": []string{"Ada"}, "label": "a"}, {"for": []string{"Xan"}, "label": "x"}}})
	for i := 1; i <= 3; i++ {
		e.gh.AddRepo("acme/app", fakegithub.Issue{Number: i, Title: "t", CreatedAt: day(i), Labels: []string{"a", "x"}})
	}
	var p Page
	e.ok("list-untracked", map[string]any{"limit": 1, "for": "Ada"}, &p)
	if p.NextCursor == nil {
		t.Fatal("no cursor")
	}
	if r := e.call("list-untracked", map[string]any{"limit": 1, "for": "Xan", "cursor": *p.NextCursor}); r.code == 0 {
		t.Fatalf("a cursor for Ada was resumed for Xan: %s", r.stdout)
	}
}

// The cursor stays inside the runner's bound with the most repositories the
// configuration allows, every one of them part-way through, with GitHub
// positions longer than any seen (their length is not promised).
func TestCursorFitsTheRunnersBoundAtMaxRepositories(t *testing.T) {
	repos := []string{}
	for i := 0; i < 20; i++ {
		repos = append(repos, fmt.Sprintf("a-rather-long-organisation-name/a-rather-long-repository-name-%02d", i))
	}
	e := newEnv(t, map[string]any{"repositories": repos})
	e.gh.PadCursors(129)
	for i, r := range repos {
		e.gh.AddRepo(r,
			fakegithub.Issue{Number: 100000 + i, Title: "t", CreatedAt: day(1).Add(time.Duration(i) * time.Minute)},
			fakegithub.Issue{Number: 200000 + i, Title: "t", CreatedAt: day(20)},
		)
	}
	var p Page
	e.ok("list-untracked", map[string]any{"limit": 20}, &p)
	if p.NextCursor == nil || len(p.Items) != 20 {
		t.Fatalf("items %d cursor %v", len(p.Items), p.NextCursor)
	}
	if err := CheckPage(p); err != nil {
		t.Fatal(err)
	}
	if got := len(e.walk("", 100, nil)); got != 40 {
		t.Fatalf("walk: %d of 40", got)
	}
}

// Positions too long to fit refuse the call: a page is never answered with
// a cursor the runner would refuse.
func TestACursorThatCannotFitIsARefusalNotAnAnswer(t *testing.T) {
	repos := []string{}
	for i := 0; i < 20; i++ {
		repos = append(repos, fmt.Sprintf("acme/r%02d", i))
	}
	e := newEnv(t, map[string]any{"repositories": repos})
	e.gh.PadCursors(400)
	for i, r := range repos {
		e.gh.AddRepo(r,
			fakegithub.Issue{Number: 1, Title: "t", CreatedAt: day(1).Add(time.Duration(i) * time.Minute)},
			fakegithub.Issue{Number: 2, Title: "t", CreatedAt: day(20)},
		)
	}
	r := e.call("list-untracked", map[string]any{"limit": 20})
	if r.code == 0 {
		var p Page
		if err := OneObject(r.stdout, &p); err == nil && CheckPage(p) == nil {
			return // fitted after all: fine
		}
		t.Fatalf("answered with a cursor the runner refuses: %d bytes", len(r.stdout))
	}
	if len(bytes.TrimSpace(r.stdout)) != 0 {
		t.Fatalf("a refusal wrote stdout")
	}
}

// Text GitHub returns is never repeated with the credential in it: not
// only GitHub's error messages, but data the adapter refuses.
func TestARefusedIssueURLDoesNotEchoTheCredential(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}})
	e.gh.AddRepo("acme/app", fakegithub.Issue{Number: 1, Title: "a", CreatedAt: day(1)})
	e.gh.SuffixURLs("?echo=" + token)
	r := e.call("list-untracked", map[string]any{"limit": 20})
	if r.code == 0 {
		t.Fatalf("a malformed issue URL was listed: %s", r.stdout)
	}
	e.noToken(r)
}

// A request with anything after its one object is refused before any write.
func TestTrailingTextAfterTheRequestIsRefusedBeforeAnyWrite(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}})
	e.gh.AddRepo("acme/app", fakegithub.Issue{Number: 4, Title: "x", CreatedAt: day(1)})
	req := `{"contractVersion":1,"externalId":"` + e.gh.IssueURL("acme/app", 4) + `","task":{"id":"t"},"worker":{}}`
	for _, suffix := range []string{"]", "}", " x", "{}", "null"} {
		r := e.raw([]string{"mark-tracked"}, []byte(req+suffix))
		if r.code == 0 {
			t.Fatalf("suffix %q accepted: %s", suffix, r.stdout)
		}
		if l := e.gh.Labels("acme/app", 4); len(l) != 0 {
			t.Fatalf("suffix %q: the mark was written before the refusal: %v", suffix, l)
		}
	}
}

func TestCheckReports(t *testing.T) {
	e := newEnv(t, map[string]any{"repositories": []string{"acme/app"}})
	e.gh.AddRepo("acme/app")
	type report struct {
		OK     bool
		Checks []struct {
			Name   string
			OK     bool
			Detail string
		}
	}
	var c report
	e.ok("check", nil, &c)
	if c.OK {
		t.Fatalf("check passed with the tracked label not defined in the repository: %+v", c)
	}
	e.gh.DefineLabel("acme/app", "Hadron-Tracked")
	c = report{}
	e.ok("check", nil, &c)
	if !c.OK {
		t.Fatalf("check failed on a sound setup: %+v", c)
	}
	e2 := newEnv(t, map[string]any{"repositories": []string{"acme/app", "acme/gone"}})
	e2.gh.AddRepo("acme/app")
	e2.gh.DefineLabel("acme/app", "hadron-tracked")
	c = report{}
	e2.ok("check", nil, &c)
	if c.OK {
		t.Fatalf("check passed with an unreadable repository: %+v", c)
	}
	e.vars = nil
	e.ok("check", nil, &c)
	if c.OK || len(c.Checks) == 0 || c.Checks[0].OK {
		t.Fatalf("check passed with no credential: %+v", c)
	}
}

// Nothing team-specific and no credential is committed to this public
// repository: no configuration file but the example, and nothing shaped like
// a GitHub token except this suite's own labelled fakes.
func TestRepositoryCarriesNoCredentialOrTeamConfig(t *testing.T) {
	out, err := exec.Command("git", "-C", "..", "ls-files").Output()
	if err != nil {
		t.Skip("not a git checkout:", err)
	}
	tokenShape := regexp.MustCompile(`(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{20,}`)
	for _, f := range strings.Fields(string(out)) {
		base := filepath.Base(f)
		if strings.HasSuffix(base, ".json") && base != "config.example.json" {
			t.Errorf("%s: a JSON file is committed; adapter configuration stays with the team", f)
		}
		b, err := os.ReadFile(filepath.Join("..", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tokenShape.FindAllString(string(b), -1) {
			if !strings.Contains(m, "FAKE") {
				t.Errorf("%s: something shaped like a GitHub token is committed", f)
			}
		}
	}
}
