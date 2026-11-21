// Package conformance runs the built adapter the way hadron-cli's intake
// runner does and checks its answers with the rules hadron-cli publishes as
// pkg/intakecontract, pinned in go.mod. It is its own module so that the
// adapter itself needs no dependency and an older Go than the CLI's.
package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hadron-memory/hadron-cli/pkg/intakecontract"
)

// A fake tracker: 230 open issues in one repository, 100 per GraphQL page.
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		start := 0
		if a, ok := req.Variables["after"].(string); ok {
			start, _ = strconv.Atoi(a)
		}
		const total = 230
		end := min(start+100, total)
		var nodes []map[string]any
		for i := start + 1; i <= end; i++ {
			nodes = append(nodes, map[string]any{
				"number": i, "title": "issue " + strconv.Itoa(i), "state": "OPEN",
				"url":       "https://github.com/acme/app/issues/" + strconv.Itoa(i),
				"createdAt": fmt.Sprintf("2026-01-01T00:%02d:%02dZ", i/60, i%60), "updatedAt": "2026-02-01T00:00:00Z",
				"labels": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"issues": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": end < total, "endCursor": strconv.Itoa(end)}, "nodes": nodes}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func buildAdapter(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hadron-task-adapter-github")
	// Built from the adapter's own module (the parent directory).
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

type harness struct {
	bin string
	env []string
}

func newHarness(t *testing.T) *harness {
	srv := fakeGitHub(t)
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"repositories":["acme/app"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return &harness{bin: buildAdapter(t), env: []string{
		"GITHUB_ADAPTER_CONFIG=" + cfg, "GITHUB_TOKEN=ghp_conformance", "GITHUB_API_URL=" + srv.URL, "GITHUB_GRAPHQL_URL=" + srv.URL + "/graphql",
		"PATH=" + os.Getenv("PATH"), // the CLI removes every HADRON_* variable: none is given
	}}
}

// run calls the adapter as `<exe> <call>` with one JSON request on stdin.
func (h *harness) run(t *testing.T, call string, req map[string]any) ([]byte, int) {
	t.Helper()
	req["contractVersion"] = intakecontract.ContractVersion
	body, _ := json.Marshal(req)
	cmd := exec.Command(h.bin, call)
	cmd.Env = h.env
	cmd.Stdin = bytes.NewReader(body)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), code
}

func TestDescribeIsAcceptedByTheRunnersRules(t *testing.T) {
	h := newHarness(t)
	out, code := h.run(t, "describe", map[string]any{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var d intakecontract.Describe
	if err := intakecontract.DecodeAnswer(out, &d); err != nil {
		t.Fatal(err)
	}
	if err := intakecontract.CheckDescribe(d); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"list-untracked", "mark-tracked"} { // what find-untracked --track needs
		if !d.Has(c) {
			t.Errorf("describe does not list %s", c)
		}
	}
}

func TestEveryListPageIsAcceptedAndThePagesAreAnOpaqueCursorChain(t *testing.T) {
	h := newHarness(t)
	seenCursor := map[string]bool{"": true}
	seenItem := map[string]bool{}
	cursor, pages, items := "", 0, 0
	for {
		req := map[string]any{"limit": 100}
		if cursor != "" {
			req["cursor"] = cursor
		}
		out, code := h.run(t, "list-untracked", req)
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		var page intakecontract.ListAnswer
		if err := intakecontract.DecodeAnswer(out, &page); err != nil {
			t.Fatal(err)
		}
		if err := intakecontract.CheckPage(page); err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			if seenItem[it.ExternalID] {
				t.Fatalf("a repeated item %s", it.ExternalID)
			}
			seenItem[it.ExternalID] = true
			items++
		}
		pages++
		if pages > intakecontract.MaxPages {
			t.Fatal("more pages than the runner walks")
		}
		if page.NextCursor == nil {
			break
		}
		if seenCursor[*page.NextCursor] {
			t.Fatalf("a repeated cursor %q", *page.NextCursor)
		}
		seenCursor[*page.NextCursor] = true
		cursor = *page.NextCursor
	}
	if items != 230 || pages != 3 {
		t.Fatalf("%d items in %d pages, want 230 in 3", items, pages)
	}
}

func TestMarkTrackedAnswersOneObject(t *testing.T) {
	h := newHarness(t)
	out, code := h.run(t, "mark-tracked", map[string]any{
		"externalId": "https://github.com/acme/app/issues/1",
		"task":       map[string]any{"id": "t1", "handle": "AT1.2", "portalUrl": "https://hadronmemory.com/app/t/1"},
		"worker":     map[string]any{"id": "w1", "name": "Iris", "role": "cli-engineer", "model": "m"},
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var ignored map[string]json.RawMessage
	if err := intakecontract.DecodeAnswer(out, &ignored); err != nil {
		t.Fatal(err)
	}
}

func TestARefusalIsANonZeroExitWithOneLineOnStderr(t *testing.T) {
	h := newHarness(t)
	out, code := h.run(t, "list-untracked", map[string]any{"for": "nobody"})
	if code == 0 || len(out) != 0 {
		t.Fatalf("exit %d stdout %q: a refusal exits non-zero and prints nothing on stdout", code, out)
	}
}
