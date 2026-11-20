package daemon

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/applock"
	"github.com/onrcanogul/liman/internal/certs"
	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/engine/enginetest"
)

// saveCert puts a self-signed certificate for domain into the store.
func saveCert(t *testing.T, s certs.Store, domain string, notBefore, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "Test CA"},
		DNSNames:     []string{domain},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(domain, [][]byte{der}, key); err != nil {
		t.Fatal(err)
	}
}

// renewProxy is a deploy.Proxy that keeps configs in memory and records
// what was done to Nginx.
type renewProxy struct {
	files   map[string]string
	store   certs.Store
	calls   []string
	testErr error
}

func (p *renewProxy) Path(app string) string { return "/etc/nginx/liman/" + app + ".conf" }
func (p *renewProxy) TestCommand() string    { return "nginx -t" }
func (p *renewProxy) ReloadCommand() string  { return "nginx -s reload" }
func (p *renewProxy) ReleaseOf(c []byte) string {
	first, _, _ := strings.Cut(string(c), ";")
	return strings.TrimPrefix(first, "release ")
}
func (p *renewProxy) Render(_, domain string, port int, release string) ([]byte, error) {
	conf := fmt.Sprintf("release %s;\nserver_name %s;\nproxy_pass 127.0.0.1:%d;\n", release, domain, port)
	if p.store.Has(domain) {
		conf += "listen 443 ssl;\n"
	}
	return []byte(conf), nil
}
func (p *renewProxy) Current(app string) ([]byte, error) {
	c, ok := p.files[app]
	if !ok {
		return nil, nil
	}
	return []byte(c), nil
}
func (p *renewProxy) Write(app string, c []byte) error {
	p.files[app] = string(c)
	p.calls = append(p.calls, "write "+app)
	return nil
}
func (p *renewProxy) Restore(app string, c []byte) error {
	p.calls = append(p.calls, "restore "+app)
	if c == nil {
		delete(p.files, app)
	} else {
		p.files[app] = string(c)
	}
	return nil
}
func (p *renewProxy) Test(context.Context) error                        { p.calls = append(p.calls, "test"); return p.testErr }
func (p *renewProxy) Reload(context.Context) error                      { p.calls = append(p.calls, "reload"); return nil }
func (p *renewProxy) WaitRelease(context.Context, string, string) error { return nil }

// testRenewer returns a renewer whose CA hands out a 90-day certificate
// for every domain asked for (or fails with failWith), and the log.
func testRenewer(t *testing.T, td *testDaemon, now time.Time, failWith error) (*certRenewer, *renewProxy, *[]string, *bytes.Buffer) {
	t.Helper()
	store := certs.Store{Dir: filepath.Join(t.TempDir(), "certs")}
	proxy := &renewProxy{files: map[string]string{}, store: store}
	var asked []string
	var log bytes.Buffer
	r := &certRenewer{
		store: store,
		obtain: func(_ context.Context, domain string, _ certs.Progress) (certs.Info, error) {
			asked = append(asked, domain)
			if failWith != nil {
				return certs.Info{}, failWith
			}
			saveCert(t, store, domain, now, now.Add(90*24*time.Hour))
			return store.Info(domain)
		},
		proxy:   proxy,
		connect: func(context.Context) (engine.Engine, error) { return td.engine, nil },
		locks:   &applock.Locks{},
		log:     slog.New(slog.NewTextHandler(&log, nil)),
		now:     func() time.Time { return now },
	}
	return r, proxy, &asked, &log
}

func TestRenewerRenewsWhatIsDueAndReloadsOnce(t *testing.T) {
	td := newTestDaemon(t)
	now := time.Now()
	r, proxy, asked, log := testRenewer(t, td, now, nil)
	// 80 of 90 days gone: due. 10 of 90 gone: not.
	saveCert(t, r.store, "old.example.com", now.Add(-80*24*time.Hour), now.Add(10*24*time.Hour))
	saveCert(t, r.store, "new.example.com", now.Add(-10*24*time.Hour), now.Add(80*24*time.Hour))
	saveCert(t, r.store, "older.example.com", now.Add(-85*24*time.Hour), now.Add(5*24*time.Hour))
	// Due, but its app was stopped: left alone.
	saveCert(t, r.store, "stopped.example.com", now.Add(-85*24*time.Hour), now.Add(5*24*time.Hour))
	for i, d := range []string{"old.example.com", "new.example.com", "older.example.com"} {
		// The app's previous release, stopped, still counts as the app.
		td.engine.Add(engine.Container{Name: fmt.Sprintf("app%d-r1", i), App: fmt.Sprintf("app%d", i), State: "exited", Domain: d})
	}

	r.check(context.Background())

	if want := []string{"old.example.com", "older.example.com"}; !slices.Equal(*asked, want) {
		t.Errorf("renewed %v, want %v", *asked, want)
	}
	if want := []string{"test", "reload"}; !slices.Equal(proxy.calls, want) {
		t.Errorf("Nginx calls = %v, want one test and one reload", proxy.calls)
	}
	if info, _ := r.store.Info("old.example.com"); !info.NotAfter.After(now.Add(80 * 24 * time.Hour)) {
		t.Errorf("old.example.com still expires %v", info.NotAfter)
	}
	if !strings.Contains(log.String(), `msg="certificate: renewed" domain=old.example.com`) {
		t.Errorf("the log does not say what was renewed:\n%s", log.String())
	}
}

func TestRenewerKeepsTheOldCertificateWhenRenewalFails(t *testing.T) {
	td := newTestDaemon(t)
	now := time.Now()
	r, proxy, _, log := testRenewer(t, td, now, errors.New("the CA could not verify old.example.com"))
	saveCert(t, r.store, "old.example.com", now.Add(-80*24*time.Hour), now.Add(10*24*time.Hour))
	saveCert(t, r.store, "gone.example.com", now.Add(-91*24*time.Hour), now.Add(-time.Hour))
	td.engine.Add(engine.Container{Name: "old-r1", App: "old", State: "running", Domain: "old.example.com"})
	td.engine.Add(engine.Container{Name: "gone-r1", App: "gone", State: "running", Domain: "gone.example.com"})

	r.check(context.Background())

	if len(proxy.calls) != 0 {
		t.Errorf("Nginx was touched although nothing was renewed: %v", proxy.calls)
	}
	if info, _ := r.store.Info("old.example.com"); !info.NotAfter.Equal(now.Add(10 * 24 * time.Hour).Truncate(time.Second)) {
		t.Errorf("the old certificate changed: %+v", info)
	}
	if !strings.Contains(log.String(), `level=WARN msg="certificate: renewal failed" domain=old.example.com`) ||
		!strings.Contains(log.String(), `level=ERROR msg="certificate: renewal failed" domain=gone.example.com`) {
		t.Errorf("the log does not report the failures, louder for the expired one:\n%s", log.String())
	}
}

func TestRenewerSwitchesAnAppWithoutCertificateToHTTPS(t *testing.T) {
	td := newTestDaemon(t)
	now := time.Now()
	r, proxy, asked, _ := testRenewer(t, td, now, nil)

	// web is in service on plain HTTP; its first attempt failed.
	td.engine.Add(engine.Container{Name: "web-r2", App: "web", State: "running", HostPort: 40001, Release: "r2", Domain: "web.example.com"})
	proxy.files["web"] = "release r2;\nserver_name web.example.com;\nproxy_pass 127.0.0.1:40001;\n"
	// The previous release of api is still running but not in service.
	td.engine.Add(engine.Container{Name: "api-r1", App: "api", State: "running", HostPort: 40002, Release: "r1", Domain: "api.example.com"})
	proxy.files["api"] = "release r2;\nserver_name api.example.com;\nproxy_pass 127.0.0.1:40003;\n"
	// A .localhost domain cannot be validated.
	td.engine.Add(engine.Container{Name: "dev-r1", App: "dev", State: "running", HostPort: 40004, Release: "r1", Domain: "dev.localhost"})
	proxy.files["dev"] = "release r1;\nserver_name dev.localhost;\n"
	// busy is being deployed right now.
	td.engine.Add(engine.Container{Name: "busy-r1", App: "busy", State: "running", HostPort: 40005, Release: "r1", Domain: "busy.example.com"})
	proxy.files["busy"] = "release r1;\nserver_name busy.example.com;\n"
	unlock, _ := r.locks.TryLock("busy")
	defer unlock()

	r.check(context.Background())

	if want := []string{"web.example.com"}; !slices.Equal(*asked, want) {
		t.Errorf("asked for %v, want %v", *asked, want)
	}
	if conf := proxy.files["web"]; !strings.Contains(conf, "listen 443 ssl;") || !strings.Contains(conf, "release r2;") {
		t.Errorf("web's config = %q, want r2 with HTTPS", conf)
	}
	if want := []string{"write web", "test", "reload"}; !slices.Equal(proxy.calls, want) {
		t.Errorf("Nginx calls = %v, want %v", proxy.calls, want)
	}
}

func TestRenewerPutsTheConfigBackWhenNginxRejectsIt(t *testing.T) {
	td := newTestDaemon(t)
	r, proxy, _, log := testRenewer(t, td, time.Now(), nil)
	proxy.testErr = errors.New("ssl_certificate failed")
	td.engine.Add(engine.Container{Name: "web-r2", App: "web", State: "running", HostPort: 40001, Release: "r2", Domain: "web.example.com"})
	before := "release r2;\nserver_name web.example.com;\nproxy_pass 127.0.0.1:40001;\n"
	proxy.files["web"] = before

	r.check(context.Background())

	if proxy.files["web"] != before {
		t.Errorf("web's config = %q, want the working one back", proxy.files["web"])
	}
	if !strings.Contains(log.String(), "the HTTPS config failed") {
		t.Errorf("the log does not say so:\n%s", log.String())
	}
}

func TestCertList(t *testing.T) {
	td := newTestDaemon(t)
	certDir := filepath.Join(td.Home, "certs")
	writeConfig(t, td.Home, "nginx:\n  conf_dir: "+filepath.Join(td.Home, "nginx")+"\n  cert_dir: "+certDir+"\n")
	store := certs.Store{Dir: certDir}
	now := time.Now()
	saveCert(t, store, "a.example.com", now.Add(-time.Hour), now.Add(90*24*time.Hour))
	saveCert(t, store, "b.example.com", now.Add(-80*24*time.Hour), now.Add(10*24*time.Hour))
	saveCert(t, store, "c.example.com", now.Add(-91*24*time.Hour), now.Add(-time.Hour))

	if code := td.Run(context.Background(), []string{"cert", "list"}); code != exitOK {
		t.Fatalf("cert list: code %d, stderr %q", code, td.stderr.String())
	}
	out := td.stdout.String()
	for _, re := range []string{
		`(?m)^DOMAIN\s+ISSUER\s+EXPIRES\s+STATUS$`,
		`(?m)^a\.example\.com\s+Test CA\s+\S+ \S+ \(in 90 days\)\s+ok$`,
		`(?m)^b\.example\.com\s+Test CA\s+\S+ \S+ \(in 10 days\)\s+renewal due$`,
		`(?m)^c\.example\.com\s+Test CA\s+\S+ \S+\s+EXPIRED$`,
	} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("cert list does not match %s:\n%s", re, out)
		}
	}
}

func TestCertRenewNeedsACME(t *testing.T) {
	td := newTestDaemon(t)
	if code := td.Run(context.Background(), []string{"cert", "renew", "web.example.com"}); code != exitError || !strings.Contains(td.stderr.String(), "acme is not set up") {
		t.Errorf("cert renew without acme: code %d, stderr %q", code, td.stderr.String())
	}
	if code := td.Run(context.Background(), []string{"cert"}); code != exitUsage {
		t.Errorf("cert without a subcommand: code %d, want %d", code, exitUsage)
	}
}

func TestCertificatesForAPI(t *testing.T) {
	dir := t.TempDir()
	store := certs.Store{Dir: dir}
	now := time.Now()
	saveCert(t, store, "a.example.com", now.Add(-time.Hour), now.Add(90*24*time.Hour))
	saveCert(t, store, "b.example.com", now.Add(-80*24*time.Hour), now.Add(10*24*time.Hour))
	saveCert(t, store, "c.example.com", now.Add(-91*24*time.Hour), now.Add(-time.Hour))

	// a and b belong to apps (b's only container is stopped); c to none.
	fake := enginetest.New()
	fake.Add(engine.Container{Name: "a-r1", App: "a", State: "running", Domain: "a.example.com"})
	fake.Add(engine.Container{Name: "b-r1", App: "b", State: "exited", Domain: "b.example.com"})
	connect := func(context.Context) (engine.Engine, error) { return fake, nil }

	cfg := config.Config{Nginx: config.Nginx{CertDir: dir}, ACME: config.ACME{Directory: "https://ca.example/dir"}}
	out, err := certificatesForAPI(cfg, connect)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range out.Certificates {
		got = append(got, fmt.Sprintf("%s %s %v", c.Domain, c.Status, c.InUse))
	}
	if want := []string{"a.example.com ok true", "b.example.com renewal due true", "c.example.com expired false"}; !slices.Equal(got, want) || !out.ACME || out.Directory != "https://ca.example/dir" || len(out.Errors) != 0 {
		t.Errorf("certificates = %v (acme %v, %q), want %v", got, out.ACME, out.Directory, want)
	}
}

func TestCertificatesForAPIWithoutDocker(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	saveCert(t, certs.Store{Dir: dir}, "a.example.com", now.Add(-91*24*time.Hour), now.Add(-time.Hour))
	connect := func(context.Context) (engine.Engine, error) { return nil, errors.New("docker is down") }

	out, err := certificatesForAPI(config.Config{Nginx: config.Nginx{CertDir: dir}}, connect)(context.Background())
	if err != nil || len(out.Certificates) != 1 || !out.Certificates[0].InUse || len(out.Errors) != 1 {
		t.Errorf("certificates without Docker = %+v, %v; want the certificate counted as used, and the reason", out, err)
	}
}
