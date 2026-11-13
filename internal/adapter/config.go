package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// ConfigEnv names the configuration file. The adapter takes no flags (the
// runner passes exactly one argument, the call), so this and the default
// path are the only ways to point it at a team's configuration.
const ConfigEnv = "TASK_ADAPTER_GITHUB_CONFIG"

// Limits on the configuration.
const (
	MaxRepositories   = 30 // keeps the cursor inside the runner's 4096-byte bound
	MaxLabelLen       = 100
	MaxExcludeLabels  = 50
	MaxTeams          = 50
	MaxForPerTeam     = 20
	DefaultTracked    = "hadron-tracked"
	DefaultAPIURL     = "https://api.github.com"
	DefaultWebHost    = "github.com"
	maxConfigFileSize = 256 << 10
)

// Config is a team's adapter configuration: today's label model
// (cor:agt:120, D7: tracker labels never reach Hadron, they live here), the
// repositories, and how the adapter marks an item tracked. It holds no
// credential: the token is read from the environment.
type Config struct {
	// Repositories are "owner/name"; their open issues are the intake.
	Repositories []string `json:"repositories"`
	// ReadyLabel marks an issue ready for intake.
	ReadyLabel string `json:"readyLabel,omitempty"`
	// Ready is "only" (list only ready issues; the default when ReadyLabel
	// is set), "exclude" or "any" (the default when it is not).
	Ready string `json:"ready,omitempty"`
	// ExcludeLabels: an issue carrying any of them is never listed.
	ExcludeLabels []string `json:"excludeLabels,omitempty"`
	// Teams maps the runner's `for` (a worker's name or URN) to a team label,
	// or to the issues carrying no team label at all.
	Teams []Team `json:"teams,omitempty"`
	// TrackedLabel is the adapter's mark. Default "hadron-tracked".
	TrackedLabel string `json:"trackedLabel,omitempty"`
	// Comment also posts a comment naming the task and worker on mark-tracked.
	// Off by default: on a public repository it publishes the team's names.
	Comment bool `json:"comment,omitempty"`
	// TokenEnv names the environment variable holding the team's GitHub
	// token. Unset: GITHUB_TOKEN, then GH_TOKEN.
	TokenEnv string `json:"tokenEnv,omitempty"`
	// APIURL is the REST base (GitHub Enterprise Server: https://HOST/api/v3).
	APIURL string `json:"apiUrl,omitempty"`
	// GraphQLURL defaults to APIURL + "/graphql" (Enterprise Server: https://HOST/api/graphql).
	GraphQLURL string `json:"graphqlUrl,omitempty"`
	// WebHost is the host of issue URLs. Default github.com.
	WebHost string `json:"webHost,omitempty"`
}

// Team is one entry of the team map.
type Team struct {
	For    []string `json:"for"`
	Label  string   `json:"label,omitempty"`
	NoTeam bool     `json:"noTeam,omitempty"`
}

// repo is one configured repository.
type repo struct {
	Owner, Name string
	Key         string // lower-cased "owner/name"
}

var repoSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validSegment is one segment of "owner/name": GitHub's characters, and never
// "." or "..", which would make a path out of a name.
func validSegment(s string) bool {
	return repoSegment.MatchString(s) && s != "." && s != ".."
}

// ConfigPath is where the configuration is read from.
func ConfigPath(getenv func(string) string) (string, error) {
	if p := getenv(ConfigEnv); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no configuration: set %s, or create it under the user configuration directory", ConfigEnv)
	}
	return filepath.Join(dir, "hadron-task-adapter-github", "config.json"), nil
}

// LoadConfig reads and validates the configuration.
func LoadConfig(getenv func(string) string) (*Config, error) {
	p, err := ConfigPath(getenv)
	if err != nil {
		return nil, err
	}
	b, err := readBounded(p, maxConfigFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no configuration at %s (set %s to its path; see config.example.json)", p, ConfigEnv)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the configuration %s: %v", p, err)
	}
	return ParseConfig(b)
}

func readBounded(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("larger than %d bytes", max)
	}
	return b, nil
}

// ParseConfig decodes and validates a configuration. Unknown keys are
// refused: a misspelt label key would otherwise silently widen the intake.
func ParseConfig(b []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("configuration: %v", err)
	}
	if dec.More() {
		return nil, errors.New("configuration: more than one JSON value")
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("configuration: %v", err)
	}
	return &c, nil
}

func checkLabel(field, l string) error {
	if l == "" || len([]rune(l)) > MaxLabelLen {
		return fmt.Errorf("%s: a label is 1 to %d characters", field, MaxLabelLen)
	}
	for _, r := range l {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%s: a label may not contain control or invisible format characters", field)
		}
	}
	return nil
}

func (c *Config) validate() error {
	if len(c.Repositories) == 0 || len(c.Repositories) > MaxRepositories {
		return fmt.Errorf("repositories: list 1 to %d repositories", MaxRepositories)
	}
	seen := map[string]bool{}
	for i, r := range c.Repositories {
		parts := strings.Split(r, "/")
		if len(parts) != 2 || !validSegment(parts[0]) || !validSegment(parts[1]) {
			return fmt.Errorf("repositories[%d]: %q is not \"owner/name\"", i, r)
		}
		k := strings.ToLower(r)
		if seen[k] {
			return fmt.Errorf("repositories[%d]: %q is listed twice", i, r)
		}
		seen[k] = true
	}
	if c.ReadyLabel != "" {
		if err := checkLabel("readyLabel", c.ReadyLabel); err != nil {
			return err
		}
	}
	switch c.Ready {
	case "":
		c.Ready = "any"
		if c.ReadyLabel != "" {
			c.Ready = "only"
		}
	case "only", "exclude":
		if c.ReadyLabel == "" {
			return fmt.Errorf("ready: %q needs a readyLabel", c.Ready)
		}
	case "any":
	default:
		return fmt.Errorf("ready: %q is not one of only, exclude, any", c.Ready)
	}
	if len(c.ExcludeLabels) > MaxExcludeLabels {
		return fmt.Errorf("excludeLabels: at most %d", MaxExcludeLabels)
	}
	for i, l := range c.ExcludeLabels {
		if err := checkLabel(fmt.Sprintf("excludeLabels[%d]", i), l); err != nil {
			return err
		}
	}
	if len(c.Teams) > MaxTeams {
		return fmt.Errorf("teams: at most %d", MaxTeams)
	}
	who := map[string]bool{}
	for i, t := range c.Teams {
		at := fmt.Sprintf("teams[%d]", i)
		if len(t.For) == 0 || len(t.For) > MaxForPerTeam {
			return fmt.Errorf("%s.for: name 1 to %d workers (names or URNs)", at, MaxForPerTeam)
		}
		for j, f := range t.For {
			if strings.TrimSpace(f) == "" || len(f) > 512 {
				return fmt.Errorf("%s.for[%d]: a worker name or URN of 1 to 512 bytes", at, j)
			}
			k := strings.ToLower(f)
			if who[k] {
				return fmt.Errorf("%s.for[%d]: %q is mapped to another team already", at, j, f)
			}
			who[k] = true
		}
		switch {
		case t.NoTeam && t.Label != "":
			return fmt.Errorf("%s: a team has a label or noTeam, not both", at)
		case !t.NoTeam:
			if err := checkLabel(at+".label", t.Label); err != nil {
				return err
			}
		}
	}
	if c.TrackedLabel == "" {
		c.TrackedLabel = DefaultTracked
	}
	if err := checkLabel("trackedLabel", c.TrackedLabel); err != nil {
		return err
	}
	if strings.EqualFold(c.TrackedLabel, c.ReadyLabel) {
		return errors.New("trackedLabel: must differ from readyLabel")
	}
	if c.TokenEnv != "" {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(c.TokenEnv) {
			return fmt.Errorf("tokenEnv: %q is not an environment variable name", c.TokenEnv)
		}
		// The hadron CLI strips HADRON_* credentials from the adapter's
		// environment (cor:agt:120, D2): such a variable never arrives.
		if strings.HasPrefix(strings.ToUpper(c.TokenEnv), "HADRON_") {
			return errors.New("tokenEnv: a HADRON_ variable is never passed to the adapter; the GitHub token is the team's own")
		}
	}
	if c.APIURL == "" {
		c.APIURL = DefaultAPIURL
	}
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	if err := checkEndpoint("apiUrl", c.APIURL); err != nil {
		return err
	}
	if c.GraphQLURL == "" {
		c.GraphQLURL = c.APIURL + "/graphql"
	}
	if err := checkEndpoint("graphqlUrl", c.GraphQLURL); err != nil {
		return err
	}
	if c.WebHost == "" {
		c.WebHost = DefaultWebHost
	}
	if strings.ContainsAny(c.WebHost, "/@: \t") {
		return fmt.Errorf("webHost: %q is a host name, not a URL", c.WebHost)
	}
	return nil
}

// checkEndpoint: the token is sent there, so it is https, or plain http only
// to the loopback interface (a local fake or proxy).
func checkEndpoint(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s: %q is not a plain https URL", field, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
	}
	return fmt.Errorf("%s: %q must be https (http only to the loopback interface)", field, raw)
}

func (c *Config) repos() []repo {
	out := make([]repo, 0, len(c.Repositories))
	for _, r := range c.Repositories {
		o, n, _ := strings.Cut(r, "/")
		out = append(out, repo{Owner: o, Name: n, Key: strings.ToLower(r)})
	}
	return out
}

func (c *Config) hasRepo(owner, name string) bool {
	k := strings.ToLower(owner + "/" + name)
	for _, r := range c.repos() {
		if r.Key == k {
			return true
		}
	}
	return false
}

// team resolves the runner's `for`. ok is false when `for` is given and no
// team maps it.
func (c *Config) team(forWorker string) (t *Team, ok bool) {
	if forWorker == "" {
		return nil, true
	}
	for i := range c.Teams {
		for _, f := range c.Teams[i].For {
			if strings.EqualFold(f, forWorker) {
				return &c.Teams[i], true
			}
		}
	}
	return nil, false
}

// Token returns the team's GitHub token from the environment.
func (c *Config) Token(getenv func(string) string) (string, error) {
	names := []string{"GITHUB_TOKEN", "GH_TOKEN"}
	if c.TokenEnv != "" {
		names = []string{c.TokenEnv}
	}
	for _, n := range names {
		if v := strings.TrimSpace(getenv(n)); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("no GitHub credential: set %s to the team's own GitHub token (never a Hadron one)", strings.Join(names, " or "))
}
