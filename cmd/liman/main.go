// Command liman is the CLI for the liman PaaS.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/onrcanogul/liman/internal/cli"
	"github.com/onrcanogul/liman/internal/docker"
	"github.com/onrcanogul/liman/internal/engine"
)

func main() {
	// os.Exit ends the process immediately and skips deferred calls, so the
	// work happens in run and main only turns its result into an exit code.
	os.Exit(run())
}

func run() int {
	// ctx is cancelled when the user presses Ctrl+C or the process receives
	// SIGTERM. Every long-running operation receives this context and stops
	// when it is cancelled.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := &cli.CLI{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		// This is the only place that names the concrete engine. Everything
		// below it works with the engine.Engine interface.
		Connect: func(ctx context.Context) (engine.Engine, error) {
			return docker.Connect(ctx)
		},
	}
	return c.Run(ctx, os.Args[1:])
}
