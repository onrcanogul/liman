// Package certs obtains TLS certificates over ACME (RFC 8555), the protocol
// Let's Encrypt speaks, and keeps them where Nginx reads them.
//
// A certificate lives in <dir>/<domain>/ as two files: fullchain.pem (the
// certificate followed by the intermediate that signed it) and privkey.pem
// (its private key, which never leaves the machine). That is the layout
// the Nginx config liman writes already expects.
package certs

import (
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/onrcanogul/liman/internal/app"
)

// Issuable reports whether a CA could issue a certificate for domain.
// Nothing can prove control of a .localhost name from outside, so asking
// would only fail; such domains stay on plain HTTP quietly.
func Issuable(domain string) bool {
	return domain != "" && domain != "localhost" && !strings.HasSuffix(domain, ".localhost") && app.ValidDomain(domain)
}

// ErrNoCertificate is returned for a domain without a certificate on disk.
var ErrNoCertificate = errors.New("no certificate")

// Store is the directory of certificates, nginx.cert_dir.
type Store struct {
	Dir string
}

// Paths returns the certificate chain and key files of domain.
func (s Store) Paths(domain string) (chain, key string) {
	dir := filepath.Join(s.Dir, domain)
	return filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
}

// Info describes a certificate on disk.
type Info struct {
	Domain    string
	Names     []string // the names the certificate is valid for (SAN)
	Issuer    string
	NotBefore time.Time
	NotAfter  time.Time
}

// Expired reports whether the certificate is no longer valid at now.
func (i Info) Expired(now time.Time) bool {
	return !now.Before(i.NotAfter)
}

// Due reports whether the certificate should be renewed at now: when less
// than a third of its lifetime is left. For the 90 days Let's Encrypt
// gives, that is 30 days before the end, which leaves weeks of retries if
// renewing fails; and the rule follows along when lifetimes get shorter.
func (i Info) Due(now time.Time) bool {
	lifetime := i.NotAfter.Sub(i.NotBefore)
	return i.NotAfter.Sub(now) < lifetime/3
}

// Has reports whether both files of domain's certificate are on disk. It
// is the same test the Nginx config uses to decide on HTTPS.
func (s Store) Has(domain string) bool {
	chain, key := s.Paths(domain)
	return isFile(chain) && isFile(key)
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Info reads the certificate of domain. It returns ErrNoCertificate if
// there is none.
func (s Store) Info(domain string) (Info, error) {
	chain, _ := s.Paths(domain)
	data, err := os.ReadFile(chain)
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, fmt.Errorf("%s: %w", domain, ErrNoCertificate)
	}
	if err != nil {
		return Info{}, err
	}
	// The first block is the certificate itself; what follows is the
	// chain up to (not including) the root.
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return Info{}, fmt.Errorf("%s: not a PEM certificate", chain)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return Info{}, fmt.Errorf("%s: %w", chain, err)
	}
	return Info{
		Domain:    domain,
		Names:     cert.DNSNames,
		Issuer:    cert.Issuer.CommonName,
		NotBefore: cert.NotBefore,
		NotAfter:  cert.NotAfter,
	}, nil
}

// List returns the certificates in the store, by domain. A directory that
// is not named like a domain, or holds no certificate, is skipped; one
// whose certificate cannot be read is reported in the error and skipped.
func (s Store) List() ([]Info, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !app.ValidDomain(e.Name()) {
			continue
		}
		info, err := s.Info(e.Name())
		if errors.Is(err, ErrNoCertificate) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, errors.Join(errs...)
}

// Save writes the certificate chain (DER, the certificate first) and its
// key for domain.
//
// Each file is replaced atomically, so Nginx never reads half a file. The
// two renames are not one step, though: a reload that happened exactly
// between them would see the new key with the old certificate and refuse
// the config. The key is written second so that the window is as short as
// it can be, and the caller reloads Nginx only after Save has returned.
func (s Store) Save(domain string, chain [][]byte, key crypto.Signer) error {
	if !app.ValidDomain(domain) {
		return fmt.Errorf("%q is not a domain", domain)
	}
	if len(chain) == 0 {
		return errors.New("save certificate: empty chain")
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("save certificate: %w", err)
	}

	var chainPEM strings.Builder
	for _, c := range chain {
		if err := pem.Encode(&chainPEM, &pem.Block{Type: "CERTIFICATE", Bytes: c}); err != nil {
			return err
		}
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	chainPath, keyPath := s.Paths(domain)
	// Nginx's master process runs as root and reads the files itself;
	// nobody else needs the key.
	if err := os.MkdirAll(filepath.Dir(chainPath), 0o755); err != nil {
		return fmt.Errorf("create certificate directory: %w", err)
	}
	if err := writeAtomic(chainPath, []byte(chainPEM.String()), 0o644); err != nil {
		return err
	}
	return writeAtomic(keyPath, keyPEM, 0o600)
}

// writeAtomic writes data to a temporary file next to path and renames it
// into place.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	_, writeErr := tmp.Write(data)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
