// Package servertest starts a complete API server for tests: a real HTTP
// server and a real SQLite database, with Docker replaced by an in-memory
// engine and Nginx by small stand-ins.
package servertest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/audit"
	"github.com/onrcanogul/liman/internal/backup"
	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/deploy"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/engine/enginetest"
	"github.com/onrcanogul/liman/internal/nginx"
	"github.com/onrcanogul/liman/internal/secrets"
	"github.com/onrcanogul/liman/internal/server"
	"github.com/onrcanogul/liman/internal/store"
)

// Harness is a running test server and everything behind it.
type Harness struct {
	Server *server.Server
	// Client is authenticated with a valid token.
	Client *api.Client
	// URL is the server's base address.
	URL string
	// HookURL is the address of the trigger endpoint's own server.
	HookURL string

	Engine *enginetest.Fake
	Store  *store.Store

	// KeyPath is the master key file; BackupDir is where backups go.
	KeyPath   string
	BackupDir string

	// ConnectErr, when set, makes the engine unavailable, as when Docker is
	// not running.
	ConnectErr error

	// HealthErr, when set, is what every health probe of a deploy returns.
	HealthErr error

	// releases numbers the deploys: r1, r2, ... It is atomic because
	// handlers of concurrent requests may deploy at the same time.
	releases atomic.Int32
}

// New starts a server that is shut down when the test ends. Each option can
// adjust the server before it starts handling requests.
func New(t *testing.T, options ...func(*server.Server)) *Harness {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "liman.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	token, err := st.CreateToken(context.Background(), "test")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	// A fresh random key per test server, in a file like the real one, so
	// that backups can be checked against it.
	keyPath := filepath.Join(t.TempDir(), "master.key")
	key, err := secrets.CreateKey(keyPath)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	vault, err := secrets.NewVault(key, st)
	if err != nil {
		t.Fatalf("create vault: %v", err)
	}

	h := &Harness{Engine: enginetest.New(), Store: st, KeyPath: keyPath, BackupDir: filepath.Join(t.TempDir(), "backups")}

	h.Server = &server.Server{
		ChecksumKey: secrets.Derive(key, server.ChecksumLabel),
		Store:       st,
		Secrets:     vault,
		Backups:     &backup.Service{Store: st, Dir: h.BackupDir, KeyPath: keyPath},
		Audit:       &audit.Recorder{Store: st, Keys: audit.KeysAt(keyPath)},
		Connect: func(context.Context) (engine.Engine, error) {
			if h.ConnectErr != nil {
				return nil, h.ConnectErr
			}
			return h.Engine, nil
		},
		ConfirmTimeout: 2 * time.Second,
		// Deploys must not wait for real ports, clocks or networks.
		NewDeployer: func(e engine.Engine, out io.Writer) *deploy.Deployer {
			d := deploy.New(e, out)
			d.AllocatePort = func() (int, error) { return 40001, nil }
			d.Probe = func(context.Context, string) error { return h.HealthErr }
			// Fake deploys finish within a millisecond, faster than the
			// clock-based release names can tell apart.
			d.NewRelease = func() string { return fmt.Sprintf("r%d", h.releases.Add(1)) }
			d.HealthTimeout = 100 * time.Millisecond
			d.HealthInterval = time.Millisecond
			d.Drain = 0
			return d
		},
	}

	for _, option := range options {
		option(h.Server)
	}

	srv := httptest.NewServer(h.Server.Handler())
	t.Cleanup(srv.Close)

	hooks := httptest.NewServer(h.Server.HookHandler())
	t.Cleanup(hooks.Close)

	h.URL = srv.URL
	h.HookURL = hooks.URL
	h.Client = &api.Client{BaseURL: srv.URL, Token: token}
	return h
}

// Register registers app with its image repository and domain.
func (h *Harness) Register(t *testing.T, app, repo, domain string) {
	t.Helper()
	if err := h.Store.Register(context.Background(), store.Registration{Name: app, Repo: repo, Domain: domain}); err != nil {
		t.Fatalf("register %s: %v", app, err)
	}
}

// DeployKey creates a deploy key named name for apps, which must be
// registered, and returns its token and a client for the trigger endpoint
// that uses it.
func (h *Harness) DeployKey(t *testing.T, name string, apps ...string) (string, *api.Client) {
	t.Helper()
	token, err := h.Store.CreateScopedToken(context.Background(), name, store.ScopeDeploy, time.Time{}, apps...)
	if err != nil {
		t.Fatalf("create deploy key: %v", err)
	}
	return token, &api.Client{BaseURL: h.HookURL, Token: token}
}

// WithProxy gives the server a real nginx.Manager that writes into a
// temporary directory. Nginx itself is replaced by two stand-ins:
//
//   - the test and reload commands are the standard Unix programs true and
//     false, which do nothing and exit with success or failure;
//   - a local HTTP server answers the release probe by looking at the files
//     in the config directory, as an Nginx that reloads instantly would.
func (h *Harness) WithProxy(t *testing.T, reloadCommand string) *nginx.Manager {
	t.Helper()

	confDir := t.TempDir()

	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		files, _ := filepath.Glob(filepath.Join(confDir, "*.conf"))
		for _, file := range files {
			content, err := os.ReadFile(file)
			if err == nil && strings.Contains(string(content), "server_name "+r.Host+";") {
				w.Header().Set(nginx.ReleaseHeader, nginx.ReleaseOf(content))
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(probe.Close)

	m := nginx.New(config.Nginx{
		ConfDir:       confDir,
		UpstreamHost:  "127.0.0.1",
		ProbeAddr:     strings.TrimPrefix(probe.URL, "http://"),
		TestCommand:   []string{"true"},
		ReloadCommand: []string{reloadCommand},
	})
	h.Server.Proxy = m
	return m
}

// ErrDockerDown is a ready-made ConnectErr.
var ErrDockerDown = errors.New("connect to docker (is it running?): connection refused")
