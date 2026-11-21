// Package adapter is the GitHub intake adapter for hadron-cli's task intake
// contract (cor:agt:120:04, contract version 1).
package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ConfigEnv names the config file. The CLI removes every HADRON_* variable
// from an adapter's environment, so the adapter's own settings live under its
// own names.
const ConfigEnv = "GITHUB_ADAPTER_CONFIG"

// Team maps a tracker label to the worker who leads that team: the CLI's
// `--for <worker>` asks for the items carrying the label of the team that
// worker leads.
type Team struct {
	Label string `json:"label"`
	Lead  string `json:"lead"`
}

// ReportAction is what `report` does on an issue when a task reaches a state.
type ReportAction struct {
	AddLabels    []string `json:"addLabels"`
	RemoveLabels []string `json:"removeLabels"`
}

// Config is the team's label model: which repositories, which labels exclude
// an item, which label means ready, which labels name a team, how the adapter
// marks a tracked item and what it does on a report. It carries no credential:
// the token is read from GITHUB_TOKEN.
type Config struct {
	// Repositories are the "owner/name" repositories whose open issues are listed.
	Repositories []string `json:"repositories"`
	// ExcludeLabels hide an item that carries any of them (case-insensitive).
	ExcludeLabels []string `json:"excludeLabels"`
	// ReadyLabel is the label that marks an item ready; with RequireReady only
	// items carrying it are listed.
	ReadyLabel   string `json:"readyLabel"`
	RequireReady bool   `json:"requireReady"`
	// Teams map a label to the worker that leads the team.
	Teams []Team `json:"teams"`
	// TrackedLabel, when set, is added by mark-tracked and removed by untrack.
	TrackedLabel string `json:"trackedLabel"`
	// Comment, when true, makes mark-tracked leave one comment naming the task
	// and worker (updated, not repeated, on a re-mark).
	Comment bool `json:"comment"`
	// CommentAuthor is the GitHub login of the account the token acts as. With
	// comment on, the adapter edits only comments carrying its marker AND written
	// by this login, so it never overwrites someone else's text. Unset, the
	// login is read from GET /user, which a personal or fine-grained token
	// answers; an installation token (a GitHub App) cannot, so name the app's
	// bot login here (for example "my-app[bot]").
	CommentAuthor string `json:"commentAuthor"`
	// DefaultRepository lets `normalize` accept "#123".
	DefaultRepository string `json:"defaultRepository"`
	// Report maps a Hadron task state (e.g. "FINISHED", any case) to label changes.
	Report map[string]ReportAction `json:"report"`
	// MaxPagesPerRepository bounds the tracker read (100 issues a page); 0 is
	// the default of 20. A repository past it is refused, never listed in part.
	MaxPagesPerRepository int `json:"maxPagesPerRepository"`
}

// DefaultMaxPages is the read bound per repository when the config sets none.
const DefaultMaxPages = 20

// ConfigPath is where the config is read from: $GITHUB_ADAPTER_CONFIG, else
// $XDG_CONFIG_HOME/hadron-task-adapter-github/config.json, else
// ~/.config/hadron-task-adapter-github/config.json.
func ConfigPath(getenv func(string) string, home func() (string, error)) (string, error) {
	if p := getenv(ConfigEnv); p != "" {
		return p, nil
	}
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "hadron-task-adapter-github", "config.json"), nil
	}
	h, err := home()
	if err != nil || h == "" {
		return "", errors.New("no config: set " + ConfigEnv + " (the home directory is unknown)")
	}
	return filepath.Join(h, ".config", "hadron-task-adapter-github", "config.json"), nil
}

// LoadConfig reads and validates the config file. An unknown key is an error,
// so a typo cannot silently disable a filter.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the config %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("the config %s is not valid: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("the config %s has text after its JSON object", path)
	}
	if err := c.normalize(); err != nil {
		return nil, fmt.Errorf("the config %s is not valid: %w", path, err)
	}
	return &c, nil
}

func (c *Config) normalize() error {
	if len(c.Repositories) == 0 {
		return errors.New("repositories is empty: name at least one owner/name")
	}
	seen := map[string]bool{}
	for i, r := range c.Repositories {
		r = strings.ToLower(strings.TrimSpace(r))
		if !validRepo(r) {
			return fmt.Errorf("repositories[%d] %q is not owner/name", i, c.Repositories[i])
		}
		if seen[r] {
			return fmt.Errorf("repositories lists %q twice", r)
		}
		seen[r] = true
		c.Repositories[i] = r
	}
	for i, l := range c.ExcludeLabels {
		if strings.TrimSpace(l) == "" {
			return fmt.Errorf("excludeLabels[%d] is blank", i)
		}
	}
	if c.RequireReady && strings.TrimSpace(c.ReadyLabel) == "" {
		return errors.New("requireReady needs a readyLabel")
	}
	teamLabels := map[string]bool{}
	teamLeads := map[string]bool{}
	for i, t := range c.Teams {
		if strings.TrimSpace(t.Label) == "" || strings.TrimSpace(t.Lead) == "" {
			return fmt.Errorf("teams[%d] needs a label and a lead", i)
		}
		l, w := strings.ToLower(t.Label), strings.ToLower(t.Lead)
		if teamLabels[l] || teamLeads[w] {
			return fmt.Errorf("teams[%d] repeats a label or a lead", i)
		}
		teamLabels[l], teamLeads[w] = true, true
	}
	if c.DefaultRepository != "" {
		d := strings.ToLower(strings.TrimSpace(c.DefaultRepository))
		if !validRepo(d) {
			return fmt.Errorf("defaultRepository %q is not owner/name", c.DefaultRepository)
		}
		c.DefaultRepository = d
	}
	if c.MaxPagesPerRepository < 0 {
		return errors.New("maxPagesPerRepository cannot be negative")
	}
	normalized := map[string]ReportAction{}
	for state, a := range c.Report {
		key := strings.ToUpper(strings.TrimSpace(state))
		if key == "" {
			return errors.New("report has a blank state")
		}
		if _, dup := normalized[key]; dup {
			return fmt.Errorf("report names the state %q twice (states are compared without regard to case)", key)
		}
		for _, l := range append(append([]string{}, a.AddLabels...), a.RemoveLabels...) {
			if strings.TrimSpace(l) == "" {
				return fmt.Errorf("report[%s] has a blank label", state)
			}
		}
		normalized[key] = a
	}
	if c.Report != nil {
		c.Report = normalized
	}
	return nil
}

func (c *Config) maxPages() int {
	if c.MaxPagesPerRepository > 0 {
		return c.MaxPagesPerRepository
	}
	return DefaultMaxPages
}

func (c *Config) inRepositories(repo string) bool {
	repo = strings.ToLower(repo)
	for _, r := range c.Repositories {
		if r == repo {
			return true
		}
	}
	return false
}

func validRepo(r string) bool {
	owner, name, ok := strings.Cut(r, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return false
	}
	for _, s := range []string{owner, name} {
		for _, c := range s {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return false
			}
		}
	}
	return true
}
