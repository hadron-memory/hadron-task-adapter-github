package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCall runs the adapter the way the CLI does and returns exit, stdout, stderr.
func runCall(t *testing.T, env map[string]string, args []string, stdin string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	in, _ := os.Create(filepath.Join(dir, "in"))
	_, _ = io.WriteString(in, stdin)
	_, _ = in.Seek(0, 0)
	out, _ := os.Create(filepath.Join(dir, "out"))
	errf, _ := os.Create(filepath.Join(dir, "err"))
	code := run(args, in, out, errf, func(k string) string { return env[k] })
	rd := func(f *os.File) string { b, _ := os.ReadFile(f.Name()); return string(b) }
	return code, rd(out), rd(errf)
}

func TestDescribeAnswersWithOneJSONObjectAndNoConfig(t *testing.T) {
	code, out, errText := runCall(t, nil, []string{"describe"}, `{"contractVersion":1}`)
	if code != 0 || errText != "" || !strings.Contains(out, `"contractVersion":1`) || strings.Count(out, "\n") != 1 {
		t.Fatalf("%d %q %q", code, out, errText)
	}
}

func TestUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{nil, {"a", "b"}} {
		if code, _, errText := runCall(t, nil, args, ``); code != 2 || !strings.Contains(errText, "usage:") {
			t.Errorf("%v: %d %q", args, code, errText)
		}
	}
	cfg := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(cfg, []byte(`{"repositories":["a/b"]}`), 0o600)
	if code, _, _ := runCall(t, map[string]string{"GITHUB_ADAPTER_CONFIG": cfg, "GITHUB_TOKEN": "ghp_testtoken"}, []string{"frobnicate"}, `{}`); code != 2 {
		t.Errorf("an unknown call must exit 2, got %d", code)
	}
}

func TestMissingOrBadConfigRefusesAndNamesThePath(t *testing.T) {
	code, out, errText := runCall(t, map[string]string{"GITHUB_ADAPTER_CONFIG": "/nonexistent/c.json"}, []string{"list-untracked"}, `{}`)
	if code != 1 || out != "" || !strings.Contains(errText, "/nonexistent/c.json") {
		t.Fatalf("%d %q %q", code, out, errText)
	}
}

func TestNoCredentialIsAnInstructiveRefusal(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(cfg, []byte(`{"repositories":["a/b"]}`), 0o600)
	code, out, errText := runCall(t, map[string]string{"GITHUB_ADAPTER_CONFIG": cfg}, []string{"list-untracked"}, `{"contractVersion":1}`)
	if code != 1 || out != "" || !strings.Contains(errText, "GITHUB_TOKEN") {
		t.Fatalf("%d %q %q", code, out, errText)
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{"": "", "https://github.com": "github.com", "https://ghe.example.com/": "ghe.example.com", "http://ghe.example.com": "ghe.example.com"} {
		if got := hostOf(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}

func TestCheckRunsWithAMissingConfigAndExitsZero(t *testing.T) {
	// No config file and no reachable GitHub: check still answers one JSON object
	// and says why there is no config; the other calls refuse.
	env := map[string]string{"GITHUB_ADAPTER_CONFIG": "/nonexistent/c.json", "GITHUB_API_URL": "https://127.0.0.1:1", "GITHUB_TOKEN": "ghp_testtoken"}
	code, out, errText := runCall(t, env, []string{"check"}, `{"contractVersion":1}`)
	if code != 0 || errText != "" || !strings.Contains(out, `"configValid":false`) || !strings.Contains(out, "/nonexistent/c.json") || !strings.Contains(out, `"reachable":false`) {
		t.Fatalf("%d %q %q", code, out, errText)
	}
	if code, _, _ := runCall(t, env, []string{"list-untracked"}, `{"contractVersion":1}`); code != 1 {
		t.Errorf("list-untracked without a config must refuse, got %d", code)
	}
}

func TestAPlainHTTPEndpointIsRefusedBeforeAnyRequest(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(cfg, []byte(`{"repositories":["a/b"]}`), 0o600)
	env := map[string]string{"GITHUB_ADAPTER_CONFIG": cfg, "GITHUB_TOKEN": "ghp_testtoken", "GITHUB_API_URL": "http://ghe.example.com/api/v3"}
	code, out, errText := runCall(t, env, []string{"list-untracked"}, `{"contractVersion":1}`)
	if code != 1 || out != "" || !strings.Contains(errText, "GITHUB_API_URL") || !strings.Contains(errText, "https") {
		t.Fatalf("%d %q %q", code, out, errText)
	}
}

func TestOneLineOnStderrWhateverTheUpstreamSaid(t *testing.T) {
	got := oneLine("first\nsecond\x1b[31m‮end", 1000)
	if strings.ContainsAny(got, "\n\x1b‮") || got != "first second [31m end" {
		t.Fatalf("%q", got)
	}
	if n := len(oneLine(strings.Repeat("é", 2000), 1000)); n > 1000 {
		t.Fatalf("not capped: %d", n)
	}
}

// The credential never reaches stderr, even from an error built before any
// request: a malformed endpoint that contains the token, a token that cannot be
// sent in a header (Go would quote it into its own error).
func TestTheCredentialNeverReachesStderr(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(cfg, []byte(`{"repositories":["a/b"]}`), 0o600)
	const tok = "ghp_SYNTH3TIC"
	for name, env := range map[string]map[string]string{
		"a malformed graphql url holding the token and DEL": {"GITHUB_TOKEN": tok, "GITHUB_GRAPHQL_URL": "https://api.example.com/" + tok + "\x7f"},
		"a malformed api url holding the token":             {"GITHUB_TOKEN": tok, "GITHUB_API_URL": "https://api.example.com/" + tok + "\n"},
		"an http endpoint holding the token":                {"GITHUB_TOKEN": tok, "GITHUB_API_URL": "http://api.example.com/" + tok},
		"a token with a control character":                  {"GITHUB_TOKEN": tok + "\nX"},
		"a token with a space":                              {"GITHUB_TOKEN": tok + " X"},
	} {
		env["GITHUB_ADAPTER_CONFIG"] = cfg
		code, out, errText := runCall(t, env, []string{"list-untracked"}, `{"contractVersion":1}`)
		if code != 1 || out != "" || strings.Contains(errText, tok) || strings.Contains(errText, "SYNTH3TIC") {
			t.Errorf("%s: %d %q %q", name, code, out, errText)
		}
	}
}
