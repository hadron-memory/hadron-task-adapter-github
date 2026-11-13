// Package adapter is the GitHub intake adapter: the calls of the intake
// adapter contract (cor:agt:120:04) answered against GitHub issues, invoked
// the way the hadron-cli runner invokes an adapter (hadron-cli#1961):
//
//	hadron-task-adapter-github <call>
//
// with one JSON object on stdin, which always carries "contractVersion", and
// one JSON object on stdout with exit 0. Any refusal is a non-zero exit, a
// one-line reason on stderr, and nothing on stdout.
package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ContractVersion is the contract version this adapter speaks.
const ContractVersion = 1

// Name is the adapter's name in describe.
const Name = "hadron-task-adapter-github"

// Version is the adapter's own version, set at build time.
var Version = "dev"

// Exit codes. Any non-zero exit is a refusal to the runner; the codes only
// tell a person which kind.
const (
	ExitOK      = 0
	ExitTracker = 1 // GitHub refused, failed or answered something unusable
	ExitUsage   = 2 // the request: unknown call, bad JSON, another contract version, a bad argument
	ExitConfig  = 3 // the configuration or the credential
)

const (
	maxRequestBytes = 1 << 20
	callTimeout     = 50 * time.Second // inside the runner's 60 s
	defaultLimit    = 20
)

// Calls lists every call this adapter answers (describe.calls).
var Calls = []string{"describe", "list-untracked", "mark-tracked", "untrack", "get", "normalize", "check"}

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func usageErr(f string, a ...any) error  { return &exitError{ExitUsage, fmt.Sprintf(f, a...)} }
func configErr(f string, a ...any) error { return &exitError{ExitConfig, fmt.Sprintf(f, a...)} }

// Env is what a run sees of its process.
type Env struct {
	Args   []string // the arguments after the program name
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
}

// Run answers one call and returns the process's exit code.
func Run(ctx context.Context, e Env) int {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var token string
	out, err := run(ctx, e, &token)
	if err == nil {
		b, merr := json.Marshal(out)
		if merr == nil {
			b = append(b, '\n')
			if _, werr := e.Stdout.Write(b); werr == nil {
				return ExitOK
			}
		}
		err = &exitError{ExitTracker, "the answer could not be written"}
	}
	code := ExitTracker
	var ee *exitError
	if errors.As(err, &ee) {
		code = ee.code
	}
	// Every refusal is scrubbed of the credential, whatever its source:
	// GitHub's error text, or data GitHub returned that the adapter refuses.
	msg := err.Error()
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "[redacted]")
	}
	fmt.Fprintf(e.Stderr, "%s: %s\n", Name, oneLine(msg, 600))
	return code
}

func run(ctx context.Context, e Env, tokenOut *string) (any, error) {
	if len(e.Args) != 1 {
		return nil, usageErr("expected exactly one argument, the call (one of %v)", Calls)
	}
	call := e.Args[0]
	known := false
	for _, c := range Calls {
		known = known || c == call
	}
	if !known {
		return nil, usageErr("%q is not a call this adapter answers (one of %v)", oneLine(call, 40), Calls)
	}
	req, err := readRequest(e.Stdin)
	if err != nil {
		return nil, err
	}
	var version struct {
		ContractVersion *int `json:"contractVersion"`
	}
	_ = json.Unmarshal(req, &version)
	// describe answers whatever version the caller speaks: it is how the
	// caller learns which one this adapter speaks.
	if call == "describe" {
		return describe(), nil
	}
	if version.ContractVersion == nil || *version.ContractVersion != ContractVersion {
		got := "none"
		if version.ContractVersion != nil {
			got = fmt.Sprint(*version.ContractVersion)
		}
		return nil, usageErr("the request speaks contract version %s; this adapter speaks %d", got, ContractVersion)
	}
	if call == "check" {
		return check(ctx, e.Getenv), nil
	}
	cfg, err := LoadConfig(e.Getenv)
	if err != nil {
		return nil, configErr("%v", err)
	}
	token, err := cfg.Token(e.Getenv)
	if err != nil {
		return nil, configErr("%v", err)
	}
	*tokenOut = token
	s := &session{cfg: cfg, gh: newGitHub(cfg, token)}
	switch call {
	case "list-untracked":
		return s.listUntracked(ctx, req)
	case "mark-tracked":
		return s.markTracked(ctx, req)
	case "untrack":
		return s.untrack(ctx, req)
	case "get":
		return s.get(ctx, req)
	case "normalize":
		return s.normalize(ctx, req)
	}
	return nil, usageErr("%q is not answered", call)
}

// readRequest reads exactly one JSON object from stdin.
func readRequest(r io.Reader) (json.RawMessage, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxRequestBytes+1))
	if err != nil {
		return nil, usageErr("the request could not be read")
	}
	if len(b) > maxRequestBytes {
		return nil, usageErr("the request is larger than %d bytes", maxRequestBytes)
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return nil, usageErr("the request is not one JSON object")
	}
	var raw json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&raw); err != nil {
		return nil, usageErr("the request is not one well-formed JSON object")
	}
	// More is not an end-of-input check: it reports false before a stray
	// "]" or "}". Only io.EOF after the object proves nothing follows.
	if _, err := dec.Token(); err != io.EOF {
		return nil, usageErr("the request carries text after its JSON object")
	}
	return raw, nil
}

func decode(req json.RawMessage, call string, v any) error {
	if err := json.Unmarshal(req, v); err != nil {
		return usageErr("the %s request has a field of the wrong type: %v", call, err)
	}
	return nil
}

type describeAnswer struct {
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	ContractVersion int      `json:"contractVersion"`
	Calls           []string `json:"calls"`
	Filters         []string `json:"filters"`
	Order           string   `json:"order"`
}

func describe() describeAnswer {
	return describeAnswer{
		Name:            Name,
		Version:         Version,
		ContractVersion: ContractVersion,
		Calls:           Calls,
		// `for` is the only filter a caller passes; the label model is the
		// configuration's.
		Filters: []string{"for"},
		Order:   "created-asc",
	}
}

type session struct {
	cfg *Config
	gh  *githubClient
}

func (s *session) listUntracked(ctx context.Context, req json.RawMessage) (any, error) {
	var r struct {
		Limit  *int   `json:"limit"`
		Cursor string `json:"cursor"`
		For    string `json:"for"`
	}
	if err := decode(req, "list-untracked", &r); err != nil {
		return nil, err
	}
	limit := defaultLimit
	if r.Limit != nil {
		limit = *r.Limit
	}
	if limit < 1 || limit > 100 {
		return nil, usageErr("limit %d is outside 1 to 100", limit)
	}
	items, next, err := (&lister{cfg: s.cfg, gh: s.gh}).list(ctx, r.Cursor, r.For, limit)
	if err != nil {
		return nil, err
	}
	return struct {
		Items      []listedItem `json:"items"`
		NextCursor *string      `json:"nextCursor"`
	}{items, next}, nil
}
