package adapter

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// issueRef is an issue named by an external id or a short reference.
type issueRef struct {
	Owner, Name string
	Key         string // lower-cased owner/name
	Number      int
}

// parseIssueURL reads https://<host>/<owner>/<name>/issues/<number>, exactly:
// no userinfo, query, fragment, port or trailing path. It is how an external
// id the CLI hands back is read, so it accepts only the shape list-untracked
// produces.
func parseIssueURL(raw, host string) (issueRef, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return issueRef{}, errors.New("not a URL")
	}
	if u.Scheme != "https" || !strings.EqualFold(u.Host, host) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return issueRef{}, fmt.Errorf("not an issue URL on %s", host)
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(parts) != 4 || parts[2] != "issues" || !validSegment(parts[0]) || !validSegment(parts[1]) {
		return issueRef{}, fmt.Errorf("not an issue URL (https://%s/<owner>/<name>/issues/<number>)", host)
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 || strconv.Itoa(n) != parts[3] {
		return issueRef{}, errors.New("the issue number is not a positive number")
	}
	return issueRef{Owner: parts[0], Name: parts[1], Key: strings.ToLower(parts[0] + "/" + parts[1]), Number: n}, nil
}

// parseShortRef reads the forms normalize accepts: an issue URL in any case
// and with a trailing slash, "owner/name#N", and "#N" when exactly one
// repository is configured.
func (c *Config) parseShortRef(ref string) (issueRef, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(strings.ToLower(ref), "https://") {
		return parseIssueURL(strings.TrimSuffix(ref, "/"), c.WebHost)
	}
	repoPart, num, ok := strings.Cut(ref, "#")
	if !ok {
		return issueRef{}, errors.New(`a reference is an issue URL, "owner/name#N" or "#N"`)
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return issueRef{}, errors.New("the issue number is not a positive number")
	}
	if repoPart == "" {
		if len(c.Repositories) != 1 {
			return issueRef{}, errors.New(`"#N" names an issue only when exactly one repository is configured`)
		}
		repoPart = c.Repositories[0]
	}
	o, name, ok := strings.Cut(repoPart, "/")
	if !ok || !validSegment(o) || !validSegment(name) {
		return issueRef{}, errors.New(`a reference is an issue URL, "owner/name#N" or "#N"`)
	}
	return issueRef{Owner: o, Name: name, Key: strings.ToLower(o + "/" + name), Number: n}, nil
}

// checkExternalID applies the external id's checks (cor:agt:120:02): 1 to
// 512 characters, no whitespace, control or invisible format characters, no
// userinfo. The server applies them too; checking here refuses a bad id
// before the CLI tries to claim it.
func checkExternalID(id string) error {
	if id == "" || utf8.RuneCountInString(id) > 512 {
		return errors.New("an external id is 1 to 512 characters")
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return errors.New("an external id has no whitespace, control or invisible format characters")
		}
	}
	if u, err := url.Parse(id); err == nil && u.User != nil {
		return errors.New("an external id carries no userinfo")
	}
	return nil
}

// oneLine makes outside text safe for one line of a terminal or a message:
// control and invisible format characters become spaces, runs of spaces
// collapse, and it is cut to max runes.
func oneLine(s string, max int) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		if n == max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		space = false
		n++
	}
	return strings.TrimSpace(b.String())
}

// boundTitle keeps a title inside the runner's 1024-byte bound, cutting on a
// character boundary. GitHub's own 256-character limit keeps real titles far
// inside it.
func boundTitle(s string) string {
	const max = 1000
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
