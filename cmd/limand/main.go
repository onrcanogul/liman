// Command limand is the liman daemon. It runs on the server, serves the API
// and executes deploys.
package main

import (
	"context"
	"fmt"
	"log/syslog"
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
		// This is the only place that names the concrete engines. Everything
		// below it works with the engine.Engine interface.
		Connect: func(ctx context.Context, cfg config.Engine) (engine.Engine, error) {
			opts, err := engineOptions(cfg)
			if err != nil {
				return nil, err
			}
			return docker.Connect(ctx, opts)
		},
	}
	// The anchors of audit records added by limand commands go to the
	// system log, where journald keeps them next to the daemon's own
	// (journalctl -t limand). Without a system log there are none; the
	// command still works.
	if w, err := syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "limand"); err == nil {
		d.AuditAnchor = w
		defer func() { _ = w.Close() }()
	}
	return d.Run(ctx, os.Args[1:])
}

// engineOptions turns engine.driver into the client for that engine. Podman
// speaks Docker's API; what differs is in its preset.
func engineOptions(cfg config.Engine) (docker.Options, error) {
	switch cfg.Driver {
	case "", "docker":
		return docker.ForDocker(socketHost(cfg.Socket)), nil
	case "podman":
		return docker.ForPodman(cfg.Socket), nil
	default:
		return docker.Options{}, fmt.Errorf("engine.driver: unknown engine %q; liman drives docker or podman", cfg.Driver)
	}
}

// socketHost turns a socket path into a client address; "" keeps DOCKER_HOST.
func socketHost(socket string) string {
	if socket == "" {
		return ""
	}
	return "unix://" + socket
}
