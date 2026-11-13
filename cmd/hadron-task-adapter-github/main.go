// Command hadron-task-adapter-github is the GitHub intake adapter for Hadron
// teams: the hadron CLI runs it to list a team's GitHub issues that no task
// tracks yet, and to mark the ones a task claims (cor:agt:120:04). See the
// README for the calls, the configuration and the credential.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/hadron-memory/hadron-task-adapter-github/internal/adapter"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := adapter.Run(ctx, adapter.Env{
		Args:   os.Args[1:],
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv,
	})
	stop()
	os.Exit(code)
}
