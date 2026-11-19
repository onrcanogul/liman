// Package daemon implements the limand command: serving the API and
// managing API tokens.
//
// It lives outside cmd/limand so it can be tested like any other package;
// main only supplies the real streams and environment.
package daemon

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/onrcanogul/liman/internal/buildinfo"
	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/nginx"
	"github.com/onrcanogul/liman/internal/server"
	"github.com/onrcanogul/liman/internal/store"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `limand is the liman daemon. It runs on the server and does the work the
liman CLI asks for.

Usage:
  limand                          Serve the API (same as "limand serve")
  limand token create <name> [--write-client <file>] [--url <url>]
                                  Create an API token. It is printed once, or
                                  with --write-client written to a client.yaml
                                  instead of being printed
  limand token list               List token names
  limand token revoke <name>      Revoke a token
  limand version                  Print version information
  limand help                     Show this help

Files are kept in $LIMAN_HOME (default ~/.liman): config.yaml and liman.db.
`

// Daemon holds what the limand command needs from the outside world.
type Daemon struct {
	Stdout io.Writer
	Stderr io.Writer

	// Home is the directory holding config.yaml and liman.db.
	Home string

	// Connect returns the container engine.
	Connect func(ctx context.Context) (engine.Engine, error)

	// ShutdownTimeout is how long running requests get to finish when the
	// daemon is asked to stop. Zero means two minutes.
	ShutdownTimeout time.Duration

	// Ready, when set, is called with the address the server listens on
	// once it accepts connections. Tests use it to learn the port.
	Ready func(addr string)
}

// Run executes limand with the given arguments and returns the exit code.
func (d *Daemon) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return d.serve(ctx)
	}

	switch args[0] {
	case "serve":
		if len(args) != 1 {
			return d.usageError("limand serve")
		}
		return d.serve(ctx)
	case "token":
		return d.runToken(ctx, args[1:])
	case "version":
		fmt.Fprintln(d.Stdout, buildinfo.String())
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(d.Stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(d.Stderr, "limand: unknown command %q\n\n", args[0])
		fmt.Fprint(d.Stderr, usage)
		return exitUsage
	}
}

// serve runs the API server until ctx is cancelled.
func (d *Daemon) serve(ctx context.Context) int {
	cfg, err := config.Load(filepath.Join(d.Home, "config.yaml"))
	if err != nil {
		return d.fail(err)
	}

	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(err)
	}
	defer func() { _ = st.Close() }()

	// slog writes structured log lines: a message plus key=value pairs.
	// Under systemd they go to the journal; nothing else is needed.
	log := slog.New(slog.NewTextHandler(d.Stderr, nil))

	srv := &server.Server{Store: st, Connect: d.Connect, Log: log}
	// Only assign the proxy when it is enabled. Assigning a nil
	// *nginx.Manager to the interface field would make the field non-nil.
	if cfg.Nginx.Enabled() {
		srv.Proxy = nginx.New(cfg.Nginx)
	}

	// Listening and serving are separate steps on purpose: if the address
	// is taken, the error surfaces here, before anything claims to be up.
	listener, err := net.Listen("tcp", cfg.Daemon.Listen)
	if err != nil {
		return d.fail(fmt.Errorf("listen on %s: %w", cfg.Daemon.Listen, err))
	}

	httpServer := &http.Server{
		Handler: srv.Handler(),

		// Each limit below closes a way of tying up the daemon without
		// being authenticated.
		//
		// A client that opens a connection and never finishes its headers
		// would otherwise hold it, and a goroutine, forever (the attack is
		// known as Slowloris).
		ReadHeaderTimeout: 10 * time.Second,
		// The default allows a megabyte of headers per request.
		MaxHeaderBytes: 16 << 10,
		// Connections kept open between requests are closed when idle.
		IdleTimeout: 2 * time.Minute,

		// There is deliberately no ReadTimeout and no WriteTimeout. Both
		// are measured from the start of a request and would cut off a
		// deploy or a followed log that is working as intended. Slow
		// request bodies are limited per handler instead (server.readBody).
	}

	if tokens, err := st.Tokens(ctx); err == nil && len(tokens) == 0 {
		log.Warn("no API tokens exist yet; create one with: limand token create <name>")
	}
	log.Info("limand started",
		"listen", listener.Addr().String(),
		"version", buildinfo.Version,
		"nginx", cfg.Nginx.Enabled(),
	)
	if d.Ready != nil {
		d.Ready(listener.Addr().String())
	}

	// Serve blocks, so it runs in its own goroutine and reports its result
	// on a channel. The buffer of one lets that goroutine finish even if
	// nobody is receiving any more.
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()

	// Wait for whichever happens first: the server failing on its own, or
	// a request to stop (ctx is cancelled on SIGINT and SIGTERM).
	select {
	case err := <-serveErr:
		return d.fail(fmt.Errorf("serve: %w", err))
	case <-ctx.Done():
	}

	timeout := d.ShutdownTimeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	log.Info("shutting down; waiting for running requests", "timeout", timeout.String())

	// ctx is already cancelled, so the shutdown needs a context of its own.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Shutdown stops accepting new connections and waits for running
	// requests to finish. A deploy in progress therefore completes (or
	// rolls back) instead of being cut off halfway.
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return d.fail(fmt.Errorf("shutdown: %w", err))
	}
	// After Shutdown, Serve returns http.ErrServerClosed; that is the
	// expected way for it to end.
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return d.fail(err)
	}

	log.Info("limand stopped")
	return exitOK
}

func (d *Daemon) runToken(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return d.usageError("limand token create|list|revoke")
	}

	switch args[0] {
	case "create":
		return d.tokenCreate(ctx, args[1:])
	case "list":
		return d.tokenList(ctx, args[1:])
	case "revoke":
		return d.tokenRevoke(ctx, args[1:])
	default:
		return d.usageError("limand token create|list|revoke")
	}
}

func (d *Daemon) tokenCreate(ctx context.Context, args []string) int {
	const synopsis = "limand token create <name> [--write-client <file>] [--url <url>]"

	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	writeClient := fs.String("write-client", "", "write the token to this client.yaml instead of printing it")
	url := fs.String("url", "", "daemon URL to put into the client.yaml")

	// The flag package stops at the first non-flag argument, so the name is
	// taken first and the flags are parsed from what follows it.
	if len(args) == 0 || len(args[0]) == 0 || args[0][0] == '-' {
		return d.usageError(synopsis)
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return d.usageError(synopsis)
	}
	if *url != "" && *writeClient == "" {
		fmt.Fprintln(d.Stderr, "limand: --url only makes sense together with --write-client")
		return exitUsage
	}

	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(err)
	}
	defer func() { _ = st.Close() }()

	token, err := st.CreateToken(ctx, name)
	if err != nil {
		return d.fail(err)
	}

	if *writeClient != "" {
		client := config.Client{URL: *url, Token: token}
		if client.URL == "" {
			cfg, err := config.Load(filepath.Join(d.Home, "config.yaml"))
			if err != nil {
				return d.fail(err)
			}
			client.URL = "http://" + cfg.Daemon.Listen
		}
		if err := config.SaveClient(*writeClient, client); err != nil {
			// The token exists but nobody has it; remove it again rather
			// than leave an unusable entry behind.
			_ = st.RevokeToken(ctx, name)
			return d.fail(err)
		}
		// The token itself is deliberately not printed in this mode.
		fmt.Fprintf(d.Stdout, "Created token %q and wrote it to %s\n", name, *writeClient)
		return exitOK
	}

	fmt.Fprintf(d.Stdout, "Created token %q. It is shown only this once:\n\n  %s\n\n", name, token)
	fmt.Fprintln(d.Stdout, "Put it into client.yaml (token: ...) or the LIMAN_TOKEN variable on the machine you run liman from.")
	return exitOK
}

func (d *Daemon) tokenList(ctx context.Context, args []string) int {
	if len(args) != 0 {
		return d.usageError("limand token list")
	}

	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(err)
	}
	defer func() { _ = st.Close() }()

	tokens, err := st.Tokens(ctx)
	if err != nil {
		return d.fail(err)
	}
	if len(tokens) == 0 {
		fmt.Fprintln(d.Stdout, "No tokens. Create one with: limand token create <name>")
		return exitOK
	}

	w := tabwriter.NewWriter(d.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tCREATED")
	for _, t := range tokens {
		fmt.Fprintf(w, "%s\t%s\n", t.Name, t.CreatedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if err := w.Flush(); err != nil {
		return d.fail(err)
	}
	return exitOK
}

func (d *Daemon) tokenRevoke(ctx context.Context, args []string) int {
	if len(args) != 1 {
		return d.usageError("limand token revoke <name>")
	}

	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(err)
	}
	defer func() { _ = st.Close() }()

	if err := st.RevokeToken(ctx, args[0]); err != nil {
		return d.fail(err)
	}
	fmt.Fprintf(d.Stdout, "Revoked token %q\n", args[0])
	return exitOK
}

func (d *Daemon) openStore(ctx context.Context) (*store.Store, error) {
	return store.Open(ctx, filepath.Join(d.Home, "liman.db"))
}

func (d *Daemon) fail(err error) int {
	fmt.Fprintf(d.Stderr, "limand: %v\n", err)
	return exitError
}

func (d *Daemon) usageError(synopsis string) int {
	fmt.Fprintf(d.Stderr, "usage: %s\n", synopsis)
	return exitUsage
}
