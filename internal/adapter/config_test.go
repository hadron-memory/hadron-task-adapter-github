package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRefusals(t *testing.T) {
	cases := map[string]string{
		"no repositories":           `{"repositories":[]}`,
		"not owner/name":            `{"repositories":["acme"]}`,
		"path in name":              `{"repositories":["acme/app/x"]}`,
		"dot name":                  `{"repositories":["acme/.."]}`,
		"dot owner":                 `{"repositories":["../app"]}`,
		"duplicate repo":            `{"repositories":["acme/app","ACME/App"]}`,
		"unknown key":               `{"repositories":["acme/app"],"readyLable":"x"}`,
		"two values":                `{"repositories":["acme/app"]}{}`,
		"trailing bracket":          `{"repositories":["acme/app"]}]`,
		"trailing brace":            `{"repositories":["acme/app"]}}`,
		"twenty-one repositories":   `{"repositories":[` + manyRepos(21) + `]}`,
		"ready only without label":  `{"repositories":["acme/app"],"ready":"only"}`,
		"ready unknown":             `{"repositories":["acme/app"],"readyLabel":"r","ready":"some"}`,
		"control in label":          "{\"repositories\":[\"acme/app\"],\"readyLabel\":\"a\\u0007b\"}",
		"format char in label":      "{\"repositories\":[\"acme/app\"],\"excludeLabels\":[\"a\\u200bb\"]}",
		"team without for":          `{"repositories":["acme/app"],"teams":[{"for":[],"label":"t"}]}`,
		"team with label and none":  `{"repositories":["acme/app"],"teams":[{"for":["a"],"label":"t","noTeam":true}]}`,
		"team without label":        `{"repositories":["acme/app"],"teams":[{"for":["a"]}]}`,
		"worker in two teams":       `{"repositories":["acme/app"],"teams":[{"for":["Ada"],"label":"t"},{"for":["ada"],"label":"u"}]}`,
		"tracked equals ready":      `{"repositories":["acme/app"],"readyLabel":"x","trackedLabel":"X"}`,
		"hadron token env":          `{"repositories":["acme/app"],"tokenEnv":"HADRON_GITHUB_TOKEN"}`,
		"bad token env":             `{"repositories":["acme/app"],"tokenEnv":"A-B"}`,
		"plain http to remote":      `{"repositories":["acme/app"],"apiUrl":"http://api.example.com"}`,
		"userinfo in api url":       `{"repositories":["acme/app"],"apiUrl":"https://u:p@api.example.com"}`,
		"web host with scheme":      `{"repositories":["acme/app"],"webHost":"https://github.com"}`,
		"graphql url to plain http": `{"repositories":["acme/app"],"graphqlUrl":"http://example.com/graphql"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(raw)); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
}

func manyRepos(n int) string {
	parts := []string{}
	for i := 0; i < n; i++ {
		parts = append(parts, `"o/r`+strings.Repeat("x", i)+`"`)
	}
	return strings.Join(parts, ",")
}

func TestConfigDefaults(t *testing.T) {
	c, err := ParseConfig([]byte(`{"repositories":["acme/app"],"readyLabel":"cleared"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Ready != "only" || c.TrackedLabel != DefaultTracked || c.APIURL != DefaultAPIURL || c.GraphQLURL != DefaultAPIURL+"/graphql" || c.WebHost != DefaultWebHost {
		t.Fatalf("defaults: %+v", c)
	}
	c, err = ParseConfig([]byte(`{"repositories":["acme/app"],"apiUrl":"https://ghe.example.com/api/v3/","graphqlUrl":"https://ghe.example.com/api/graphql","webHost":"ghe.example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Ready != "any" || c.APIURL != "https://ghe.example.com/api/v3" {
		t.Fatalf("enterprise: %+v", c)
	}
}

func TestTokenLookup(t *testing.T) {
	c, _ := ParseConfig([]byte(`{"repositories":["acme/app"]}`))
	env := map[string]string{"GH_TOKEN": "b"}
	if tok, err := c.Token(func(k string) string { return env[k] }); err != nil || tok != "b" {
		t.Fatalf("GH_TOKEN: %q %v", tok, err)
	}
	env["GITHUB_TOKEN"] = "a"
	if tok, _ := c.Token(func(k string) string { return env[k] }); tok != "a" {
		t.Fatalf("GITHUB_TOKEN first: %q", tok)
	}
	c.TokenEnv = "TEAM_GH"
	if _, err := c.Token(func(k string) string { return env[k] }); err == nil {
		t.Fatal("tokenEnv set but empty: fell back to another variable")
	}
}

func TestConfigFileIsBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"repositories":["acme/app"],"readyLabel":"`+strings.Repeat("a", maxConfigFileSize)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(func(k string) string {
		if k == ConfigEnv {
			return p
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversized configuration: %v", err)
	}
}

func TestParseIssueURL(t *testing.T) {
	good := map[string]issueRef{
		"https://github.com/Acme/App/issues/12": {Owner: "Acme", Name: "App", Key: "acme/app", Number: 12},
	}
	for raw, want := range good {
		got, err := parseIssueURL(raw, "github.com")
		if err != nil || got != want {
			t.Fatalf("%s: %+v %v", raw, got, err)
		}
	}
	for _, raw := range []string{
		"http://github.com/acme/app/issues/1",
		"https://evil.example/acme/app/issues/1",
		"https://u@github.com/acme/app/issues/1",
		"https://github.com:443/acme/app/issues/1",
		"https://github.com/acme/app/pull/1",
		"https://github.com/acme/app/issues/1/",
		"https://github.com/acme/app/issues/01",
		"https://github.com/acme/app/issues/0",
		"https://github.com/acme/app/issues/1?x=1",
		"https://github.com/acme/app/issues/1#c",
		"https://github.com/acme/app/issues/1/comments",
		"https://github.com/acme/../issues/1",
		"https://github.com/acme/app%2fx/issues/1",
	} {
		if _, err := parseIssueURL(raw, "github.com"); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestCheckExternalID(t *testing.T) {
	for _, bad := range []string{"", "https://github.com/a/b/issues/1 ", "https://x/\u200b", "https://u:p@github.com/a", strings.Repeat("a", 513)} {
		if checkExternalID(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if err := checkExternalID(strings.Repeat("é", 512)); err != nil {
		t.Errorf("512 code points refused: %v", err)
	}
}

func TestOneLineAndBoundTitle(t *testing.T) {
	if got := oneLine("a\n\tb\u202ec\x1b[31m", 50); got != "a b c [31m" {
		t.Fatalf("oneLine: %q", got)
	}
	long := strings.Repeat("é", 700) // 1400 bytes
	got := boundTitle(long)
	if len(got) > 1024 || !strings.HasSuffix(got, "…") {
		t.Fatalf("boundTitle: %d bytes", len(got))
	}
}

// The example the README points at is a valid configuration.
func TestExampleConfigParses(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseConfig(b); err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
}

// A fragment of the credential is redacted wherever a cut can leave one:
// text that arrives already truncated (a proxy echoing a cut token), and a
// cut of ours that lands inside it (Ada's ruling on the LOCAL delta F3).
func TestClipRedactsAnyFragmentOfTheCredential(t *testing.T) {
	const tok = "ghp_FAKEunitTOKEN000000000000000000000000"
	g := &githubClient{token: tok}
	// Text that arrives already cut: any prefix right before the ellipsis,
	// and any prefix of 8 runes or more anywhere.
	for k := 1; k < len(tok); k++ {
		for _, tail := range []string{"…", " and more"} {
			in := "bad URL https://github.com/a/b/issues/1?echo=" + tok[:k] + tail
			got := g.clip(in, 200)
			if strings.Contains(got, tok[:k]+"…") || (k >= 8 && strings.Contains(got, tok[:k])) {
				t.Fatalf("prefix %d (tail %q) survived: %q", k, tail, got)
			}
		}
	}
	// A cut of ours, at every length.
	for max := 1; max < 120; max++ {
		got := g.clip(strings.Repeat("x", 40)+tok+" tail", max)
		if strings.Contains(got, tok[:8]) || strings.Contains(got, "g…") {
			t.Fatalf("cut at %d leaked: %q", max, got)
		}
	}
	// The whole credential is replaced exactly, leaving none of it behind.
	if got := g.clip("a "+tok+" b", 200); got != "a [redacted] b" {
		t.Fatalf("whole credential: %q", got)
	}
	if got := g.clip("plain text, no credential", 200); got != "plain text, no credential" {
		t.Fatalf("clean text changed: %q", got)
	}
}
