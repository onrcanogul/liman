// Command limand is the liman daemon. It runs on the server, serves the API
// and executes deploys.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/daemon"
	"github.com/onrcanogul/liman/internal/docker"
	"github.com/onrcanogul/liman/internal/engine"
)

func main() {
	// os.Exit ends the process immediately and skips deferred calls, so the
	// work happens in run and main only turns its result into an exit code.
	os.Exit(run())
}

func run() int {
	// SIGTERM is what systemd and "kill" send to ask a service to stop;
	// SIGINT is Ctrl+C. Both cancel ctx, which starts a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d := &daemon.Daemon{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Home:   config.Home(),
		// This is the only place that names the concrete engine. Everything
		// below it works with the engine.Engine interface.
		Connect: func(ctx context.Context) (engine.Engine, error) {
			return docker.Connect(ctx)
		},
	}
	return d.Run(ctx, os.Args[1:])
}
