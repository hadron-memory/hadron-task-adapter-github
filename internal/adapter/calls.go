package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// configuredIssue reads an external id the CLI hands back and checks it names
// an issue of a configured repository. The adapter acts only on the
// repositories its configuration lists.
func (s *session) configuredIssue(call, externalID string) (issueRef, error) {
	if externalID == "" {
		return issueRef{}, usageErr("%s needs an externalId", call)
	}
	ref, err := parseIssueURL(externalID, s.cfg.WebHost)
	if err != nil {
		return issueRef{}, usageErr("%s: %q: %v", call, s.gh.clip(externalID, 120), err)
	}
	if !s.cfg.hasRepo(ref.Owner, ref.Name) {
		return issueRef{}, usageErr("%s: %s/%s is not a repository in the adapter's configuration", call, ref.Owner, ref.Name)
	}
	return ref, nil
}

func issuePath(ref issueRef, rest string) string {
	return fmt.Sprintf("/repos/%s/%s/issues/%d%s", escapePath(ref.Owner), escapePath(ref.Name), ref.Number, rest)
}

type markRequest struct {
	ExternalID string `json:"externalId"`
	Task       struct {
		ID        string `json:"id"`
		Handle    string `json:"handle"`
		PortalURL string `json:"portalUrl"`
	} `json:"task"`
	Worker struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Role  string `json:"role"`
		Model string `json:"model"`
	} `json:"worker"`
}

// markTracked puts the tracked label on the issue, and when the
// configuration asks for it, a comment naming the task and the worker. It
// never refuses once the label is on: a failed comment is reported as
// commented:false with a commentError. It never refuses because the issue
// is already marked: the mark shows a claim, it never makes or decides one
// (cor:agt:120, D14). A re-mark after a reassignment is an ordinary call.
func (s *session) markTracked(ctx context.Context, req json.RawMessage) (any, error) {
	var r markRequest
	if err := decode(req, "mark-tracked", &r); err != nil {
		return nil, err
	}
	ref, err := s.configuredIssue("mark-tracked", r.ExternalID)
	if err != nil {
		return nil, err
	}
	if r.Task.ID == "" {
		return nil, usageErr("mark-tracked needs the task's id")
	}
	body := map[string][]string{"labels": {s.cfg.TrackedLabel}}
	if _, err := s.gh.rest(ctx, http.MethodPost, issuePath(ref, "/labels"), body, nil, http.StatusOK); err != nil {
		return nil, fmt.Errorf("labelling %s/%s#%d: %w", ref.Owner, ref.Name, ref.Number, err)
	}
	ans := map[string]any{"marked": true, "externalId": r.ExternalID, "label": s.cfg.TrackedLabel, "commented": false}
	if s.cfg.Comment {
		if _, err := s.gh.rest(ctx, http.MethodPost, issuePath(ref, "/comments"), map[string]string{"body": markComment(r)}, nil, http.StatusCreated); err != nil {
			// The label is the mark, and it is on: the issue no longer
			// lists. Refusing now would tell the runner the mark failed
			// while the issue is in fact marked, stranding it outside
			// intake. The comment is decoration, so its failure is
			// reported in a successful answer, scrubbed like a refusal.
			ans["commentError"] = s.gh.scrub(err.Error())
			return ans, nil
		}
		ans["commented"] = true
	}
	return ans, nil
}

// code quotes text from Hadron as inline code on one line, so a name cannot
// mention anyone, link anywhere or change the comment's layout.
func code(s string) string {
	s = strings.ReplaceAll(oneLine(s, 120), "`", "'")
	if s == "" {
		return ""
	}
	return "`" + s + "`"
}

func markComment(r markRequest) string {
	var b strings.Builder
	b.WriteString("Tracked in Hadron")
	task := r.Task.Handle
	if task == "" {
		task = r.Task.ID
	}
	fmt.Fprintf(&b, " as task %s", code(task))
	// The portal URL is the server's, passed as printed (cor:agt:120:04);
	// the adapter builds none, and links only a plain https URL.
	if u := r.Task.PortalURL; strings.HasPrefix(u, "https://") && checkExternalID(u) == nil && !strings.ContainsAny(u, "()[]<>`") {
		fmt.Fprintf(&b, " (%s)", u)
	}
	if r.Worker.Name != "" {
		fmt.Fprintf(&b, ", by %s", code(r.Worker.Name))
		if r.Worker.Role != "" {
			fmt.Fprintf(&b, " (%s)", code(r.Worker.Role))
		}
	}
	b.WriteString(".")
	return b.String()
}

// untrack removes the tracked label. An issue that carries no mark is not an
// error: the outcome the caller wants already holds.
func (s *session) untrack(ctx context.Context, req json.RawMessage) (any, error) {
	var r struct {
		ExternalID string `json:"externalId"`
	}
	if err := decode(req, "untrack", &r); err != nil {
		return nil, err
	}
	ref, err := s.configuredIssue("untrack", r.ExternalID)
	if err != nil {
		return nil, err
	}
	status, err := s.gh.rest(ctx, http.MethodDelete, issuePath(ref, "/labels/"+escapePath(s.cfg.TrackedLabel)), nil, nil, http.StatusOK)
	if err != nil {
		var ae *apiError
		// GitHub answers 404 "Label does not exist" for a label the issue
		// does not carry. Any other 404 (no such issue, no access) is an
		// error.
		noMark := errors.As(err, &ae) && status == http.StatusNotFound && strings.Contains(strings.ToLower(ae.Message), "label does not exist")
		if !noMark {
			return nil, fmt.Errorf("unlabelling %s/%s#%d: %w", ref.Owner, ref.Name, ref.Number, err)
		}
	}
	return map[string]any{"untracked": true, "externalId": r.ExternalID}, nil
}

const getQuery = `query GetIssue($owner:String!,$name:String!,$number:Int!,$labelsFirst:Int!){
  repository(owner:$owner,name:$name){
    issue(number:$number){number title body state url labels(first:$labelsFirst){nodes{name} pageInfo{hasNextPage}}}
  }
}`

type issueDetail struct {
	Number int       `json:"number"`
	Title  string    `json:"title"`
	Body   string    `json:"body"`
	State  string    `json:"state"`
	URL    string    `json:"url"`
	Labels labelConn `json:"labels"`
}

func (s *session) readIssue(ctx context.Context, ref issueRef) (*issueDetail, error) {
	var data struct {
		Repository *struct {
			Issue *issueDetail `json:"issue"`
		} `json:"repository"`
	}
	err := s.gh.graphql(ctx, "GetIssue", getQuery, map[string]any{"owner": ref.Owner, "name": ref.Name, "number": ref.Number, "labelsFirst": labelsPerIssue}, &data)
	var nf *notFoundError
	if errors.As(err, &nf) || err == nil && (data.Repository == nil || data.Repository.Issue == nil) {
		return nil, fmt.Errorf("%s/%s#%d: no such issue, or the credential cannot read it", ref.Owner, ref.Name, ref.Number)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s/%s#%d: %w", ref.Owner, ref.Name, ref.Number, err)
	}
	return data.Repository.Issue, nil
}

// get returns one item. Its externalId is the one asked about, as given;
// url is GitHub's canonical URL now (it differs after a transfer or a rename).
func (s *session) get(ctx context.Context, req json.RawMessage) (any, error) {
	var r struct {
		ExternalID string `json:"externalId"`
	}
	if err := decode(req, "get", &r); err != nil {
		return nil, err
	}
	ref, err := s.configuredIssue("get", r.ExternalID)
	if err != nil {
		return nil, err
	}
	is, err := s.readIssue(ctx, ref)
	if err != nil {
		return nil, err
	}
	labels, err := (&lister{cfg: s.cfg, gh: s.gh}).labels(ctx, repo{Owner: ref.Owner, Name: ref.Name, Key: ref.Key}, issueNode{Number: is.Number, Labels: is.Labels})
	if err != nil {
		return nil, err
	}
	tracked := false
	for _, l := range labels {
		tracked = tracked || strings.EqualFold(l, s.cfg.TrackedLabel)
	}
	// Labels themselves are not returned: tracker labels never reach Hadron
	// (cor:agt:120, D7).
	return map[string]any{"item": map[string]any{
		"externalId": r.ExternalID,
		"title":      boundTitle(is.Title),
		"body":       is.Body,
		"state":      strings.ToLower(is.State),
		"url":        is.URL,
		"tracked":    tracked,
	}}, nil
}

// normalize turns a short reference into the issue's canonical external id,
// as GitHub prints it.
func (s *session) normalize(ctx context.Context, req json.RawMessage) (any, error) {
	var r struct {
		Ref string `json:"ref"`
	}
	if err := decode(req, "normalize", &r); err != nil {
		return nil, err
	}
	ref, err := s.cfg.parseShortRef(r.Ref)
	if err != nil {
		return nil, usageErr("normalize: %q: %v", s.gh.clip(r.Ref, 120), err)
	}
	if !s.cfg.hasRepo(ref.Owner, ref.Name) {
		return nil, usageErr("normalize: %s/%s is not a repository in the adapter's configuration", ref.Owner, ref.Name)
	}
	is, err := s.readIssue(ctx, ref)
	if err != nil {
		return nil, err
	}
	l := &lister{cfg: s.cfg}
	id, err := l.externalID(repo{Owner: ref.Owner, Name: ref.Name, Key: ref.Key}, issueNode{Number: is.Number, URL: is.URL})
	if err != nil {
		return nil, err
	}
	return map[string]any{"externalId": id}, nil
}

type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// check says whether the configuration is valid, the credential present and
// accepted, and each repository readable. It answers (exit 0) whatever it
// finds; ok is true only when every check passed.
func check(ctx context.Context, getenv func(string) string) any {
	results := []checkResult{}
	answer := func() any {
		ok := true
		for _, r := range results {
			ok = ok && r.OK
		}
		return map[string]any{"ok": ok, "checks": results}
	}
	cfg, err := LoadConfig(getenv)
	if err != nil {
		results = append(results, checkResult{Name: "configuration", OK: false, Detail: oneLine(err.Error(), 300)})
		return answer()
	}
	token, err := cfg.Token(getenv)
	if err != nil {
		results = append(results, checkResult{Name: "credential", OK: false, Detail: oneLine(err.Error(), 300)})
		return answer()
	}
	gh := newGitHub(cfg, token)
	if _, err := gh.rest(ctx, http.MethodGet, "/rate_limit", nil, nil, http.StatusOK); err != nil {
		results = append(results, checkResult{Name: "credential", OK: false, Detail: gh.clip(err.Error(), 300)})
		return answer()
	}
	results = append(results, checkResult{Name: "credential", OK: true})
	for _, r := range cfg.repos() {
		name := "repository " + r.Owner + "/" + r.Name
		if _, err := gh.rest(ctx, http.MethodGet, "/repos/"+escapePath(r.Owner)+"/"+escapePath(r.Name), nil, nil, http.StatusOK); err != nil {
			results = append(results, checkResult{Name: name, OK: false, Detail: gh.clip(err.Error(), 300)})
			continue
		}
		results = append(results, checkResult{Name: name, OK: true})
		lname := "label " + cfg.TrackedLabel + " in " + r.Owner + "/" + r.Name
		if _, err := gh.rest(ctx, http.MethodGet, "/repos/"+escapePath(r.Owner)+"/"+escapePath(r.Name)+"/labels/"+escapePath(cfg.TrackedLabel), nil, nil, http.StatusOK); err != nil {
			results = append(results, checkResult{Name: lname, OK: false, Detail: "create it first (gh label create " + cfg.TrackedLabel + " -R " + r.Owner + "/" + r.Name + "): " + gh.clip(err.Error(), 200)})
			continue
		}
		results = append(results, checkResult{Name: lname, OK: true})
	}
	return answer()
}
