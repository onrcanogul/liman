package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/config"
)

// testDialect is the smallest dialect: one line per setting. The Manager
// must work the same whatever the file looks like.
type testDialect struct {
	watches bool // no commands: the proxy watches its files
	tls     bool // obtains certificates itself
}

func (testDialect) Name() string { return "Test" }
func (testDialect) Ext() string  { return ".conf" }
func (testDialect) Render(s Site) ([]byte, error) {
	return fmt.Appendf(nil, "domain %s\nupstream %s\nrelease %q\nautotls %v\n", s.Domain, s.Upstream, s.Release, s.AutoTLS), nil
}

var testReleaseRe = regexp.MustCompile(`release "([^"]*)"`)

func (testDialect) ReleaseOf(content []byte) string {
	if m := testReleaseRe.FindSubmatch(content); m != nil {
		return string(m[1])
	}
	return ""
}
func (d testDialect) Commands() (test, reload []string) {
	if d.watches {
		return nil, nil
	}
	return []string{"proxy", "-t"}, []string{"proxy", "-s", "reload"}
}
func (d testDialect) ManagesCertificates() bool { return d.tls }

// newTestManager returns a manager that writes to a temporary directory and
// records commands instead of running them.
func newTestManager(t *testing.T) (*Manager, *[]string) {
	t.Helper()

	var commands []string
	m := New(config.Proxy{
		ConfDir:      filepath.Join(t.TempDir(), "conf"),
		UpstreamHost: "127.0.0.1",
	}, testDialect{})
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
		t.Errorf("file mode = %o, want 644 so the proxy can read it", perm)
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

	// The dialect's commands, as config.yaml gives none.
	want := []string{"proxy -t", "proxy -s reload"}
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
	m := New(config.Proxy{ConfDir: "/conf", UpstreamHost: "host.docker.internal", CertDir: certDir, ACMERoot: "/acme"}, testDialect{})

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

// probeServer starts an HTTP server that plays the role of the proxy: it reports
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

	m := New(config.Proxy{ProbeAddr: strings.TrimPrefix(srv.URL, "http://")}, testDialect{})
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
	m := New(config.Proxy{ProbeAddr: "127.0.0.1:1"}, testDialect{}) // nothing listens on port 1
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

func TestConfiguredCommandsWinOverTheDialects(t *testing.T) {
	m := New(config.Proxy{ConfDir: t.TempDir(), ReloadCommand: []string{"sudo", "-n", "systemctl", "reload", "proxy"}}, testDialect{})
	if got := m.TestCommand(); got != "proxy -t" {
		t.Errorf("TestCommand() = %q, want the dialect's", got)
	}
	if got := m.ReloadCommand(); got != "sudo -n systemctl reload proxy" {
		t.Errorf("ReloadCommand() = %q, want the configured one", got)
	}
}

func TestAProxyThatWatchesItsFilesRunsNothing(t *testing.T) {
	m, commands := newTestManager(t)
	m.dialect = testDialect{watches: true}
	m.cfg.TestCommand, m.cfg.ReloadCommand = nil, nil

	if err := m.Test(context.Background()); err != nil {
		t.Errorf("Test() error = %v", err)
	}
	if err := m.Reload(context.Background()); err != nil {
		t.Errorf("Reload() error = %v", err)
	}
	if len(*commands) != 0 || m.TestCommand() != "" || m.ReloadCommand() != "" {
		t.Errorf("ran %v, commands %q and %q; want nothing", *commands, m.TestCommand(), m.ReloadCommand())
	}
}

func TestAutoTLSOnlyForAProxyWithCertificatesAndAPublicName(t *testing.T) {
	tests := []struct {
		tls    bool
		domain string
		want   bool
	}{
		{true, "api.example.com", true},
		{true, "web.localhost", false}, // no public CA issues for it
		{false, "api.example.com", false},
	}
	for _, tt := range tests {
		m := New(config.Proxy{ConfDir: "/conf"}, testDialect{tls: tt.tls})
		if got := m.Site("web", tt.domain, 1, "r1").AutoTLS; got != tt.want {
			t.Errorf("tls=%v %s: AutoTLS = %v, want %v", tt.tls, tt.domain, got, tt.want)
		}
	}
}

func TestPathUsesTheDialectsExtension(t *testing.T) {
	m := New(config.Proxy{ConfDir: "/etc/proxy/liman.d"}, testDialect{})
	if got := m.Path("web"); got != "/etc/proxy/liman.d/web.conf" {
		t.Errorf("Path() = %q", got)
	}
}
