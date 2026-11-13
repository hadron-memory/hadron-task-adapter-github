// Package conformance holds the hadron-cli intake runner's rules for an
// adapter's answers, and a suite that runs the built adapter against them.
//
// The rules are copied from hadron-cli at 585bd9d6 (internal/intake/intake.go,
// hadron-cli#1961, the invocation format of cor:agt:120:04), because that
// package is internal to the CLI module and cannot be imported. They are
// copied, not paraphrased: each constant names its source. When the CLI's
// runner changes a bound, change it here in the same breath and name the new
// revision in RunnerRevision.
package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RunnerRevision is the hadron-cli commit whose runner these rules copy.
const RunnerRevision = "585bd9d6"

// ContractVersion is the contract version the runner speaks (intake.ContractVersion).
const ContractVersion = 1

// Bounds, as in hadron-cli internal/intake/intake.go at RunnerRevision.
const (
	MaxOutputBytes      = 1 << 20 // stdout of one call
	MaxItemsPerPage     = 100
	MaxPages            = 50
	ServerExternalIDMax = 512 // code points
	MaxExternalIDLen    = 2048
	MaxCursorLen        = 4096
	MaxTextLen          = 1024
)

// CoreCalls are the calls every adapter answers (cor:agt:120:04, The Grammar).
var CoreCalls = []string{"describe", "list-untracked", "mark-tracked", "untrack", "get"}

// Describe is what the runner reads from `describe`.
type Describe struct {
	Name            string   `json:"name"`
	ContractVersion int      `json:"contractVersion"`
	Calls           []string `json:"calls"`
	Filters         []string `json:"filters"`
	Order           string   `json:"order"`
}

// Item and Page are what the runner reads from `list-untracked`.
type Item struct {
	ExternalID string `json:"externalId"`
	Title      string `json:"title"`
}

type Page struct {
	Items      []Item  `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// OneObject applies the runner's checks on any answer: at most
// MaxOutputBytes, valid UTF-8, exactly one JSON object and nothing after it.
func OneObject(stdout []byte, out any) error {
	if len(stdout) > MaxOutputBytes {
		return fmt.Errorf("more than %d bytes on stdout", MaxOutputBytes)
	}
	raw := bytes.TrimSpace(stdout)
	if len(raw) == 0 || raw[0] != '{' {
		return fmt.Errorf("stdout is not one JSON object: %q", trunc(raw))
	}
	if !utf8.Valid(raw) {
		return fmt.Errorf("stdout is not valid UTF-8")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("stdout is not one well-formed JSON object: %v", err)
	}
	return nil
}

// CheckDescribe applies the runner's describe checks, plus the contract's
// rule that every core call is answered.
func CheckDescribe(d Describe) error {
	if d.ContractVersion != ContractVersion {
		return fmt.Errorf("contractVersion %d, the runner speaks %d", d.ContractVersion, ContractVersion)
	}
	if strings.TrimSpace(d.Name) == "" || len(d.Name) > MaxTextLen {
		return fmt.Errorf("name %q is empty or longer than %d bytes", d.Name, MaxTextLen)
	}
	for _, c := range CoreCalls {
		found := false
		for _, have := range d.Calls {
			found = found || have == c
		}
		if !found {
			return fmt.Errorf("describe.calls does not list the core call %q", c)
		}
	}
	return nil
}

// CheckPage applies the runner's per-page checks, and the external id's own
// checks (cor:agt:120:02: 1 to 512 characters, no whitespace, control or
// invisible format characters, no userinfo), which the server applies to the
// claim the runner writes.
func CheckPage(p Page) error {
	if len(p.Items) > MaxItemsPerPage {
		return fmt.Errorf("%d items in one page, more than %d", len(p.Items), MaxItemsPerPage)
	}
	for i, it := range p.Items {
		if err := CheckExternalID(it.ExternalID); err != nil {
			return fmt.Errorf("item %d: %v", i, err)
		}
		if len(it.Title) > MaxTextLen {
			return fmt.Errorf("item %d: title longer than %d bytes", i, MaxTextLen)
		}
	}
	if p.NextCursor != nil && len(*p.NextCursor) > MaxCursorLen {
		return fmt.Errorf("nextCursor is %d bytes, more than %d", len(*p.NextCursor), MaxCursorLen)
	}
	return nil
}

// CheckExternalID is the external id's checks.
func CheckExternalID(id string) error {
	if id == "" {
		return fmt.Errorf("no externalId")
	}
	if len(id) > MaxExternalIDLen || utf8.RuneCountInString(id) > ServerExternalIDMax {
		return fmt.Errorf("externalId longer than %d characters", ServerExternalIDMax)
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("externalId %q has whitespace, control or invisible format characters", id)
		}
	}
	if scheme, rest, ok := strings.Cut(id, "://"); ok && scheme != "" {
		host, _, _ := strings.Cut(rest, "/")
		if strings.Contains(host, "@") {
			return fmt.Errorf("externalId %q carries userinfo", id)
		}
	}
	return nil
}

func trunc(b []byte) string {
	if len(b) > 80 {
		return string(b[:80]) + "…"
	}
	return string(b)
}
