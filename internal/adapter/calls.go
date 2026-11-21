package adapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Contract constants (cor:agt:120:04, contract version 1).
const (
	ContractVersion = 1
	AdapterName     = "hadron-task-adapter-github"
	maxPage         = 100
	defaultPage     = 20
	maxExternalID   = 512 // code points, the server's cap
	maxBodyRunes    = 64 << 10
	commentMarker   = "<!-- hadron-task:"
	maxRequest      = 1 << 20
)

// markerLine is the first line of a comment the adapter owns: the marker, a
// task id (or "released") and the closing. Anything else is someone's text.
var markerID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

var markerLine = regexp.MustCompile(`^<!-- hadron-task:([A-Za-z0-9._:-]{1,128}|released) -->(\n|$)`)

// Exit codes. The CLI treats every non-zero exit as the adapter's refusal and
// shows the first 4 KiB of stderr, so each carries a one-line message.
const (
	ExitRefused  = 1 // GitHub or the config refused or failed
	ExitUsage    = 2 // the request is malformed
	ExitNotFound = 4 // the item does not exist
)

// Refusal is an error that exits with Code and prints Msg on stderr.
type Refusal struct {
	Code int
	Msg  string
}

func (r *Refusal) Error() string { return r.Msg }

// Calls lists every call this adapter answers, core first.
var Calls = []string{"describe", "list-untracked", "mark-tracked", "untrack", "get", "report", "changed-since", "normalize", "check"}

// Adapter is one invocation's state.
type Adapter struct {
	Config *Config
	Client *Client
	Host   string // the tracker's web host, e.g. github.com
	// ConfigError is why the config could not be loaded; only `check` runs
	// without one, and reports it.
	ConfigError string
	// ClientError is why the GitHub client could not be built (a bad endpoint
	// or token); only `check` runs without one, and reports it.
	ClientError     string
	CredentialAsked bool
	// Secret is the credential, so what check prints without a client is scrubbed too.
	Secret string
}

// Handle runs one call: stdin is the request object, the returned value is the
// single answer object. A *Refusal carries the exit code.
func (a *Adapter) Handle(ctx context.Context, call string, stdin io.Reader) (any, error) {
	req, err := decodeRequest(stdin)
	if err != nil {
		return nil, err
	}
	if call == "describe" {
		return Describe(), nil // answers without a config or a credential
	}
	if call == "check" && a.Client == nil && a.ClientError != "" {
		return a.checkWithoutClient(), nil // the request was judged above; the setup is reported, not refused
	}
	if call == "check" && a.Client != nil {
		return a.check(ctx) // runs without a (valid) config: it is how a bad setup is diagnosed
	}
	if a.Config == nil || a.Client == nil {
		return nil, &Refusal{Code: ExitRefused, Msg: "the adapter is not configured"}
	}
	switch call {
	case "list-untracked":
		return a.listUntracked(ctx, req)
	case "mark-tracked":
		return a.markTracked(ctx, req)
	case "untrack":
		return a.untrack(ctx, req)
	case "get":
		return a.get(ctx, req)
	case "report":
		return a.report(ctx, req)
	case "changed-since":
		return a.changedSince(ctx, req)
	case "normalize":
		return a.normalize(ctx, req)
	}
	return nil, &Refusal{Code: ExitUsage, Msg: "unknown call " + strconv.Quote(call) + "; the calls are " + strings.Join(Calls, ", ")}
}

// Describe is the adapter's self-description.
func Describe() map[string]any {
	return map[string]any{
		"name":            AdapterName,
		"contractVersion": ContractVersion,
		"calls":           Calls,
		"filters":         []string{"for"},
		"order":           "oldest-first",
	}
}

type request struct {
	ContractVersion *int            `json:"contractVersion"`
	For             *string         `json:"for"`
	Limit           *int            `json:"limit"`
	Cursor          *string         `json:"cursor"`
	ExternalID      string          `json:"externalId"`
	Reference       string          `json:"reference"`
	Since           string          `json:"since"`
	State           string          `json:"state"`
	Task            json.RawMessage `json:"task"`
	Worker          json.RawMessage `json:"worker"`
}

func decodeRequest(r io.Reader) (*request, error) {
	// One byte past the cap is read, so an oversized request is refused rather
	// than silently cut (trailing data beyond the cap would otherwise be hidden).
	raw, err := io.ReadAll(io.LimitReader(r, maxRequest+1))
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		return nil, &Refusal{Code: ExitUsage, Msg: "the request is empty: one JSON object is expected on stdin"}
	}
	if len(raw) > maxRequest {
		return nil, &Refusal{Code: ExitUsage, Msg: fmt.Sprintf("the request is larger than %d bytes", maxRequest)}
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, &Refusal{Code: ExitUsage, Msg: "the request is not one JSON object"}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var q request
	if err := dec.Decode(&q); err != nil {
		return nil, &Refusal{Code: ExitUsage, Msg: "the request is not one JSON object"}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &Refusal{Code: ExitUsage, Msg: "the request has text after its JSON object"}
	}
	// The CLI puts contractVersion in every request (describe included).
	if q.ContractVersion == nil {
		return nil, &Refusal{Code: ExitUsage, Msg: "the request has no contractVersion"}
	}
	if *q.ContractVersion != ContractVersion {
		return nil, &Refusal{Code: ExitUsage, Msg: fmt.Sprintf("this adapter speaks contract version %d, the request says %d", ContractVersion, *q.ContractVersion)}
	}
	return &q, nil
}

// ---- list-untracked ----

type itemOut struct {
	ExternalID string `json:"externalId"`
	Title      string `json:"title"`
}

type pageOut struct {
	Items      []itemOut `json:"items"`
	NextCursor *string   `json:"nextCursor"`
}

func (a *Adapter) listUntracked(ctx context.Context, q *request) (any, error) {
	limit := defaultPage
	if q.Limit != nil {
		if *q.Limit < 1 || *q.Limit > maxPage {
			return nil, &Refusal{Code: ExitUsage, Msg: fmt.Sprintf("limit must be 1 to %d", maxPage)}
		}
		limit = *q.Limit
	}
	teamLabel := ""
	if q.For != nil && *q.For != "" {
		l, ok := a.Config.teamLabelFor(*q.For)
		if !ok {
			return nil, &Refusal{Code: ExitUsage, Msg: "no team in the config is led by " + strconv.Quote(*q.For)}
		}
		teamLabel = l
	}
	var after *cursorKey
	if q.Cursor != nil && *q.Cursor != "" {
		k, err := decodeCursor(*q.Cursor)
		if err != nil {
			return nil, err
		}
		after = &k
	}
	var all []Issue
	for _, repo := range a.Config.Repositories {
		issues, err := a.Client.ReadIssues(ctx, repo, []string{"OPEN"}, "", a.Config.maxPages())
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			if a.Config.passes(is.Labels, teamLabel) {
				all = append(all, is)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return lessCreated(all[i], all[j]) })
	start := 0
	if after != nil {
		for start < len(all) && !after.before(all[start]) {
			start++
		}
	}
	rest := all[start:]
	more := len(rest) > limit
	if more {
		rest = rest[:limit]
	}
	out := pageOut{Items: make([]itemOut, 0, len(rest))}
	for _, is := range rest {
		if err := checkExternalID(is.URL); err != nil {
			return nil, err
		}
		if err := a.inScope(is); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, itemOut{ExternalID: is.URL, Title: cleanText(is.Title, 1024)})
	}
	if more {
		c := encodeCursor(cursorKey{Time: rest[len(rest)-1].CreatedAt, Repo: rest[len(rest)-1].Repo, Number: rest[len(rest)-1].Number})
		out.NextCursor = &c
	}
	return out, nil
}

func (c *Config) teamLabelFor(lead string) (string, bool) {
	for _, t := range c.Teams {
		if strings.EqualFold(t.Lead, strings.TrimSpace(lead)) {
			return t.Label, true
		}
	}
	return "", false
}

// passes applies the label model: excluded labels, the team label, ready.
func (c *Config) passes(labels []string, teamLabel string) bool {
	has := map[string]bool{}
	for _, l := range labels {
		has[strings.ToLower(l)] = true
	}
	for _, l := range c.ExcludeLabels {
		if has[strings.ToLower(l)] {
			return false
		}
	}
	if teamLabel != "" && !has[strings.ToLower(teamLabel)] {
		return false
	}
	if c.RequireReady && !has[strings.ToLower(c.ReadyLabel)] {
		return false
	}
	return true
}

func lessCreated(a, b Issue) bool {
	if a.CreatedAt != b.CreatedAt {
		ta, tb := parseTime(a.CreatedAt), parseTime(b.CreatedAt)
		if !ta.Equal(tb) {
			return ta.Before(tb)
		}
		return a.CreatedAt < b.CreatedAt
	}
	if a.Repo != b.Repo {
		return a.Repo < b.Repo
	}
	return a.Number < b.Number
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// ---- cursor: an opaque keyset position, never an offset ----

type cursorKey struct {
	Time   string `json:"t"` // the last item's timestamp (created, or updated for changed-since)
	Repo   string `json:"r"` // its repository
	Number int    `json:"n"` // its number
}

func encodeCursor(k cursorKey) string {
	b, _ := json.Marshal(k)
	return "v1." + base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursorKey, error) {
	var k cursorKey
	rest, ok := strings.CutPrefix(s, "v1.")
	if !ok {
		return k, &Refusal{Code: ExitUsage, Msg: "the cursor is not one this adapter issued"}
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || json.Unmarshal(b, &k) != nil || k.Time == "" || k.Repo == "" || k.Number < 1 {
		return k, &Refusal{Code: ExitUsage, Msg: "the cursor is not one this adapter issued"}
	}
	return k, nil
}

// before reports whether the cursor position is strictly before is, in the
// created order (the next page starts at the first item it is before).
func (k cursorKey) before(is Issue) bool {
	return lessCreated(Issue{CreatedAt: k.Time, Repo: k.Repo, Number: k.Number}, is)
}

// ---- mark-tracked, untrack, report ----

type taskIn struct {
	ID        string `json:"id"`
	Handle    string `json:"handle"`
	PortalURL string `json:"portalUrl"`
}

type workerIn struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	Model string `json:"model"`
}

// writeTarget resolves the item a write would touch and refuses unless it is an
// issue (never a pull request: GitHub's label and comment endpoints take both)
// in a configured repository that exists. Every write call goes through it
// first, so a missing item can never be mistaken for an absent label.
func (a *Adapter) writeTarget(ctx context.Context, externalID string) (string, int, error) {
	repo, n, err := a.parseExternalID(externalID)
	if err != nil {
		return "", 0, err
	}
	is, err := a.Client.GetIssue(ctx, repo, n)
	if err != nil {
		return "", 0, err
	}
	// GitHub follows a moved or renamed repository: write only to the issue the
	// lookup really resolved, and only if it is still one the config lists.
	if got, gn, ok := repoOfIssueURL(is.URL); !ok || gn != n || !strings.EqualFold(got, repo) {
		return "", 0, &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("%s#%d now resolves to %s, which is not the listed repository; it is not written", repo, n, cleanText(is.URL, 200))}
	}
	return repo, n, nil
}

func (a *Adapter) markTracked(ctx context.Context, q *request) (any, error) {
	var task taskIn
	if len(q.Task) == 0 || json.Unmarshal(q.Task, &task) != nil || task.ID == "" {
		return nil, &Refusal{Code: ExitUsage, Msg: "mark-tracked needs the task (its id)"}
	}
	if !markerID.MatchString(task.ID) { // before any write: a comment the reader cannot recognise would be created again on every retry
		return nil, &Refusal{Code: ExitUsage, Msg: "the task id has characters a tracked marker cannot hold (letters, digits and . _ : - only, up to 128)"}
	}
	var worker workerIn
	if len(q.Worker) != 0 && string(q.Worker) != "null" {
		if err := json.Unmarshal(q.Worker, &worker); err != nil {
			return nil, &Refusal{Code: ExitUsage, Msg: "the worker is not an object of id, name, role and model"}
		}
	}
	if a.Config.TrackedLabel == "" && !a.Config.Comment {
		if _, _, err := a.parseExternalID(q.ExternalID); err != nil { // judged, but nothing to write
			return nil, err
		}
		return map[string]any{}, nil
	}
	repo, n, err := a.writeTarget(ctx, q.ExternalID)
	if err != nil {
		return nil, err
	}
	// Every write is idempotent, so a retry after a failure between them (the
	// label set, the comment not) repairs it: the CLI leaves the claim standing
	// and reports the failed mark, and running mark-tracked again completes it.
	if a.Config.TrackedLabel != "" {
		if err := a.Client.AddLabels(ctx, repo, n, a.Config.TrackedLabel); err != nil {
			return nil, err
		}
	}
	if a.Config.Comment {
		if err := a.upsertComment(ctx, repo, n, trackedComment(task, worker)); err != nil {
			return nil, err
		}
	}
	return map[string]any{}, nil
}

func (a *Adapter) untrack(ctx context.Context, q *request) (any, error) {
	if a.Config.TrackedLabel == "" && !a.Config.Comment {
		if _, _, err := a.parseExternalID(q.ExternalID); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	}
	repo, n, err := a.writeTarget(ctx, q.ExternalID)
	if err != nil {
		return nil, err
	}
	if a.Config.TrackedLabel != "" {
		if err := a.Client.RemoveLabel(ctx, repo, n, a.Config.TrackedLabel); err != nil { // the issue exists: a 404 is the absent label
			return nil, err
		}
	}
	if a.Config.Comment {
		own, err := a.ownComments(ctx, repo, n)
		if err != nil {
			return nil, err
		}
		for _, c := range own {
			if err := a.Client.UpdateComment(ctx, repo, c.ID, commentMarker+"released -->\nNo longer tracked in Hadron: the task was released."); err != nil {
				return nil, err
			}
		}
	}
	return map[string]any{}, nil
}

func (a *Adapter) report(ctx context.Context, q *request) (any, error) {
	act, ok := a.Config.Report[strings.ToUpper(strings.TrimSpace(q.State))] // keys are normalized at load
	if !ok {
		if _, _, err := a.parseExternalID(q.ExternalID); err != nil {
			return nil, err
		}
		return map[string]any{}, nil // no action configured for this state
	}
	repo, n, err := a.writeTarget(ctx, q.ExternalID)
	if err != nil {
		return nil, err
	}
	if err := a.Client.AddLabels(ctx, repo, n, act.AddLabels...); err != nil {
		return nil, err
	}
	for _, l := range act.RemoveLabels {
		if err := a.Client.RemoveLabel(ctx, repo, n, l); err != nil {
			return nil, err
		}
	}
	return map[string]any{}, nil
}

func trackedComment(t taskIn, w workerIn) string {
	var b strings.Builder
	b.WriteString(commentMarker + cleanText(t.ID, 128) + " -->\n")
	b.WriteString("Tracked in Hadron by ")
	name := cleanText(w.Name, 128)
	if name == "" {
		name = "a worker"
	}
	b.WriteString(name)
	if w.Role != "" {
		b.WriteString(" (" + cleanText(w.Role, 64) + ")")
	}
	b.WriteString(".")
	label := cleanText(t.Handle, 64)
	if label == "" {
		label = cleanText(t.ID, 128)
	}
	if u := safeHTTPS(t.PortalURL); u != "" {
		b.WriteString("\nTask: [" + label + "](" + u + ")")
	} else {
		b.WriteString("\nTask: " + label)
	}
	return b.String()
}

// safeHTTPS returns u when it is an https URL without userinfo or whitespace.
func safeHTTPS(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil || strings.ContainsAny(u, " \t\r\n()<>") {
		return ""
	}
	return u
}

// ownLogin is the account whose comments the adapter may edit: the config's
// commentAuthor, else the token's own login.
func (a *Adapter) ownLogin(ctx context.Context) (string, error) {
	if a.Config.CommentAuthor != "" {
		// Bind the configured identity to the credential on every path that reads
		// or writes the adapter's comments (create, edit, release, delete): with a
		// token that names its account the two must agree, or the adapter would
		// edit another account's marker comments. An installation token names no
		// account (GET /user is refused), so the configured login is trusted there
		// and the author of a created comment is checked instead.
		if actual, lerr := a.Client.Login(ctx); lerr == nil && !strings.EqualFold(actual, a.Config.CommentAuthor) {
			return "", &Refusal{Code: ExitRefused, Msg: "commentAuthor " + strconv.Quote(cleanText(a.Config.CommentAuthor, 64)) + " is not the credential's account " + strconv.Quote(cleanText(actual, 64)) + "; nothing was changed"}
		}
		return a.Config.CommentAuthor, nil
	}
	login, err := a.Client.Login(ctx)
	if err != nil {
		return "", &Refusal{Code: ExitRefused, Msg: "comment needs to know which comments are the adapter's own, and GET /user did not name the token's account; set commentAuthor in the config (for a GitHub App installation token, its bot login)"}
	}
	return login, nil
}

// ownComments are the comments that start with a valid marker line AND were
// written by the adapter's own account, oldest first. A marker in someone
// else's comment is only text.
func (a *Adapter) ownComments(ctx context.Context, repo string, n int) ([]Comment, error) {
	login, err := a.ownLogin(ctx)
	if err != nil {
		return nil, err
	}
	all, err := a.Client.ListComments(ctx, repo, n, 10)
	if err != nil {
		return nil, err
	}
	var own []Comment
	for _, c := range all {
		if strings.EqualFold(c.User.Login, login) && markerLine.MatchString(c.Body) {
			own = append(own, c)
		}
	}
	sort.Slice(own, func(i, j int) bool { return own[i].ID < own[j].ID })
	return own, nil
}

// upsertComment keeps exactly one adapter comment on the issue: it edits the
// oldest own comment, or creates one. After a create it re-reads and, if a
// concurrent mark created another, keeps the oldest and deletes the extras
// (its own comments only), so two simultaneous marks end with one comment.
func (a *Adapter) upsertComment(ctx context.Context, repo string, n int, body string) error {
	own, err := a.ownComments(ctx, repo, n)
	if err != nil {
		return err
	}
	if len(own) == 0 {
		login, err := a.ownLogin(ctx)
		if err != nil {
			return err
		}
		created, err := a.Client.CreateComment(ctx, repo, n, body)
		if err != nil {
			return err
		}
		if created.User.Login != "" && !strings.EqualFold(created.User.Login, login) {
			who := "the comment was posted as " + strconv.Quote(cleanText(created.User.Login, 64)) + ", not commentAuthor " + strconv.Quote(cleanText(login, 64))
			// undo it: it would not be recognised on a retry, which would post another
			if derr := a.Client.DeleteComment(ctx, repo, created.ID); derr != nil {
				return &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("%s, and it could not be removed (%s): delete comment %d on %s#%d by hand, then set commentAuthor to the credential's login", who, cleanText(a.Client.scrub(derr.Error()), 200), created.ID, repo, n)}
			}
			return &Refusal{Code: ExitRefused, Msg: who + "; it was removed. Set commentAuthor to the credential's login"}
		}
		own, err = a.ownComments(ctx, repo, n)
		var lim commentLimit
		if errors.As(err, &lim) { // the comment list is past the scan bound, but the write is known: GitHub named it
			own, err = []Comment{created}, nil
		}
		if err != nil {
			return err
		}
	}
	if len(own) == 0 {
		return &Refusal{Code: ExitRefused, Msg: "the comment was written but could not be read back"}
	}
	if err := a.Client.UpdateComment(ctx, repo, own[0].ID, body); err != nil {
		return err
	}
	for _, extra := range own[1:] {
		if err := a.Client.DeleteComment(ctx, repo, extra.ID); err != nil {
			return err
		}
	}
	return nil
}

// ---- get, normalize, check ----

func (a *Adapter) get(ctx context.Context, q *request) (any, error) {
	repo, n, err := a.parseExternalID(q.ExternalID)
	if err != nil {
		return nil, err
	}
	is, err := a.Client.GetIssue(ctx, repo, n)
	if err != nil {
		return nil, err
	}
	if err := a.inScope(is); err != nil {
		return nil, err
	}
	body, truncated := truncateRunes(is.Body, maxBodyRunes)
	return map[string]any{"externalId": is.URL, "title": cleanText(is.Title, 1024), "body": body, "bodyTruncated": truncated, "state": is.State, "url": is.URL}, nil
}

func (a *Adapter) normalize(ctx context.Context, q *request) (any, error) {
	repo, n, err := a.parseReference(q.Reference)
	if err != nil {
		return nil, err
	}
	// The canonical form is the URL GitHub itself gives the issue (its real
	// capitalisation), and the lookup also proves it is an issue.
	is, err := a.Client.GetIssue(ctx, repo, n)
	if err != nil {
		return nil, err
	}
	if err := checkExternalID(is.URL); err != nil {
		return nil, err
	}
	if err := a.inScope(is); err != nil {
		return nil, err
	}
	return map[string]any{"externalId": is.URL}, nil
}

func (a *Adapter) check(ctx context.Context) (any, error) {
	reachable, accepted, err := a.Client.Ping(ctx)
	out := map[string]any{"credentialPresent": a.Client.Token != "", "credentialValid": accepted, "reachable": reachable, "configValid": a.Config != nil}
	if err != nil {
		out["detail"] = cleanText(a.Client.scrub(err.Error()), 512)
	}
	if a.ConfigError != "" {
		out["configDetail"] = cleanText(a.Client.scrub(a.ConfigError), 512)
	}
	return out, nil
}

func (a *Adapter) changedSince(ctx context.Context, q *request) (any, error) {
	if _, err := time.Parse(time.RFC3339, q.Since); err != nil {
		return nil, &Refusal{Code: ExitUsage, Msg: "changed-since needs since as an RFC 3339 time"}
	}
	limit := defaultPage
	if q.Limit != nil {
		if *q.Limit < 1 || *q.Limit > maxPage {
			return nil, &Refusal{Code: ExitUsage, Msg: fmt.Sprintf("limit must be 1 to %d", maxPage)}
		}
		limit = *q.Limit
	}
	var after *cursorKey
	if q.Cursor != nil && *q.Cursor != "" {
		k, err := decodeCursor(*q.Cursor)
		if err != nil {
			return nil, err
		}
		after = &k
	}
	var all []Issue
	for _, repo := range a.Config.Repositories {
		issues, err := a.Client.ReadIssues(ctx, repo, []string{"OPEN", "CLOSED"}, q.Since, a.Config.maxPages())
		if err != nil {
			return nil, err
		}
		all = append(all, issues...)
	}
	sort.Slice(all, func(i, j int) bool {
		x, y := all[i], all[j]
		if x.UpdatedAt != y.UpdatedAt {
			tx, ty := parseTime(x.UpdatedAt), parseTime(y.UpdatedAt)
			if !tx.Equal(ty) {
				return tx.Before(ty)
			}
			return x.UpdatedAt < y.UpdatedAt
		}
		if x.Repo != y.Repo {
			return x.Repo < y.Repo
		}
		return x.Number < y.Number
	})
	start := 0
	if after != nil {
		for start < len(all) && !updatedAfter(*after, all[start]) {
			start++
		}
	}
	rest := all[start:]
	more := len(rest) > limit
	if more {
		rest = rest[:limit]
	}
	type changed struct {
		ExternalID string `json:"externalId"`
		Title      string `json:"title"`
		State      string `json:"state"`
		UpdatedAt  string `json:"updatedAt"`
	}
	out := struct {
		Items      []changed `json:"items"`
		NextCursor *string   `json:"nextCursor"`
	}{Items: make([]changed, 0, len(rest))}
	for _, is := range rest {
		if err := a.inScope(is); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, changed{ExternalID: is.URL, Title: cleanText(is.Title, 1024), State: is.State, UpdatedAt: is.UpdatedAt})
	}
	if more {
		c := encodeCursor(cursorKey{Time: rest[len(rest)-1].UpdatedAt, Repo: rest[len(rest)-1].Repo, Number: rest[len(rest)-1].Number})
		out.NextCursor = &c
	}
	return out, nil
}

func updatedAfter(k cursorKey, is Issue) bool {
	if k.Time != is.UpdatedAt {
		tk, ti := parseTime(k.Time), parseTime(is.UpdatedAt)
		if !tk.Equal(ti) {
			return tk.Before(ti)
		}
		return k.Time < is.UpdatedAt
	}
	if k.Repo != is.Repo {
		return k.Repo < is.Repo
	}
	return k.Number < is.Number
}

// ---- external ids and references ----

// parseExternalID reads the canonical issue URL the adapter itself issued.
func (a *Adapter) parseExternalID(id string) (repo string, n int, err error) {
	if err := checkExternalID(id); err != nil {
		return "", 0, &Refusal{Code: ExitUsage, Msg: "externalId is not a valid id: " + err.Error()}
	}
	repo, n, ok := a.issueFromURL(id)
	if !ok {
		return "", 0, &Refusal{Code: ExitUsage, Msg: "externalId is not an issue URL on " + a.host()}
	}
	if !a.Config.inRepositories(repo) {
		return "", 0, &Refusal{Code: ExitUsage, Msg: "externalId names a repository the config does not list"}
	}
	return strings.ToLower(repo), n, nil
}

func (a *Adapter) host() string {
	if a.Host != "" {
		return a.Host
	}
	return "github.com"
}

func (a *Adapter) issueFromURL(raw string) (repo string, n int, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || !strings.EqualFold(strings.TrimPrefix(u.Host, "www."), a.host()) {
		return "", 0, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "issues" {
		return "", 0, false
	}
	num, err := strconv.Atoi(parts[3])
	if err != nil || num < 1 {
		return "", 0, false
	}
	return parts[0] + "/" + parts[1], num, true
}

// parseReference reads a short or sloppy reference: an issue URL (any case,
// http, www, a trailing slash, a query or fragment), owner/repo#N, or #N with a
// defaultRepository.
func (a *Adapter) parseReference(ref string) (string, int, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", 0, &Refusal{Code: ExitUsage, Msg: "normalize needs a reference"}
	}
	repo, numText, hash := "", "", strings.LastIndex(ref, "#")
	switch {
	case strings.Contains(ref, "://"):
		u, err := url.Parse(ref)
		if err != nil || u.User != nil {
			return "", 0, &Refusal{Code: ExitUsage, Msg: "the reference is not an issue URL"}
		}
		u.Scheme = "https"
		u.RawQuery, u.Fragment = "", ""
		r, n, ok := a.issueFromURL(u.String())
		if !ok {
			return "", 0, &Refusal{Code: ExitUsage, Msg: "the reference is not an issue URL on " + a.host()}
		}
		repo, numText = r, strconv.Itoa(n)
	case hash == 0:
		if a.Config.DefaultRepository == "" {
			return "", 0, &Refusal{Code: ExitUsage, Msg: "a bare #N needs defaultRepository in the config"}
		}
		repo, numText = a.Config.DefaultRepository, ref[1:]
	case hash > 0:
		repo, numText = ref[:hash], ref[hash+1:]
	default:
		return "", 0, &Refusal{Code: ExitUsage, Msg: "the reference is not owner/repo#N, #N or an issue URL"}
	}
	n, err := strconv.Atoi(numText)
	if err != nil || n < 1 || !validRepo(strings.ToLower(repo)) {
		return "", 0, &Refusal{Code: ExitUsage, Msg: "the reference is not owner/repo#N, #N or an issue URL"}
	}
	repo = strings.ToLower(repo)
	if !a.Config.inRepositories(repo) {
		return "", 0, &Refusal{Code: ExitUsage, Msg: "the reference names a repository the config does not list"}
	}
	return repo, n, nil
}

// checkExternalID applies the id rules the server applies (cor:agt:120:02): 1 to
// 512 code points, no whitespace, control or invisible format characters, no
// userinfo. The adapter never returns an id that fails them.
func checkExternalID(id string) error {
	if id == "" {
		return errors.New("empty")
	}
	if utf8.RuneCountInString(id) > maxExternalID {
		return fmt.Errorf("longer than %d characters", maxExternalID)
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return errors.New("holds whitespace or a control or invisible character")
		}
	}
	if u, err := url.Parse(id); err == nil && u.User != nil {
		return errors.New("holds userinfo")
	}
	return nil
}

// cleanText makes tracker text safe to show: control and format characters
// become spaces, and it is cut at max bytes on a rune boundary.
func cleanText(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			r = ' '
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func truncateRunes(s string, max int) (string, bool) {
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	r := []rune(s)
	return string(r[:max]), true
}

// repoOfIssueURL reads "owner/name" and the number out of an issue's web URL
// (https://host/owner/name/issues/N).
func repoOfIssueURL(raw string) (repo string, n int, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, false
	}
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(p) < 4 || p[len(p)-2] != "issues" {
		return "", 0, false
	}
	n, err = strconv.Atoi(p[len(p)-1])
	if err != nil {
		return "", 0, false
	}
	return p[len(p)-4] + "/" + p[len(p)-3], n, true
}

// checkWithoutClient is check's answer when the client could not be built: the
// setup problem and, independently, whether the config is valid.
func (a *Adapter) checkWithoutClient() map[string]any {
	out := map[string]any{"credentialPresent": a.CredentialAsked, "credentialValid": false, "reachable": false, "configValid": a.Config != nil, "detail": cleanText(ScrubSecret(a.ClientError, a.Secret), 512)}
	if a.ConfigError != "" {
		out["configDetail"] = cleanText(ScrubSecret(a.ConfigError, a.Secret), 512)
	}
	return out
}

// inScope refuses an issue whose own URL is not on the configured host and
// repository (GitHub resolved a moved or renamed repository, or answered a
// different issue): such an issue is neither shown nor written.
func (a *Adapter) inScope(is Issue) error {
	repo, n, ok := a.issueFromURL(is.URL)
	if !ok || n != is.Number || !strings.EqualFold(repo, is.Repo) || !a.Config.inRepositories(repo) {
		return &Refusal{Code: ExitRefused, Msg: fmt.Sprintf("%s#%d is at %s, outside the configured repositories; update the config", is.Repo, is.Number, cleanText(is.URL, 200))}
	}
	return nil
}
