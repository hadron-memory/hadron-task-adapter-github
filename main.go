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
	"strings"

	"github.com/hadron-memory/hadron-task-adapter-github/internal/adapter"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)) }

func run(args []string, stdin *os.File, stdout, stderr *os.File, getenv func(string) string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: hadron-task-adapter-github <call>   call is one of: "+strings.Join(adapter.Calls, ", ")+" (request JSON on stdin)")
		return adapter.ExitUsage
	}
	a := &adapter.Adapter{Host: hostOf(getenv("GITHUB_SERVER_URL"))}
	if args[0] != "describe" {
		path, err := adapter.ConfigPath(getenv, os.UserHomeDir)
		if err == nil {
			var cfg *adapter.Config
			cfg, err = adapter.LoadConfig(path)
			a.Config = cfg
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return adapter.ExitRefused
		}
		a.Client = adapter.NewClient(getenv)
	}
	out, err := a.Handle(context.Background(), args[0], stdin)
	if err != nil {
		var r *adapter.Refusal
		if errors.As(err, &r) {
			fmt.Fprintln(stderr, r.Msg)
			return r.Code
		}
		fmt.Fprintln(stderr, err)
		return adapter.ExitRefused
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		fmt.Fprintln(stderr, err)
		return adapter.ExitRefused
	}
	return 0
}

// hostOf reads the host of GITHUB_SERVER_URL (GitHub Enterprise); empty means github.com.
func hostOf(serverURL string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(serverURL, "/"), "https://"), "http://")
	return s
}
