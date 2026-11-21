package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/applock"
	"github.com/onrcanogul/liman/internal/audit"
	"github.com/onrcanogul/liman/internal/certs"
	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/deploy"
	"github.com/onrcanogul/liman/internal/engine"
)

// issuer returns the certificate issuer config.yaml describes, or nil when
// acme is not set up.
func (d *Daemon) issuer(cfg config.Config) *certs.Issuer {
	if !cfg.ACME.Enabled() {
		return nil
	}
	key := cfg.ACME.AccountKey
	if key == "" {
		key = filepath.Join(d.Home, "acme-account.key")
	}
	return &certs.Issuer{
		Directory:  cfg.ACME.Directory,
		Email:      cfg.ACME.Email,
		CAFile:     cfg.ACME.CAFile,
		AccountKey: key,
		Webroot:    cfg.Proxy.ACMERoot,
		Store:      certs.Store{Dir: cfg.Proxy.CertDir},
	}
}

// deployCerts lets a deploy use an Issuer: deploy.Certs asks for less than
// the Issuer offers, and in its own words.
type deployCerts struct {
	is *certs.Issuer
}

func (c deployCerts) Has(domain string) bool { return c.is.Store.Has(domain) }

func (c deployCerts) Obtain(ctx context.Context, domain string, progress func(format string, args ...any)) error {
	_, err := c.is.Obtain(ctx, domain, progress)
	return err
}

// How often the renewer looks at the certificates. A check is cheap (it
// reads files and lists containers); the CA is only asked when something
// is due, so twice a day costs nothing and finds a failed renewal early.
const (
	certCheckFirst = time.Minute
	certCheckEvery = 12 * time.Hour
)

// certRenewer keeps certificates current: it renews those that are due and
// obtains one for a domain that is served without, because the attempt
// after its deploy failed.
type certRenewer struct {
	store   certs.Store
	obtain  func(ctx context.Context, domain string, progress certs.Progress) (certs.Info, error)
	proxy   deploy.Proxy
	connect func(ctx context.Context) (engine.Engine, error)
	locks   *applock.Locks
	log     *slog.Logger
	now     func() time.Time
	// audit records the certificates it obtains; nil records nothing.
	audit *audit.Recorder
}

// run checks after first and then every every, until ctx is cancelled.
func (r *certRenewer) run(ctx context.Context, first, every time.Duration) {
	wait := first
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		r.check(ctx)
		wait = every
	}
}

// check does one round: renewals, then domains without a certificate.
// Both work from the apps that exist, so it does nothing while Docker
// cannot be asked; with weeks of margin on every renewal, the next round
// is soon enough.
func (r *certRenewer) check(ctx context.Context) {
	eng, err := r.connect(ctx)
	if err != nil {
		r.log.Warn("certificates: cannot list the apps", "error", err.Error())
		return
	}
	containers, err := eng.List(ctx)
	if err != nil {
		r.log.Warn("certificates: cannot list the apps", "error", err.Error())
		return
	}
	r.renewDue(ctx, containers)
	r.issueMissing(ctx, containers)
}

// renewDue renews every certificate that is due and still used, then
// reloads Nginx once. The config does not change: it names the same files,
// which now hold the new certificate.
//
// Still used means some app has the domain. The certificate of an app that
// was stopped stays in cert_dir (deploying the app again uses it), but it
// is not renewed: once the domain points elsewhere every attempt would
// fail, and each failure counts against the CA's limits.
func (r *certRenewer) renewDue(ctx context.Context, containers []engine.Container) {
	// The app each domain is in use by, for the audit trail; a domain no
	// app uses is not renewed.
	inUse := map[string]string{}
	for _, c := range containers {
		if c.Domain != "" {
			inUse[c.Domain] = c.App
		}
	}
	list, err := r.store.List()
	if err != nil {
		r.log.Warn("certificates: cannot read all of them", "error", err.Error())
	}
	now := r.now()
	renewed := 0
	for _, info := range list {
		if !info.Due(now) || inUse[info.Domain] == "" {
			continue
		}
		r.log.Info("certificate: renewing", "domain", info.Domain, "expires", info.NotAfter.UTC().Format(time.RFC3339))
		got, err := r.obtain(ctx, info.Domain, nil)
		if err != nil {
			// The old certificate keeps working until it expires; the
			// next check tries again. Expired is worth saying louder.
			level := slog.LevelWarn
			if info.Expired(now) {
				level = slog.LevelError
			}
			r.log.Log(ctx, level, "certificate: renewal failed", "domain", info.Domain, "expires", info.NotAfter.UTC().Format(time.RFC3339), "error", err.Error())
			continue
		}
		r.log.Info("certificate: renewed", "domain", info.Domain, "expires", got.NotAfter.UTC().Format(time.RFC3339))
		// Only what changed is recorded: a failed renewal changes nothing
		// and is tried again at the next check, in the log each time.
		r.audit.Record(ctx, audit.Renewer, audit.CertObtain, inUse[info.Domain], info.Domain, nil)
		renewed++
	}
	if renewed == 0 {
		return
	}
	if err := r.apply(ctx); err != nil {
		r.log.Error("certificate: renewed, but the proxy did not take it", "proxy", r.proxy.Name(), "error", err.Error())
	}
}

// issueMissing obtains a certificate for every app that Nginx serves on a
// domain without one, and switches that app's config to HTTPS.
func (r *certRenewer) issueMissing(ctx context.Context, containers []engine.Container) {
	for _, c := range containers {
		if c.State != "running" || c.HostPort == 0 || !certs.Issuable(c.Domain) || r.store.Has(c.Domain) {
			continue
		}
		r.issueFor(ctx, c)
	}
}

func (r *certRenewer) issueFor(ctx context.Context, c engine.Container) {
	// A deploy, rollback or stop of the app may be under way; it decides
	// about the config then, and the next check comes back to it.
	unlock, ok := r.locks.TryLock(c.App)
	if !ok {
		return
	}
	defer unlock()

	// Only the release Nginx actually serves for the app: a container
	// that is running but not in service has nothing to switch.
	current, err := r.proxy.Current(c.App)
	if err != nil || current == nil || r.proxy.ReleaseOf(current) != c.Release {
		return
	}

	r.log.Info("certificate: obtaining", "app", c.App, "domain", c.Domain)
	if _, err := r.obtain(ctx, c.Domain, nil); err != nil {
		r.log.Warn("certificate: cannot obtain", "app", c.App, "domain", c.Domain, "error", err.Error())
		return
	}
	content, err := r.proxy.Render(c.App, c.Domain, c.HostPort, c.Release)
	if err == nil {
		err = r.proxy.Write(c.App, content)
	}
	if err == nil {
		if err = r.apply(ctx); err != nil {
			_ = r.proxy.Restore(c.App, current)
		}
	}
	if err != nil {
		r.log.Error("certificate: obtained, but the HTTPS config failed", "app", c.App, "domain", c.Domain, "error", err.Error())
		return
	}
	r.log.Info("certificate: HTTPS is on", "app", c.App, "domain", c.Domain)
	r.audit.Record(ctx, audit.Renewer, audit.CertObtain, c.App, c.Domain, nil)
}

// apply tests the Nginx config on disk and reloads it.
func (r *certRenewer) apply(ctx context.Context) error {
	if err := r.proxy.Test(ctx); err != nil {
		return err
	}
	return r.proxy.Reload(ctx)
}

const certSynopsis = "limand cert list | renew <domain>"

func (d *Daemon) runCert(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return d.usageError(certSynopsis)
	}
	cfg, err := config.Load(filepath.Join(d.Home, "config.yaml"))
	if err != nil {
		return d.fail(err)
	}
	switch {
	case args[0] == "list" && len(args) == 1:
		return d.certList(cfg)
	case args[0] == "renew" && len(args) == 2:
		return d.certRenew(ctx, cfg, args[1])
	default:
		return d.usageError(certSynopsis)
	}
}

func (d *Daemon) certList(cfg config.Config) int {
	if cfg.Proxy.CertDir == "" {
		return d.fail(fmt.Errorf("%s.cert_dir is not set in config.yaml, so there are no certificates to list", cfg.Proxy.Section()))
	}
	list, err := certs.Store{Dir: cfg.Proxy.CertDir}.List()
	if err != nil {
		// What could be read is still listed.
		fmt.Fprintf(d.Stderr, "limand: %v\n", err)
	}
	if len(list) == 0 {
		fmt.Fprintf(d.Stdout, "No certificates in %s.\n", cfg.Proxy.CertDir)
		if err != nil {
			return exitError
		}
		return exitOK
	}

	now := time.Now()
	w := tabwriter.NewWriter(d.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DOMAIN\tISSUER\tEXPIRES\tSTATUS")
	for _, c := range list {
		status := certStatus(c, now)
		if status == "expired" {
			status = "EXPIRED"
		}
		expires := c.NotAfter.Local().Format("2006-01-02 15:04")
		if !c.Expired(now) {
			expires += " (in " + roughly(c.NotAfter.Sub(now)) + ")"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Domain, c.Issuer, expires, status)
	}
	if err := w.Flush(); err != nil {
		return d.fail(err)
	}
	if err != nil {
		return exitError
	}
	return exitOK
}

// certStatus is "ok", "renewal due" or "expired", as in api.Certificate.
func certStatus(c certs.Info, now time.Time) string {
	switch {
	case c.Expired(now):
		return "expired"
	case c.Due(now):
		return "renewal due"
	default:
		return "ok"
	}
}

// certificatesForAPI is GET /v1/certs.
func certificatesForAPI(cfg config.Config, connect func(context.Context) (engine.Engine, error)) func(context.Context) (api.Certificates, error) {
	store := certs.Store{Dir: cfg.Proxy.CertDir}
	return func(ctx context.Context) (api.Certificates, error) {
		out := api.Certificates{ACME: cfg.ACME.Enabled(), Directory: cfg.ACME.Directory, Certificates: []api.Certificate{}}
		list, err := store.List()
		// A certificate that cannot be read is reported, not fatal: the
		// others are still worth showing.
		if err != nil {
			out.Errors = append(out.Errors, err.Error())
		}
		// Which domains an app has decides what the renewer looks after.
		// Without Docker that is unknown; every certificate then counts as
		// used, so that a warning is shown rather than hidden.
		inUse, known := map[string]bool{}, true
		if domains, err := appDomains(ctx, connect); err != nil {
			known = false
			out.Errors = append(out.Errors, "which apps use which certificate is unknown: "+err.Error())
		} else {
			inUse = domains
		}
		now := time.Now()
		for _, c := range list {
			out.Certificates = append(out.Certificates, api.Certificate{
				Domain: c.Domain, Names: c.Names, Issuer: c.Issuer,
				NotBefore: c.NotBefore, NotAfter: c.NotAfter, Status: certStatus(c, now),
				InUse: !known || inUse[c.Domain],
			})
		}
		return out, nil
	}
}

// appDomains returns the domains of every app container, running or not:
// the same set the renewer counts as in use.
func appDomains(ctx context.Context, connect func(context.Context) (engine.Engine, error)) (map[string]bool, error) {
	eng, err := connect(ctx)
	if err != nil {
		return nil, err
	}
	containers, err := eng.List(ctx)
	if err != nil {
		return nil, err
	}
	domains := map[string]bool{}
	for _, c := range containers {
		if c.Domain != "" {
			domains[c.Domain] = true
		}
	}
	return domains, nil
}

func (d *Daemon) certRenew(ctx context.Context, cfg config.Config, domain string) int {
	is := d.issuer(cfg)
	if is == nil {
		return d.fail(errors.New("acme is not set up in config.yaml (acme.directory), so liman cannot get certificates"))
	}
	if !certs.Issuable(domain) {
		return d.fail(fmt.Errorf("%q is not a domain a CA can issue a certificate for", domain))
	}
	had := is.Store.Has(domain)

	progress := func(format string, args ...any) { fmt.Fprintf(d.Stdout, "  "+format+"\n", args...) }
	info, err := is.Obtain(ctx, domain, progress)
	if err != nil {
		return d.fail(err)
	}

	proxy, err := newProxy(cfg)
	if err != nil {
		return d.fail(err)
	}
	if err := proxy.Test(ctx); err != nil {
		return d.fail(fmt.Errorf("the certificate is saved, but the %s config does not pass its test: %w", proxy.Name(), err))
	}
	if err := proxy.Reload(ctx); err != nil {
		return d.fail(fmt.Errorf("the certificate is saved, but reloading %s failed: %w", proxy.Name(), err))
	}
	fmt.Fprintf(d.Stdout, "Certificate for %s saved and %s reloaded; valid until %s.\n", domain, proxy.Name(), info.NotAfter.Local().Format("2006-01-02 15:04"))
	if !had {
		fmt.Fprintf(d.Stdout, "%s had no certificate before: its app switches to HTTPS at its next deploy, or at the daemon's next certificate check.\n", domain)
	}
	return exitOK
}
