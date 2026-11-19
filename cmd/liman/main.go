// Command liman is the CLI for the liman PaaS.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/onrcanogul/liman/internal/cli"
	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/docker"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/history"
	"github.com/onrcanogul/liman/internal/nginx"
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

	home := stateDir()

	cfg, err := config.Load(filepath.Join(home, "config.yaml"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "liman: %v\n", err)
		return 1
	}

	c := &cli.CLI{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		History: history.Store{Path: filepath.Join(home, "history.jsonl")},
		// This is the only place that names the concrete engine. Everything
		// below it works with the engine.Engine interface.
		Connect: func(ctx context.Context) (engine.Engine, error) {
			return docker.Connect(ctx)
		},
	}

	// Only assign the proxy when it is enabled. Assigning a nil *nginx.Manager
	// to the interface field would make the field non-nil (an interface
	// holding a nil pointer is not a nil interface), and the CLI's
	// "is a proxy configured?" check would wrongly say yes.
	if cfg.Nginx.Enabled() {
		c.Proxy = nginx.New(cfg.Nginx)
	}

	return c.Run(ctx, os.Args[1:])
}

// stateDir returns the directory liman keeps its own files in: $LIMAN_HOME
// if set, otherwise ~/.liman.
func stateDir() string {
	if dir := os.Getenv("LIMAN_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home directory (unusual, but possible in minimal containers):
		// fall back to the current directory rather than refusing to run.
		return ".liman"
	}
	return filepath.Join(home, ".liman")
}
