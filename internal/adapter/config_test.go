package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigReadsTheLabelModelAndNormalizes(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `{
	  "repositories": ["Acme/App", " acme/lib "],
	  "excludeLabels": ["wontfix"], "readyLabel": "ready", "requireReady": true,
	  "teams": [{"label": "team:alpha", "lead": "Iris"}],
	  "trackedLabel": "hadron:tracked", "comment": true, "defaultRepository": "ACME/app",
	  "report": {"FINISHED": {"addLabels": ["done"]}}, "maxPagesPerRepository": 3
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Repositories[0] != "acme/app" || c.Repositories[1] != "acme/lib" || c.DefaultRepository != "acme/app" || c.maxPages() != 3 {
		t.Fatalf("%+v", c)
	}
	if (&Config{}).maxPages() != DefaultMaxPages {
		t.Fatal("the default bound")
	}
}

func TestLoadConfigRefusesWhatCouldSilentlyDisableAFilter(t *testing.T) {
	for name, body := range map[string]string{
		"a typo'd key":             `{"repositories":["a/b"],"excludeLabel":["x"]}`,
		"no repositories":          `{"repositories":[]}`,
		"a bad repository":         `{"repositories":["not-a-repo"]}`,
		"two slashes":              `{"repositories":["a/b/c"]}`,
		"a repository twice":       `{"repositories":["a/b","A/B"]}`,
		"requireReady, no label":   `{"repositories":["a/b"],"requireReady":true}`,
		"a blank excluded label":   `{"repositories":["a/b"],"excludeLabels":[" "]}`,
		"a team without a lead":    `{"repositories":["a/b"],"teams":[{"label":"x"}]}`,
		"a repeated team lead":     `{"repositories":["a/b"],"teams":[{"label":"x","lead":"I"},{"label":"y","lead":"i"}]}`,
		"a repeated team label":    `{"repositories":["a/b"],"teams":[{"label":"x","lead":"I"},{"label":"X","lead":"j"}]}`,
		"a bad default repository": `{"repositories":["a/b"],"defaultRepository":"nope"}`,
		"a negative bound":         `{"repositories":["a/b"],"maxPagesPerRepository":-1}`,
		"a blank report label":     `{"repositories":["a/b"],"report":{"FINISHED":{"addLabels":[""]}}}`,
		"text after the object":    `{"repositories":["a/b"]} {}`,
		"not json":                 `nope`,
	} {
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil || !strings.Contains(err.Error(), "missing.json") {
		t.Errorf("a missing file must be named: %v", err)
	}
}

func TestConfigPathOrder(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	home := func() (string, error) { return "/home/u", nil }
	if p, _ := ConfigPath(env(map[string]string{ConfigEnv: "/x/c.json", "XDG_CONFIG_HOME": "/xdg"}), home); p != "/x/c.json" {
		t.Errorf("env wins: %s", p)
	}
	if p, _ := ConfigPath(env(map[string]string{"XDG_CONFIG_HOME": "/xdg"}), home); p != "/xdg/hadron-task-adapter-github/config.json" {
		t.Errorf("xdg: %s", p)
	}
	if p, _ := ConfigPath(env(nil), home); p != "/home/u/.config/hadron-task-adapter-github/config.json" {
		t.Errorf("home: %s", p)
	}
	if _, err := ConfigPath(env(nil), func() (string, error) { return "", os.ErrNotExist }); err == nil {
		t.Error("no home and no env must be an error")
	}
}
