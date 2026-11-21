// Command hadron-task-adapter-github is the GitHub intake adapter for
// hadron-cli: `hadron team task find-untracked --adapter <this executable>`
// runs it as `hadron-task-adapter-github <call>`, one JSON request on stdin,
// one JSON answer on stdout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/hadron-memory/hadron-task-adapter-github/internal/adapter"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)) }

func run(args []string, stdin, stdout, stderr *os.File, getenv func(string) string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: hadron-task-adapter-github <call>   call is one of: "+strings.Join(adapter.Calls, ", ")+" (request JSON on stdin)")
		return adapter.ExitUsage
	}
	call := args[0]
	if !slices.Contains(adapter.Calls, call) { // before any setup: a wrong call is a usage error whatever the environment holds
		fmt.Fprintln(stderr, "unknown call "+strconv.Quote(oneLine(adapter.ScrubSecret(call, getenv("GITHUB_TOKEN")), 60))+"; the calls are "+strings.Join(adapter.Calls, ", "))
		return adapter.ExitUsage
	}
	a := &adapter.Adapter{Host: hostOf(getenv("GITHUB_SERVER_URL"))}
	if call != "describe" {
		client, err := adapter.NewClient(getenv)
		if err != nil {
			if call != "check" {
				return refuse(stderr, err, getenv("GITHUB_TOKEN"))
			}
			// check is how a bad setup is diagnosed: it answers (after judging the request) and says why
			a.ClientError = adapter.ScrubSecret(err.Error(), getenv("GITHUB_TOKEN"))
			a.CredentialAsked = getenv("GITHUB_TOKEN") != ""
			a.Secret = getenv("GITHUB_TOKEN")
		}
		a.Client = client
		path, err := adapter.ConfigPath(getenv, os.UserHomeDir)
		if err == nil {
			a.Config, err = adapter.LoadConfig(path)
		}
		if err != nil {
			a.Config = nil
			if call != "check" { // check runs without a config, and says why it has none
				return refuse(stderr, err, getenv("GITHUB_TOKEN"))
			}
			a.ConfigError = err.Error()
		}
	}
	out, err := a.Handle(context.Background(), call, stdin)
	if err != nil {
		return refuse(stderr, err, getenv("GITHUB_TOKEN"))
	}
	return answer(stdout, stderr, out, getenv("GITHUB_TOKEN"))
}

func answer(stdout, stderr *os.File, out any, token string) int {
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return refuse(stderr, err, token)
	}
	return 0
}

// refuse prints err as ONE line (the CLI shows the first 4 KiB of stderr, and an
// upstream message must not smuggle in line breaks or terminal controls) and
// returns its exit code.
func refuse(stderr *os.File, err error, token string) int {
	code := adapter.ExitRefused
	var r *adapter.Refusal
	if errors.As(err, &r) {
		code = r.Code
	}
	// A last scrub over everything printed: whatever built the message, the
	// credential is not in it (raw, quoted or URL-escaped).
	fmt.Fprintln(stderr, oneLine(adapter.ScrubSecret(err.Error(), token), 1000))
	return code
}

func oneLine(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			r = ' '
		}
		if b.Len()+len(string(r)) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// hostOf reads the host of GITHUB_SERVER_URL (GitHub Enterprise); empty means github.com.
func hostOf(serverURL string) string {
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(serverURL, "/"), "https://"), "http://")
}
