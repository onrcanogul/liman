// Command liman is the CLI for the liman PaaS. It sends commands to the
// limand daemon.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/cli"
	"github.com/onrcanogul/liman/internal/config"
)

func main() {
	// os.Exit ends the process immediately and skips deferred calls, so the
	// work happens in run and main only turns its result into an exit code.
	os.Exit(run())
}

func run() int {
	// ctx is cancelled when the user presses Ctrl+C or the process receives
	// SIGTERM. Cancelling it aborts the HTTP request in flight; the daemon
	// notices the closed connection and stops (or rolls back) its work.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := config.LoadClient(filepath.Join(config.Home(), "client.yaml"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "liman: %v\n", err)
		return 1
	}

	c := &cli.CLI{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		API:    &api.Client{BaseURL: client.URL, Token: client.Token},
	}
	return c.Run(ctx, os.Args[1:])
}
