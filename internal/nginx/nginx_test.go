package nginx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/config"
)

func TestRenderHTTPOnly(t *testing.T) {
	got, err := Render(Site{App: "orders-api", Domain: "api.example.com", Upstream: "127.0.0.1:40001", Release: "r2"})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// The whole output is compared: in a config file every character
	// matters, and a golden text makes template changes visible in review.
	want := `# Managed by liman for app "orders-api".
# Do not edit: this file is overwritten on every deploy.

upstream liman-orders-api {
    server 127.0.0.1:40001;
}

server {
    listen 80;
    server_name api.example.com;
    add_header X-Liman-Release "r2" always;

    location / {
        proxy_pass http://liman-orders-api;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
`
	if string(got) != want {
		t.Errorf("Render() =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderWithTLSAndACME(t *testing.T) {
	got, err := Render(Site{
		App:      "orders-api",
		Domain:   "api.example.com",
		Upstream: "127.0.0.1:40001",
		CertFile: "/certs/api.example.com/fullchain.pem",
		KeyFile:  "/certs/api.example.com/privkey.pem",
		ACMERoot: "/var/lib/liman/acme",
		Release:  "r2",
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	want := `# Managed by liman for app "orders-api".
# Do not edit: this file is overwritten on every deploy.

upstream liman-orders-api {
    server 127.0.0.1:40001;
}

server {
    listen 80;
    server_name api.example.com;
    add_header X-Liman-Release "r2" always;

    # Lets an ACME client (Let's Encrypt) verify ownership of the domain.
    location /.well-known/acme-challenge/ {
        root /var/lib/liman/acme;
    }

    location / {
        return 301 https://$host$request_uri;
    }
}

server {
    listen 443 ssl;
    server_name api.example.com;
    add_header X-Liman-Release "r2" always;

    ssl_certificate     /certs/api.example.com/fullchain.pem;
    ssl_certificate_key /certs/api.example.com/privkey.pem;

    location / {
        proxy_pass http://liman-orders-api;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
`
	if string(got) != want {
		t.Errorf("Render() =\n%s\nwant\n%s", got, want)
	}
}

// newTestManager returns a manager that writes to a temporary directory and
// records commands instead of running them.
func newTestManager(t *testing.T) (*Manager, *[]string) {
	t.Helper()

	var commands []string
	m := New(config.Nginx{
		ConfDir:       filepath.Join(t.TempDir(), "conf"),
		UpstreamHost:  "127.0.0.1",
		TestCommand:   []string{"nginx", "-t"},
		ReloadCommand: []string{"nginx", "-s", "reload"},
	})
	m.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, strings.Join(append([]string{name}, args...), " "))
		return nil, nil
	}
	return m, &commands
}

func TestWriteCurrentRemove(t *testing.T) {
	m, _ := newTestManager(t)

	if got, err := m.Current("web"); err != nil || got != nil {
		t.Fatalf("Current() before any write = %q, %v; want nil, nil", got, err)
	}

	for _, content := range []string{"first\n", "second\n"} {
		if err := m.Write("web", []byte(content)); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		got, err := m.Current("web")
		if err != nil || string(got) != content {
			t.Errorf("Current() = %q, %v; want %q", got, err, content)
		}
	}

	info, err := os.Stat(m.Path("web"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("file mode = %o, want 644 so Nginx can read it", perm)
	}

	// No temporary files may be left next to the config.
	entries, err := os.ReadDir(filepath.Dir(m.Path("web")))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("config directory has %d entries, want only web.conf", len(entries))
	}

	// Remove must be repeatable.
	for range 2 {
		if err := m.Remove("web"); err != nil {
			t.Errorf("Remove() error = %v", err)
		}
	}
	if got, _ := m.Current("web"); got != nil {
		t.Errorf("Current() after Remove = %q, want nil", got)
	}
}

func TestRestore(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Write("web", []byte("new\n")); err != nil {
		t.Fatal(err)
	}

	if err := m.Restore("web", []byte("old\n")); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if got, _ := m.Current("web"); string(got) != "old\n" {
		t.Errorf("after Restore(content) = %q, want the old content", got)
	}

	// Restoring "there was no file" removes it.
	if err := m.Restore("web", nil); err != nil {
		t.Fatalf("Restore(nil) error = %v", err)
	}
	if got, _ := m.Current("web"); got != nil {
		t.Errorf("after Restore(nil) = %q, want no file", got)
	}
}

func TestTestAndReloadRunConfiguredCommands(t *testing.T) {
	m, commands := newTestManager(t)

	if err := m.Test(context.Background()); err != nil {
		t.Fatalf("Test() error = %v", err)
	}
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	want := []string{"nginx -t", "nginx -s reload"}
	if strings.Join(*commands, "|") != strings.Join(want, "|") {
		t.Errorf("commands = %v, want %v", *commands, want)
	}
}

func TestCommandFailureIncludesOutput(t *testing.T) {
	m, _ := newTestManager(t)
	exitErr := errors.New("exit status 1")
	m.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("nginx: [emerg] unexpected \"}\" in /etc/nginx/conf.d/web.conf:3\n"), exitErr
	}

	err := m.Test(context.Background())

	if !errors.Is(err, exitErr) {
		t.Fatalf("Test() error = %v, want it to wrap the command error", err)
	}
	if !strings.Contains(err.Error(), `unexpected "}"`) {
		t.Errorf("error %q does not include nginx's own message", err)
	}
}

func TestSite(t *testing.T) {
	certDir := t.TempDir()
	m := New(config.Nginx{ConfDir: "/conf", UpstreamHost: "host.docker.internal", CertDir: certDir, ACMERoot: "/acme"})

	// Without certificate files the site is plain HTTP.
	site := m.Site("web", "web.example.com", 40001, "r1")
	if site.Upstream != "host.docker.internal:40001" || site.TLS() || site.ACMERoot != "/acme" {
		t.Errorf("Site() = %+v, want an HTTP site for host.docker.internal:40001", site)
	}

	// Port 0 means the port is not known yet (dry run).
	if got := m.Site("web", "web.example.com", 0, "r1").Upstream; got != "host.docker.internal:<free port>" {
		t.Errorf("Upstream with unknown port = %q", got)
	}

	// Both files must exist for HTTPS; one alone is not enough.
	domainDir := filepath.Join(certDir, "web.example.com")
	if err := os.MkdirAll(domainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(domainDir, "fullchain.pem"), []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m.Site("web", "web.example.com", 40001, "r1").TLS() {
		t.Error("TLS enabled with only the certificate present")
	}
	if err := os.WriteFile(filepath.Join(domainDir, "privkey.pem"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

	site = m.Site("web", "web.example.com", 40001, "r1")
	if !site.TLS() || site.CertFile != filepath.Join(domainDir, "fullchain.pem") {
		t.Errorf("Site() = %+v, want TLS with the certificate from %s", site, domainDir)
	}
}

func TestExecRunnerCapturesOutputAndFailure(t *testing.T) {
	// A real process, to cover the one function the fakes replace.
	out, err := execRunner(context.Background(), "sh", "-c", "echo to-stdout; echo to-stderr >&2; exit 3")

	if err == nil {
		t.Fatal("execRunner() error = nil, want an exit error")
	}
	if got := string(out); !strings.Contains(got, "to-stdout") || !strings.Contains(got, "to-stderr") {
		t.Errorf("output = %q, want both streams", got)
	}
}

func TestReleaseOf(t *testing.T) {
	conf, err := Render(Site{App: "web", Domain: "web.example.com", Upstream: "127.0.0.1:1", Release: "20261119-183544"})
	if err != nil {
		t.Fatal(err)
	}

	if got := ReleaseOf(conf); got != "20261119-183544" {
		t.Errorf("ReleaseOf(rendered) = %q, want the release it was rendered with", got)
	}
	if got := ReleaseOf(nil); got != "" {
		t.Errorf("ReleaseOf(nil) = %q, want empty", got)
	}
	if got := ReleaseOf([]byte("server { listen 80; }")); got != "" {
		t.Errorf("ReleaseOf(foreign config) = %q, want empty", got)
	}
}

// probeServer starts an HTTP server that plays the role of Nginx: it reports
// a release per host name. release is a function so tests can change the
// answer while WaitRelease is polling.
func probeServer(t *testing.T, release func(host string) string) *Manager {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rel := release(r.Host); rel != "" {
			w.Header().Set(ReleaseHeader, rel)
		}
		// A redirect, as the HTTP listener of a TLS site answers. The probe
		// must read the header from this response and not follow it.
		http.Redirect(w, r, "https://"+r.Host+r.URL.Path, http.StatusMovedPermanently)
	}))
	t.Cleanup(srv.Close)

	m := New(config.Nginx{ProbeAddr: strings.TrimPrefix(srv.URL, "http://")})
	m.waitTimeout = 300 * time.Millisecond
	m.waitInterval = 5 * time.Millisecond
	return m
}

func TestWaitReleaseReturnsWhenReleaseIsServed(t *testing.T) {
	m := probeServer(t, func(host string) string {
		if host == "web.example.com" {
			return "r2"
		}
		return ""
	})

	if err := m.WaitRelease(context.Background(), "web.example.com", "r2"); err != nil {
		t.Errorf("WaitRelease(r2) error = %v", err)
	}
	// An empty release means "nothing of ours serves this domain".
	if err := m.WaitRelease(context.Background(), "other.example.com", ""); err != nil {
		t.Errorf("WaitRelease(empty) error = %v", err)
	}
}

func TestWaitReleaseWaitsForTheSwitch(t *testing.T) {
	// The handler runs on the server's goroutines while the test goroutine
	// reads the counter, so it must be accessed atomically.
	var requests atomic.Int32
	m := probeServer(t, func(string) string {
		// Like Nginx shortly after a reload: the first answers still come
		// from workers with the old config.
		if requests.Add(1) < 4 {
			return "r1"
		}
		return "r2"
	})

	if err := m.WaitRelease(context.Background(), "web.example.com", "r2"); err != nil {
		t.Fatalf("WaitRelease() error = %v", err)
	}
	if got := requests.Load(); got != 4 {
		t.Errorf("probe requests = %d, want 4", got)
	}
}

func TestWaitReleaseTimesOut(t *testing.T) {
	m := probeServer(t, func(string) string { return "r1" })

	err := m.WaitRelease(context.Background(), "web.example.com", "r2")

	if err == nil {
		t.Fatal("WaitRelease() error = nil, want a timeout")
	}
	for _, want := range []string{`release "r2"`, `it serves release "r1"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestWaitReleaseReportsUnreachableProxy(t *testing.T) {
	m := New(config.Nginx{ProbeAddr: "127.0.0.1:1"}) // nothing listens on port 1
	m.waitTimeout = 50 * time.Millisecond
	m.waitInterval = 5 * time.Millisecond

	err := m.WaitRelease(context.Background(), "web.example.com", "r2")

	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("WaitRelease() error = %v, want it to mention the refused connection", err)
	}
}

func TestFileOperationsRefusePathTraversal(t *testing.T) {
	m, _ := newTestManager(t)

	// A file outside the config directory that must stay untouched.
	outside := filepath.Join(filepath.Dir(filepath.Dir(m.Path("x"))), "victim.conf")
	if err := os.WriteFile(outside, []byte("precious\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// "../victim" + ".conf", joined to the config directory, is that file.
	hostile := "../victim"

	if _, err := m.Current(hostile); err == nil {
		t.Error("Current() accepted a path traversal name")
	}
	if err := m.Write(hostile, []byte("overwritten\n")); err == nil {
		t.Error("Write() accepted a path traversal name")
	}
	if err := m.Remove(hostile); err == nil {
		t.Error("Remove() accepted a path traversal name")
	}
	if err := m.Restore(hostile, nil); err == nil {
		t.Error("Restore() accepted a path traversal name")
	}

	if got, err := os.ReadFile(outside); err != nil || string(got) != "precious\n" {
		t.Errorf("the file outside the config directory is now %q (error %v)", got, err)
	}
}
