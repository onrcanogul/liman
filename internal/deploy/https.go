package deploy

import (
	"context"
	"fmt"
	"strings"
)

// Certs obtains certificates for the domains the proxy serves. It is
// declared here, by the package that uses it, like Proxy.
type Certs interface {
	// Has reports whether a certificate for domain is on disk, which is
	// what makes the proxy config switch the domain to HTTPS.
	Has(domain string) bool
	// Obtain gets a certificate for domain and puts it on disk. Each step
	// is reported to progress.
	Obtain(ctx context.Context, domain string, progress func(format string, args ...any)) error
}

// wantsCertificate reports whether a CA could issue a certificate for
// domain. Nothing can prove control of a .localhost name from outside, so
// asking would only fail; such a domain stays on plain HTTP quietly.
func wantsCertificate(domain string) bool {
	return domain != "" && domain != "localhost" && !strings.HasSuffix(domain, ".localhost")
}

// enableHTTPS gets a certificate for the app's domain if it has none yet,
// and then writes the config again, which now includes HTTPS.
//
// It runs after the pipeline, not as a step of it. A step that fails rolls
// the whole deploy back, and a certificate that could not be had (DNS not
// pointing here yet, port 80 closed, the CA's rate limit) is no reason to
// take a working release out of service. The app keeps running over plain
// HTTP, the output says why, and the daemon tries again later.
func (d *Deployer) enableHTTPS(ctx context.Context, st *state) {
	if d.Proxy == nil || d.Certs == nil || !wantsCertificate(st.domain) || d.Certs.Has(st.domain) {
		return
	}
	fmt.Fprintf(d.out, "==> HTTPS for %s\n", st.domain)
	err := d.Certs.Obtain(ctx, st.domain, d.detail)
	if err == nil {
		err = d.switchToHTTPS(ctx, st)
	}
	if err != nil {
		d.detail("HTTPS is not on: %v", err)
		d.detail("%s is served over plain HTTP. The daemon tries again at its next certificate check; deploying again also does.", st.domain)
		return
	}
	d.detail("https://%s is on", st.domain)
}

// switchToHTTPS writes the app's config again now that the certificate is
// on disk, and applies it. If Nginx does not accept the result, the config
// that was working is put back.
func (d *Deployer) switchToHTTPS(ctx context.Context, st *state) error {
	old, err := d.Proxy.Current(st.name)
	if err != nil {
		return err
	}
	content, err := d.Proxy.Render(st.name, st.domain, st.hostPort, st.release)
	if err != nil {
		return err
	}
	if err := d.Proxy.Write(st.name, content); err != nil {
		return err
	}
	if err := d.Proxy.Test(ctx); err != nil {
		if rerr := d.Proxy.Restore(st.name, old); rerr != nil {
			return fmt.Errorf("%w (and putting back the previous config failed: %v)", err, rerr)
		}
		return err
	}
	if err := d.Proxy.Reload(ctx); err != nil {
		return err
	}
	// The release does not change, so this returns as soon as Nginx
	// answers at all; it is a check that it still does.
	return d.Proxy.WaitRelease(ctx, st.domain, st.release)
}
