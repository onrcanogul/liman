package daemon

import (
	"context"
	"path/filepath"

	"github.com/onrcanogul/liman/internal/certs"
	"github.com/onrcanogul/liman/internal/config"
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
		Webroot:    cfg.Nginx.ACMERoot,
		Store:      certs.Store{Dir: cfg.Nginx.CertDir},
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
