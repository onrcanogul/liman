// Package daemon implements the limand command: serving the API and
// managing API tokens.
//
// It lives outside cmd/limand so it can be tested like any other package;
// main only supplies the real streams and environment.
package daemon

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/applock"
	"github.com/onrcanogul/liman/internal/audit"
	"github.com/onrcanogul/liman/internal/backup"
	"github.com/onrcanogul/liman/internal/buildinfo"
	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/monitor"
	"github.com/onrcanogul/liman/internal/nginx"
	"github.com/onrcanogul/liman/internal/panel"
	"github.com/onrcanogul/liman/internal/secrets"
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
  limand token create <name> [--read-only | --deploy | --preview]
                      [--expires <90d|12h>]
                      [--app <app>]... [--write-client <file>] [--url <url>]
                                  Create an API token. It is printed once, or
                                  with --write-client written to a client.yaml
                                  instead of being printed. A --read-only
                                  token can look at everything and change
                                  nothing. With --expires it stops working
                                  after that many days or hours; without, it
                                  works until it is revoked. With --app it
                                  sees and acts on those apps only, and
                                  nothing about the daemon as a whole. A
                                  --deploy token (a deploy key, for a CI
                                  pipeline) needs --app and can do one
                                  thing: trigger deploys of those apps on
                                  the hooks.listen endpoint. A --preview
                                  token, for pull requests, can only ask
                                  what a deploy would change
  limand app create <name> --repo <repository> --domain <domain>
                      [--key <name>] [--write-client <file>] [--url <url>]
                      [--preview-key [--preview-client <file>]]
                                  Register an app for deploys from CI: its
                                  image repository (no tag) and its domain,
                                  which its pipeline cannot change. Creates
                                  its deploy key, named <name>-ci unless
                                  --key, printed once; with --preview-key
                                  also a preview key, <name>-pr
  limand app list                 List registered apps, their last trigger
                                  and their keys
  limand app rotate-key <name> [--preview] [--keep-old <1h>] [--key <name>]
                      [--write-client <file>] [--url <url>]
                                  Give an app a new deploy (or preview) key
                                  and retire the old ones: at once, or, with
                                  --keep-old, when that time has passed
  limand app delete <name>        Unregister an app and revoke its deploy and
                                  preview keys; its containers keep running
                                  until stopped
  limand token list               List tokens, when they expire and were last used
  limand token revoke <name>      Revoke a token
  limand audit verify             Check the audit trail: that no entry was
                                  changed, removed, inserted or reordered
                                  since it was written
  limand backup create <dir> [--keep <n>]
                                  Write a copy of the database into <dir> and
                                  check it; with --keep, remove all but the
                                  newest <n> backups there. Safe while the
                                  daemon is running
  limand backup identity <file>   Write the age identities that open the
                                  backups (encrypted with the master key), for
                                  the age tool: age -d -i <file> <backup>
  limand backup verify <file>     Check a backup: is it intact, and can its
                                  secrets be decrypted with this master key
  limand backup restore <file> [--allow-unsigned]
                                  Replace the database with a backup. The
                                  daemon must be stopped. A backup whose
                                  signature cannot be checked is refused
                                  unless --allow-unsigned is given
  limand cert list                List the certificates in nginx.cert_dir and
                                  when they expire
  limand cert renew <domain>      Get a new certificate for <domain> now, over
                                  ACME (acme in config.yaml), and reload Nginx.
                                  The daemon renews by itself; this is for
                                  when that keeps failing or cannot wait
  limand key list                 List the master key and the old keys, with
                                  the secrets and backups that depend on each
  limand key rotate               Replace the master key and seal every secret
                                  again with the new one. The old key is kept
                                  for older backups. The daemon must be
                                  stopped
  limand key retire <id> [--force]
                                  Delete an old key. Refused while secrets
                                  depend on it; backups signed with it are
                                  listed, and only --force retires it anyway
  limand version [--schema]       Print the version, and the database schema
                                  this limand uses and liman.db has. With
                                  --schema just the two numbers: "<db> <limand>"
  limand help                     Show this help

Files are kept in $LIMAN_HOME (default ~/.liman): config.yaml and liman.db.
While it runs, limand also serves a read-only web panel under /ui/.
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
	// HooksReady is Ready for the trigger endpoint, when hooks.listen
	// turns it on. It is called before Ready.
	HooksReady func(addr string)

	// AuditAnchor receives a line for every audit record a limand command
	// adds (the daemon's own go to its log): the system log in production,
	// so that journald keeps them next to the daemon's (journalctl -t
	// limand). Nil writes none; tests leave it nil.
	AuditAnchor io.Writer

	// rotationUnfinished is set by openVault when a rotation's new key is
	// lying next to the master key.
	rotationUnfinished bool
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
	case "app":
		return d.runApp(ctx, args[1:])
	case "backup":
		return d.runBackup(ctx, args[1:])
	case "cert":
		return d.runCert(ctx, args[1:])
	case "key":
		return d.runKey(ctx, args[1:])
	case "audit":
		return d.runAudit(ctx, args[1:])
	case "version":
		return d.version(ctx, args[1:])
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

	// slog writes structured log lines: a message plus key=value pairs.
	// Under systemd they go to the journal; nothing else is needed.
	log := slog.New(slog.NewTextHandler(d.Stderr, nil))

	st, err := d.openStoreForServe(ctx, cfg, log)
	if err != nil {
		return d.fail(err)
	}
	defer func() { _ = st.Close() }()

	connect := connectOnce(d.Connect)
	// One set of locks for everything that changes an app: the API handlers
	// and the health monitor.
	locks := &applock.Locks{}

	vault, err := d.openVault(ctx, cfg, st, log)
	if err != nil {
		return d.fail(err)
	}

	recorder := &audit.Recorder{Store: st, Keys: audit.KeysAt(d.keyPath(cfg)), Log: log}
	// The trail's head as the daemon finds it, in the same form as the
	// anchor of every record it adds: records written while it was
	// stopped (a key rotation, a restore) are anchored from here on.
	if head, err := st.AuditEntries(ctx, nil, 1); err == nil && len(head) == 1 {
		log.Info("audit", "seq", head[0].Seq, "action", head[0].Action, "mac", hex.EncodeToString(head[0].MAC), "head", true)
	}
	srv := &server.Server{Store: st, Connect: connect, Log: log, Locks: locks, Secrets: vault, Started: time.Now(), Audit: recorder}
	// The checksums of triggered deploys are HMACs with a key of their
	// own, derived from the master key that seals.
	ring, err := secrets.LoadKeyring(d.keyPath(cfg))
	if err != nil {
		return d.fail(err)
	}
	srv.ChecksumKey, _ = ring.Derive(ring.Primary(), server.ChecksumLabel)
	srv.Keys = d.keysForAPI(cfg)
	srv.KeyRotationUnfinished = d.rotationUnfinished
	if cfg.Nginx.CertDir != "" {
		srv.Certificates = certificatesForAPI(cfg, connect)
	}
	if !cfg.Panel.Disabled {
		srv.Panel = panel.Handler()
	}
	srv.Backups = &backup.Service{Store: st, Dir: cfg.Backup.Dir, KeyPath: d.keyPath(cfg), Keep: 14}
	if srv.Backups.Dir == "" {
		srv.Backups.Dir = filepath.Join(d.Home, "backups")
	}
	if cfg.Backup.Keep != nil {
		srv.Backups.Keep = *cfg.Backup.Keep
	}
	if cfg.Deploy.Drain != nil {
		drain := time.Duration(*cfg.Deploy.Drain)
		srv.Drain = &drain
	}
	// Only assign the proxy when it is enabled. Assigning a nil
	// *nginx.Manager to the interface field would make the field non-nil.
	var proxy *nginx.Manager
	if cfg.Nginx.Enabled() {
		proxy = nginx.New(cfg.Nginx)
		srv.Proxy = proxy
	}

	// The certificate renewer runs like the monitor, in a goroutine of its
	// own that is stopped (and waited for) before the daemon exits.
	certsCtx, stopCerts := context.WithCancel(context.Background())
	defer stopCerts()
	certsDone := make(chan struct{})
	// The same check for the issuer: a nil *certs.Issuer in the interface
	// would look like one that is there. acme is only valid together with
	// nginx, so proxy is set whenever is is.
	if is := d.issuer(cfg); is != nil && proxy != nil {
		srv.Certs = deployCerts{is: is}
		renewer := &certRenewer{
			store: is.Store, obtain: is.Obtain, proxy: proxy,
			connect: connect, locks: locks, log: log, now: time.Now, audit: recorder,
		}
		go func() {
			defer close(certsDone)
			renewer.run(certsCtx, certCheckFirst, certCheckEvery)
		}()
	} else {
		close(certsDone)
	}

	// The monitor runs for as long as the daemon does, in a goroutine of
	// its own. monitorDone is closed when that goroutine has ended, which
	// the shutdown below waits for.
	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	defer stopMonitor()
	monitorDone := make(chan struct{})

	if cfg.Monitor.Disabled {
		close(monitorDone)
	} else {
		mon := &monitor.Monitor{
			Connect:          connect,
			Locks:            locks,
			Interval:         time.Duration(cfg.Monitor.Interval),
			FailureThreshold: cfg.Monitor.FailureThreshold,
			Log:              log,
			OnRestart:        recordRestart(st, recorder, log),
			OnCheck:          recordChecks(st, log, healthRetention),
		}
		srv.Monitoring = &api.MonitorInfo{
			IntervalSeconds:  int(orDefault(mon.Interval, monitor.DefaultInterval) / time.Second),
			FailureThreshold: orDefault(mon.FailureThreshold, monitor.DefaultFailureThreshold),
			RetentionSeconds: int(healthRetention / time.Second),
		}
		srv.HealthOf = func(app string) (string, string, int, bool) {
			status, ok := mon.Status(app)
			return status.Health, status.Detail, status.Restarts, ok
		}
		go func() {
			defer close(monitorDone)
			mon.Run(monitorCtx)
		}()
	}

	// Listening and serving are separate steps on purpose: if the address
	// is taken, the error surfaces here, before anything claims to be up.
	listener, err := net.Listen("tcp", cfg.Daemon.Listen)
	if err != nil {
		return d.fail(fmt.Errorf("listen on %s: %w", cfg.Daemon.Listen, err))
	}
	httpServer := newHTTPServer(srv.Handler())

	// The trigger endpoint, when turned on, is a second server on a
	// listener of its own, with the same limits and the same shutdown.
	var hookServer *http.Server
	var hookListener net.Listener
	if cfg.Hooks.Enabled() {
		if hookListener, err = net.Listen("tcp", cfg.Hooks.Listen); err != nil {
			_ = listener.Close()
			return d.fail(fmt.Errorf("listen on %s (hooks.listen): %w", cfg.Hooks.Listen, err))
		}
		hookServer = newHTTPServer(srv.HookHandler())
	}

	if tokens, err := st.Tokens(ctx); err == nil && len(tokens) == 0 {
		log.Warn("no API tokens exist yet; create one with: limand token create <name>")
	}
	hooksAddr := ""
	if hookListener != nil {
		hooksAddr = hookListener.Addr().String()
	}
	log.Info("limand started",
		"listen", listener.Addr().String(),
		"hooks", hooksAddr,
		"version", buildinfo.Version,
		"nginx", cfg.Nginx.Enabled(),
		"acme", cfg.ACME.Enabled(),
		"monitor", !cfg.Monitor.Disabled,
		"panel", !cfg.Panel.Disabled,
	)
	if d.HooksReady != nil && hookListener != nil {
		d.HooksReady(hooksAddr)
	}
	if d.Ready != nil {
		d.Ready(listener.Addr().String())
	}

	// Serve blocks, so each server runs in its own goroutine and reports
	// its result on a channel. The buffer lets those goroutines finish
	// even if nobody is receiving any more.
	serveErr := make(chan error, 2)
	go func() { serveErr <- httpServer.Serve(listener) }()
	if hookServer != nil {
		go func() { serveErr <- hookServer.Serve(hookListener) }()
	}

	// Wait for whichever happens first: a server failing on its own, or
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

	// Stop the monitor first and wait until it has really stopped. A
	// restart it started is finished; no new one begins while the daemon is
	// on its way out.
	stopMonitor()
	<-monitorDone
	// The same for the certificate renewer: an order in progress is cut
	// off (the CA forgets it), but a half-written certificate cannot
	// happen, since files are only ever renamed into place.
	stopCerts()
	<-certsDone

	// ctx is already cancelled, so the shutdown needs a context of its own.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Shutdown stops accepting new connections and waits for running
	// requests to finish. A deploy in progress therefore completes (or
	// rolls back) instead of being cut off halfway. The trigger endpoint
	// goes first: no new deploy should arrive from outside while the API
	// is draining.
	servers := []*http.Server{httpServer}
	if hookServer != nil {
		servers = []*http.Server{hookServer, httpServer}
	}
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			return d.fail(fmt.Errorf("shutdown: %w", err))
		}
		// After Shutdown, Serve returns http.ErrServerClosed; that is the
		// expected way for it to end.
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return d.fail(err)
		}
	}

	log.Info("limand stopped")
	return exitOK
}

// newHTTPServer returns the server for one of limand's listeners.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler: h,

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
}

// openVault loads the master key, creating it on the very first start, and
// returns the vault that encrypts app secrets with it.
func (d *Daemon) openVault(ctx context.Context, cfg config.Config, st *store.Store, log *slog.Logger) (*secrets.Vault, error) {
	path := d.keyPath(cfg)

	exists, err := secrets.KeyExists(path)
	if err != nil {
		return nil, err
	}

	if exists {
		// The master key and the keys it replaced: values and backups
		// made before a rotation name their key and are opened with it.
		ring, err := secrets.LoadKeyring(path)
		if err != nil {
			return nil, err
		}
		if id, pending := ring.Pending(); pending {
			d.rotationUnfinished = true
			log.Warn("a key rotation did not finish; everything still opens, but finish it with: limand key rotate (daemon stopped)",
				"pending_key", id.String(), "file", secrets.PendingKeyPath(path))
		}
		return secrets.NewVaultWithKeyring(ring, st), nil
	}

	// No key file. On a first start that is expected, and a key is
	// created. But if secrets are already stored, they were encrypted
	// with a key that is now missing: generating a new one would look
	// like a working setup while every stored secret had become
	// unreadable. Refuse, and say what to do.
	count, err := st.CountSecrets(ctx)
	if err != nil {
		return nil, err
	}
	if count > 0 {
		stored := fmt.Sprintf("%d secrets are stored that were", count)
		if count == 1 {
			stored = "1 secret is stored that was"
		}
		return nil, fmt.Errorf("the master key %s is missing, but %s encrypted with it; restore the key file from your backup (without it the stored secrets cannot be recovered)", path, stored)
	}
	key, err := secrets.CreateKey(path)
	if err != nil {
		return nil, err
	}
	log.Warn("created a new master key; back it up, secrets cannot be recovered without it", "path", path)
	return secrets.NewVault(key, st)
}

// auditor returns the recorder for a limand command run on the server. Its
// rows are signed like the daemon's. What goes wrong is shown on stderr;
// the anchor line of each row goes to AuditAnchor, not to the terminal,
// where nobody would keep it. Without anchors, a record a command added
// (a token created on the server, say) could be cut off the end of the
// trail unseen.
func (d *Daemon) auditor(st *store.Store) *audit.Recorder {
	keyPath := filepath.Join(d.Home, "master.key")
	if cfg, err := config.Load(filepath.Join(d.Home, "config.yaml")); err == nil {
		keyPath = d.keyPath(cfg)
	}
	handlers := fanout{slog.NewTextHandler(d.Stderr, &slog.HandlerOptions{Level: slog.LevelError})}
	if d.AuditAnchor != nil {
		handlers = append(handlers, slog.NewTextHandler(d.AuditAnchor, nil))
	}
	return &audit.Recorder{Store: st, Keys: audit.KeysAt(keyPath), Log: slog.New(handlers)}
}

// keyPath returns where the master key is kept.
func (d *Daemon) keyPath(cfg config.Config) string {
	if cfg.Secrets.KeyFile != "" {
		return cfg.Secrets.KeyFile
	}
	return filepath.Join(d.Home, "master.key")
}

// recordRestart returns the callback that writes the monitor's restarts
// into the history, next to the steps of deploys.
func recordRestart(st *store.Store, rec *audit.Recorder, log *slog.Logger) func(monitor.Event) {
	return func(ev monitor.Event) {
		ev2 := store.Event{
			Time:       ev.Started,
			App:        ev.App,
			Release:    ev.Release,
			Step:       fmt.Sprintf("Restart container %s: %s", ev.Container, ev.Reason),
			Action:     "restart",
			OK:         ev.Err == nil,
			DurationMS: ev.Duration.Milliseconds(),
		}
		if ev.Err != nil {
			ev2.Error = ev.Err.Error()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := st.AppendEvent(ctx, ev2); err != nil {
			log.Error("restart not recorded", "app", ev.App, "error", err)
		}
		rec.Record(ctx, audit.Monitor, audit.Restart, ev.App, ev.Container, ev.Err)
	}
}

// healthRetention is how long health checks are kept. It matches the
// longest range the API offers; keeping more would only grow the database.
const healthRetention = 7 * 24 * time.Hour

// pruneInterval is how often old health checks are deleted.
const pruneInterval = time.Hour

// recordChecks returns the callback that stores the monitor's health checks
// and, now and then, deletes the ones older than retention.
//
// Pruning rides along with recording instead of having a timer of its own:
// the table only grows when checks are recorded, so that is exactly when
// there can be something to delete, and there is no extra goroutine to
// start and stop.
func recordChecks(st *store.Store, log *slog.Logger, retention time.Duration) func(monitor.Check) {
	// The monitor checks several apps at once, so this callback runs on
	// several goroutines; mu guards the one variable they share.
	var (
		mu        sync.Mutex
		lastPrune time.Time
	)

	return func(c monitor.Check) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := st.AppendHealthSample(ctx, store.HealthSample{
			Time: c.Time, App: c.App, OK: c.OK, Probed: c.Probed, Duration: c.Duration,
		})
		if err != nil {
			// The history is a convenience; losing a sample must not stop
			// the monitor from doing its real job.
			log.Error("health check not recorded", "app", c.App, "error", err)
		}

		mu.Lock()
		due := c.Time.Sub(lastPrune) >= pruneInterval
		if due {
			lastPrune = c.Time
		}
		mu.Unlock()
		if !due {
			return
		}

		removed, err := st.PruneHealthSamples(ctx, c.Time.Add(-retention))
		if err != nil {
			log.Error("old health checks not removed", "error", err)
		} else if removed > 0 {
			log.Info("removed old health checks", "count", removed)
		}
	}
}

// orDefault returns v, or def if v is the zero value of its type.
func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// connectOnce wraps connect so that the engine is connected to once and the
// connection is then shared by every request.
//
// Connecting per request, as the first version did, created a new client
// (with its own connection pool) for every request and never closed it. The
// load test showed the result: the daemon's memory grew from 22 to 253 MiB
// and each listing paid for a new connection and a ping.
//
// A failed attempt is not remembered: while Docker is down every request
// tries again, and the first success is kept. The client itself survives a
// Docker restart, because it dials the socket anew whenever it needs to.
func connectOnce(connect func(ctx context.Context) (engine.Engine, error)) func(ctx context.Context) (engine.Engine, error) {
	var (
		mu     sync.Mutex
		shared engine.Engine
	)

	return func(ctx context.Context) (engine.Engine, error) {
		// The lock is held while connecting. Requests that arrive together
		// at start-up therefore wait for one connection instead of each
		// opening their own.
		mu.Lock()
		defer mu.Unlock()

		if shared != nil {
			return shared, nil
		}
		eng, err := connect(ctx)
		if err != nil {
			return nil, err
		}
		shared = eng
		return shared, nil
	}
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
	const synopsis = "limand token create <name> [--read-only | --deploy | --preview] [--expires <90d|12h>] [--app <app>]... [--write-client <file>] [--url <url>]"

	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	writeClient := fs.String("write-client", "", "write the token to this client.yaml instead of printing it")
	url := fs.String("url", "", "daemon URL to put into the client.yaml")
	readOnly := fs.Bool("read-only", false, "the token can look at everything and change nothing")
	deployKey := fs.Bool("deploy", false, "the token can only trigger deploys of its apps, from a pipeline")
	previewKey := fs.Bool("preview", false, "the token can only preview deploys of its apps, from a pull request's pipeline")
	expires := fs.String("expires", "", "the token stops working after this long: 90d, 12h")
	var apps appList
	fs.Var(&apps, "app", "limit the token to this app; repeat for more")

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
	if n := btoi(*readOnly) + btoi(*deployKey) + btoi(*previewKey); n > 1 {
		fmt.Fprintln(d.Stderr, "limand: --read-only, --deploy and --preview cannot be combined: a token has one scope")
		return exitUsage
	}
	if (*deployKey || *previewKey) && len(apps) == 0 {
		fmt.Fprintln(d.Stderr, "limand: a --deploy or --preview token needs --app: a key for every app would reach anything from anywhere")
		return exitUsage
	}
	for _, a := range apps {
		if !app.ValidName(a) {
			fmt.Fprintf(d.Stderr, "limand: --app %q is not a valid app name\n", a)
			return exitUsage
		}
	}
	// Checked before anything is touched: a typo in --expires or --app is
	// a usage error, like a typo in any other flag. An app name with a
	// typo would make a token for an app that does not exist, which
	// nobody would notice until it was refused.
	var expiresAt time.Time
	if *expires != "" {
		lifetime, err := parseLifetime(*expires)
		if err != nil {
			fmt.Fprintln(d.Stderr, "limand:", err)
			return exitUsage
		}
		expiresAt = time.Now().Add(lifetime)
	}

	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(err)
	}
	defer func() { _ = st.Close() }()

	scope := store.ScopeAdmin
	switch {
	case *readOnly:
		scope = store.ScopeRead
	case *deployKey:
		scope = store.ScopeDeploy
	case *previewKey:
		scope = store.ScopePreview
	}
	token, err := st.CreateScopedToken(ctx, name, scope, expiresAt, apps...)
	if err != nil {
		return d.fail(err)
	}
	what := fmt.Sprintf("token %q", name)
	if *deployKey || *previewKey {
		what = fmt.Sprintf("%s key %q", scope, name)
	}
	if len(apps) > 0 {
		what += " for " + strings.Join(apps, ", ")
	}
	// Recorded with what the token may do, never with the token itself.
	target := name + " (" + scope
	if len(apps) > 0 {
		target += ", " + strings.Join(apps, ", ")
	}
	if !expiresAt.IsZero() {
		target += ", until " + expiresAt.UTC().Format(time.RFC3339)
	}
	d.auditor(st).Record(ctx, audit.ServerActor(), audit.TokenCreate, "", target+")", nil)
	if !expiresAt.IsZero() {
		what += fmt.Sprintf(" (expires %s)", expiresAt.Local().Format("2006-01-02 15:04"))
	}

	return d.handOver(ctx, st, newToken{name: name, token: token, what: what, writeClient: *writeClient, url: *url, scope: scope})
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// newToken is a token just created, to be handed to its user.
type newToken struct {
	name, token string
	// what describes it: `token "laptop"`, `deploy key "web-ci" for web`.
	what string
	// writeClient and url are the flags of the same names.
	writeClient, url string
	// scope is the token's; deploy and preview keys go to the hooks
	// listener.
	scope string
}

func (t newToken) forHooks() bool {
	return t.scope == store.ScopeDeploy || t.scope == store.ScopePreview
}

// handOver prints a new token once, or writes it into a client.yaml. A
// token that cannot be handed over is revoked again: it exists, but
// nobody has it.
func (d *Daemon) handOver(ctx context.Context, st *store.Store, t newToken) int {
	if t.writeClient != "" {
		client := config.Client{URL: t.url, Token: t.token}
		if client.URL == "" {
			cfg, err := config.Load(filepath.Join(d.Home, "config.yaml"))
			if err != nil {
				_ = st.RevokeToken(ctx, t.name)
				return d.fail(err)
			}
			client.URL = "http://" + cfg.Daemon.Listen
			// Deploy and preview keys work on the hooks listener only.
			if t.forHooks() {
				if !cfg.Hooks.Enabled() {
					_ = st.RevokeToken(ctx, t.name)
					return d.fail(errors.New("hooks.listen is not set, so there is no trigger endpoint to write into the file; set it, or give --url"))
				}
				client.URL = "http://" + cfg.Hooks.Listen
			}
		}
		if err := config.SaveClient(t.writeClient, client); err != nil {
			_ = st.RevokeToken(ctx, t.name)
			return d.fail(err)
		}
		// The token itself is deliberately not printed in this mode.
		fmt.Fprintf(d.Stdout, "Created %s and wrote it to %s\n", t.what, t.writeClient)
		return exitOK
	}

	fmt.Fprintf(d.Stdout, "Created %s. It is shown only this once:\n\n  %s\n\n", t.what, t.token)
	switch t.scope {
	case store.ScopeDeploy:
		fmt.Fprintln(d.Stdout, "Store it as a secret variable of your CI (LIMAN_TOKEN), next to LIMAN_URL: the public address of the trigger endpoint.")
		return exitOK
	case store.ScopePreview:
		fmt.Fprintln(d.Stdout, "Store it as the LIMAN_TOKEN of your pull request pipeline: it can preview deploys and nothing else.")
		return exitOK
	}
	fmt.Fprintln(d.Stdout, "Put it into client.yaml (token: ...) or the LIMAN_TOKEN variable on the machine you run liman from.")
	return exitOK
}

// appList collects a flag that may be given more than once (--app a --app
// b). flag.Value is the interface the flag package calls for each one.
type appList []string

func (l *appList) String() string { return strings.Join(*l, ",") }

func (l *appList) Set(v string) error {
	*l = append(*l, v)
	return nil
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
	now := time.Now()
	fmt.Fprintln(w, "NAME\tSCOPE\tAPPS\tCREATED\tEXPIRES\tLAST USED")
	for _, t := range tokens {
		apps := "all"
		if t.Limited() {
			apps = strings.Join(t.Apps, ",")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.Name, t.Scope, apps, t.CreatedAt.Local().Format("2006-01-02 15:04:05"), describeExpiry(t, now), describeLastUse(t, now))
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
	d.auditor(st).Record(ctx, audit.ServerActor(), audit.TokenRevoke, "", args[0], nil)
	fmt.Fprintf(d.Stdout, "Revoked token %q\n", args[0])
	return exitOK
}

// version prints the version and where the database stands. It never
// changes anything, so it is safe to run with a new binary before it is
// started.
func (d *Daemon) version(ctx context.Context, args []string) int {
	machine := len(args) == 1 && args[0] == "--schema"
	if len(args) > 0 && !machine {
		return d.usageError("limand version [--schema]")
	}
	want := store.SchemaVersion()
	path := filepath.Join(d.Home, "liman.db")
	have, err := store.SchemaVersionOf(ctx, path)
	if machine {
		if err != nil {
			return d.fail(err)
		}
		fmt.Fprintf(d.Stdout, "%d %d\n", have, want)
		return exitOK
	}

	fmt.Fprintln(d.Stdout, buildinfo.String())
	fmt.Fprintf(d.Stdout, "database schema %d\n", want)
	switch {
	case err != nil:
		// The version itself is what was asked for; a database that
		// cannot be read (another user's, say) is only a note.
		fmt.Fprintf(d.Stdout, "%s: cannot be read (%v)\n", path, err)
	case have == 0:
		fmt.Fprintf(d.Stdout, "%s: not created yet\n", path)
	case have == want:
		fmt.Fprintf(d.Stdout, "%s: schema %d, up to date\n", path, have)
	case have < want:
		fmt.Fprintf(d.Stdout, "%s: schema %d; the daemon backs it up and upgrades it to %d when it starts\n", path, have, want)
	default:
		fmt.Fprintf(d.Stdout, "%s: schema %d, written by a newer limand; this one cannot use it\n", path, have)
	}
	return exitOK
}

// openStore opens the database for a command. One that an older version
// wrote is not upgraded here: that is the daemon's start, which backs it up
// first (see openStoreForServe).
func (d *Daemon) openStore(ctx context.Context) (*store.Store, error) {
	return store.OpenCurrent(ctx, filepath.Join(d.Home, "liman.db"))
}

// openStoreForServe opens the database for the daemon. A database an older
// version wrote is first backed up as it is, and only then upgraded: that
// backup and the previous binary are the way back, since the previous
// version cannot read an upgraded database. Without the backup there is no
// upgrade; the daemon does not start, and says why.
func (d *Daemon) openStoreForServe(ctx context.Context, cfg config.Config, log *slog.Logger) (*store.Store, error) {
	path := filepath.Join(d.Home, "liman.db")
	have, err := store.SchemaVersionOf(ctx, path)
	if err != nil {
		return nil, err
	}
	want := store.SchemaVersion()
	upgrading := have > 0 && have < want
	if upgrading {
		file, _, err := backup.CreateBeforeUpgrade(ctx, path, d.backupDir(cfg), d.keyPath(cfg))
		if err != nil {
			return nil, fmt.Errorf("the database has schema %d and this limand uses %d, but backing it up before the upgrade failed, so nothing was changed: %w", have, want, err)
		}
		log.Info("backed up the database before upgrading it; to go back to the previous version, restore this backup with it",
			"backup", file, "from_schema", have, "to_schema", want)
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if upgrading {
		log.Info("upgraded the database", "from_schema", have, "to_schema", want, "version", buildinfo.Version)
	}
	return st, nil
}

func (d *Daemon) fail(err error) int {
	fmt.Fprintf(d.Stderr, "limand: %v\n", err)
	return exitError
}

func (d *Daemon) usageError(synopsis string) int {
	fmt.Fprintf(d.Stderr, "usage: %s\n", synopsis)
	return exitUsage
}
