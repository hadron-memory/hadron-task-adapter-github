package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	issuesPerRequest = 100
	labelsPerIssue   = 100
	maxLabelPages    = 10 // REST fallback: 1000 labels on one issue
	// A call reads at most this many GitHub pages beyond the first of each
	// repository, then answers with what it has and a cursor. Filtering
	// happens here, so a run of ineligible issues costs reads, not items.
	extraFetchesPerCall = 20
	cursorPrefix        = "v1."
	// MaxCursorLen is the hadron CLI runner's bound on nextCursor.
	MaxCursorLen = 4096
)

const listQuery = `query ListOpenIssues($owner:String!,$name:String!,$after:String,$first:Int!,$labelsFirst:Int!){
  repository(owner:$owner,name:$name){
    issues(states:OPEN,first:$first,after:$after,orderBy:{field:CREATED_AT,direction:ASC}){
      pageInfo{hasNextPage}
      edges{cursor node{number title url createdAt labels(first:$labelsFirst){nodes{name} pageInfo{hasNextPage}}}}
    }
  }
}`

type labelConn struct {
	Nodes    []struct{ Name string } `json:"nodes"`
	PageInfo struct {
		HasNextPage bool `json:"hasNextPage"`
	} `json:"pageInfo"`
}

type issueNode struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"createdAt"`
	Labels    labelConn `json:"labels"`
}

type issueEdge struct {
	Cursor string    `json:"cursor"`
	Node   issueNode `json:"node"`
}

// repoWalk is the position in one repository's open issues, oldest first.
type repoWalk struct {
	repo
	after   string // GitHub's cursor after the last issue consumed; "" = the start
	done    bool
	fetched bool
	more    bool
	buf     []issueEdge
}

// cursorState is what the adapter's opaque cursor carries: a fingerprint of
// the query (the configuration's filters, its repositories and `for`), and
// per configured repository GitHub's own cursor, or null once it is done.
type cursorState struct {
	Query string    `json:"q"`
	Pos   []*string `json:"p"`
}

func (c *Config) fingerprint(forWorker string) string {
	b, _ := json.Marshal([]any{c.Repositories, c.ReadyLabel, c.Ready, c.ExcludeLabels, c.Teams, c.TrackedLabel, strings.ToLower(forWorker)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func encodeCursor(s cursorState) string {
	b, _ := json.Marshal(s)
	return cursorPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(raw string, want string, n int) (cursorState, error) {
	bad := errors.New("the cursor is not one this adapter issued")
	if !strings.HasPrefix(raw, cursorPrefix) {
		return cursorState{}, bad
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, cursorPrefix))
	if err != nil {
		return cursorState{}, bad
	}
	var s cursorState
	if err := json.Unmarshal(b, &s); err != nil || len(s.Pos) != n {
		return cursorState{}, bad
	}
	if s.Query != want {
		return cursorState{}, errors.New("the cursor was issued for another query or before the configuration changed; start the list again")
	}
	return s, nil
}

// lister lists untracked issues.
type lister struct {
	cfg *Config
	gh  *githubClient
}

func (l *lister) fetch(ctx context.Context, w *repoWalk) error {
	vars := map[string]any{"owner": w.Owner, "name": w.Name, "first": issuesPerRequest, "labelsFirst": labelsPerIssue}
	if w.after != "" {
		vars["after"] = w.after
	}
	var data struct {
		Repository *struct {
			Issues *struct {
				PageInfo struct {
					HasNextPage bool `json:"hasNextPage"`
				} `json:"pageInfo"`
				Edges []issueEdge `json:"edges"`
			} `json:"issues"`
		} `json:"repository"`
	}
	if err := l.gh.graphql(ctx, "ListOpenIssues", listQuery, vars, &data); err != nil {
		return fmt.Errorf("reading %s/%s: %w", w.Owner, w.Name, err)
	}
	if data.Repository == nil || data.Repository.Issues == nil {
		return fmt.Errorf("reading %s/%s: GitHub returned no issue list", w.Owner, w.Name)
	}
	iss := data.Repository.Issues
	for _, e := range iss.Edges {
		if e.Cursor == "" || e.Node.URL == "" || e.Node.Number <= 0 || e.Node.CreatedAt.IsZero() {
			return fmt.Errorf("reading %s/%s: GitHub returned an incomplete issue", w.Owner, w.Name)
		}
	}
	w.buf, w.more, w.fetched = iss.Edges, iss.PageInfo.HasNextPage, true
	// An empty page that still promises more would never make progress.
	if len(w.buf) == 0 && w.more {
		return fmt.Errorf("reading %s/%s: GitHub returned an empty page that promises more", w.Owner, w.Name)
	}
	return nil
}

// labels returns all of an issue's labels, reading past the first page.
func (l *lister) labels(ctx context.Context, r repo, n issueNode) ([]string, error) {
	out := make([]string, 0, len(n.Labels.Nodes))
	if !n.Labels.PageInfo.HasNextPage {
		for _, x := range n.Labels.Nodes {
			out = append(out, x.Name)
		}
		return out, nil
	}
	for page := 1; ; page++ {
		if page > maxLabelPages {
			return nil, fmt.Errorf("%s/%s#%d carries more than %d labels", r.Owner, r.Name, n.Number, maxLabelPages*100)
		}
		var got []struct{ Name string }
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels?per_page=100&page=%d", escapePath(r.Owner), escapePath(r.Name), n.Number, page)
		if _, err := l.gh.rest(ctx, http.MethodGet, path, nil, &got, http.StatusOK); err != nil {
			return nil, fmt.Errorf("reading the labels of %s/%s#%d: %w", r.Owner, r.Name, n.Number, err)
		}
		for _, x := range got {
			out = append(out, x.Name)
		}
		if len(got) < 100 {
			return out, nil
		}
	}
}

// eligible applies the label model to one issue (labels compared without
// regard to case).
func (l *lister) eligible(labels []string, t *Team) bool {
	has := map[string]bool{}
	for _, x := range labels {
		has[strings.ToLower(x)] = true
	}
	if has[strings.ToLower(l.cfg.TrackedLabel)] {
		return false
	}
	for _, x := range l.cfg.ExcludeLabels {
		if has[strings.ToLower(x)] {
			return false
		}
	}
	if t != nil {
		if t.NoTeam {
			for _, other := range l.cfg.Teams {
				if !other.NoTeam && has[strings.ToLower(other.Label)] {
					return false
				}
			}
		} else if !has[strings.ToLower(t.Label)] {
			return false
		}
	}
	ready := l.cfg.ReadyLabel != "" && has[strings.ToLower(l.cfg.ReadyLabel)]
	switch l.cfg.Ready {
	case "only":
		return ready
	case "exclude":
		return !ready
	}
	return true
}

// before is the list's order: oldest first, then repository, then number.
func before(a, b *repoWalk) bool {
	x, y := a.buf[0].Node, b.buf[0].Node
	if !x.CreatedAt.Equal(y.CreatedAt) {
		return x.CreatedAt.Before(y.CreatedAt)
	}
	if a.Key != b.Key {
		return a.Key < b.Key
	}
	return x.Number < y.Number
}

type listedItem struct {
	ExternalID string `json:"externalId"`
	Title      string `json:"title"`
}

// list answers one list-untracked page: up to limit eligible issues in the
// list's order, and the cursor to resume after the last issue consumed (nil
// when every repository is done).
//
// The set of open issues does not shrink when an issue is marked: the mark is
// a label, and filtering happens here, after GitHub's paging. So a cursor
// taken before others marked issues still resumes at the right place.
func (l *lister) list(ctx context.Context, cursor, forWorker string, limit int) ([]listedItem, *string, error) {
	team, ok := l.cfg.team(forWorker)
	if !ok {
		return nil, nil, usageErr("no team in the adapter's configuration is mapped to the worker %q", l.gh.clip(forWorker, 80))
	}
	repos := l.cfg.repos()
	fp := l.cfg.fingerprint(forWorker)
	walks := make([]*repoWalk, len(repos))
	for i, r := range repos {
		walks[i] = &repoWalk{repo: r}
	}
	if cursor != "" {
		s, err := decodeCursor(cursor, fp, len(repos))
		if err != nil {
			return nil, nil, usageErr("%v", err)
		}
		for i, p := range s.Pos {
			if p == nil {
				walks[i].done = true
			} else {
				walks[i].after = *p
			}
		}
	}
	items := []listedItem{}
	budget := len(walks) + extraFetchesPerCall
	stopped := false
	for len(items) < limit {
		var head *repoWalk
		for _, w := range walks {
			if w.done {
				continue
			}
			if len(w.buf) == 0 {
				if w.fetched && !w.more {
					w.done = true
					continue
				}
				if budget == 0 {
					stopped = true
					break
				}
				budget--
				if err := l.fetch(ctx, w); err != nil {
					return nil, nil, err
				}
				if len(w.buf) == 0 {
					w.done = true
					continue
				}
			}
			if head == nil || before(w, head) {
				head = w
			}
		}
		// Out of reads before every repository showed its next issue: the
		// order cannot be decided, so answer with what is decided.
		if stopped || head == nil {
			break
		}
		e := head.buf[0]
		head.buf = head.buf[1:]
		head.after = e.Cursor
		if len(head.buf) == 0 && !head.more {
			head.done = true
		}
		labels, err := l.labels(ctx, head.repo, e.Node)
		if err != nil {
			return nil, nil, err
		}
		if !l.eligible(labels, team) {
			continue
		}
		id, err := l.externalID(head.repo, e.Node)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, listedItem{ExternalID: id, Title: boundTitle(e.Node.Title)})
	}
	allDone := true
	pos := make([]*string, len(walks))
	for i, w := range walks {
		if !w.done {
			allDone = false
			a := w.after
			pos[i] = &a
		}
	}
	if allDone {
		return items, nil, nil
	}
	next := encodeCursor(cursorState{Query: fp, Pos: pos})
	// The runner refuses a cursor past its bound; GitHub does not promise
	// how long its positions are. Never answer with one that cannot be used.
	if len(next) > MaxCursorLen {
		return nil, nil, fmt.Errorf("the cursor would be %d bytes, more than the %d the hadron CLI accepts (GitHub's positions are unusually long); configure fewer repositories", len(next), MaxCursorLen)
	}
	return items, &next, nil
}

// externalID is the issue's canonical URL as GitHub prints it, checked to be
// an issue URL of the repository it was read from and a valid external id.
func (l *lister) externalID(r repo, n issueNode) (string, error) {
	ref, err := parseIssueURL(n.URL, l.cfg.WebHost)
	if err != nil || ref.Key != r.Key || ref.Number != n.Number {
		return "", fmt.Errorf("GitHub returned %q for %s/%s#%d, which is not that issue's URL", l.gh.clip(n.URL, 120), r.Owner, r.Name, n.Number)
	}
	if err := checkExternalID(n.URL); err != nil {
		return "", fmt.Errorf("GitHub returned an issue URL that is not a valid external id: %v", err)
	}
	return n.URL, nil
}
