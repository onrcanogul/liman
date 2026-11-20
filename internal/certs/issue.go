package certs

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/onrcanogul/liman/internal/app"
)

// Issuer obtains certificates from an ACME CA with the HTTP-01 challenge:
// the CA asks for a file under http://<domain>/.well-known/acme-challenge/,
// Issuer writes it into Webroot, and Nginx serves it from there.
type Issuer struct {
	// Directory is the CA's ACME directory URL.
	Directory string
	// Email is the account's contact address; may be empty.
	Email string
	// CAFile, when set, is a PEM file of further CAs to trust for the
	// connection to Directory (a test CA such as Pebble needs this).
	CAFile string
	// AccountKey is the file holding the account's private key. It is
	// created on first use.
	AccountKey string
	// Webroot is nginx.acme_root, the directory Nginx serves challenges from.
	Webroot string
	// Store is where certificates are written.
	Store Store

	// Timeout bounds one Obtain. Zero means two minutes.
	Timeout time.Duration

	// mu serialises Obtain: the deploy and the renewal loop may want a
	// certificate at the same time, and one account talking to the CA
	// twice at once gains nothing.
	mu sync.Mutex
}

// tokenRe is what an ACME token may look like: base64url without padding.
// The token becomes a file name; checking it means that a CA answering
// with "../../etc/x" cannot make liman write outside the webroot.
var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// Progress receives a line for each step of Obtain. It is an argument
// rather than a field because the deploy and the renewal loop share one
// Issuer and show progress in different places.
type Progress func(format string, args ...any)

// Obtain gets a new certificate for domain and saves it in the store. It
// does so whether or not a valid one exists: deciding when is the
// caller's business. progress may be nil.
func (is *Issuer) Obtain(ctx context.Context, domain string, progress Progress) (Info, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	if !app.ValidDomain(domain) {
		return Info{}, fmt.Errorf("%q is not a domain a certificate can be issued for", domain)
	}
	is.mu.Lock()
	defer is.mu.Unlock()

	timeout := is.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client, err := is.client(ctx)
	if err != nil {
		return Info{}, err
	}

	// 1. The order: "a certificate for these names, please".
	progress("ordering a certificate for %s from %s", domain, is.Directory)
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domain))
	if err != nil {
		return Info{}, fmt.Errorf("order: %w", err)
	}

	// 2. One authorization per name, each to be proven. One the CA still
	// remembers as valid (from an earlier order) needs no new proof.
	for _, url := range order.AuthzURLs {
		if err := is.authorize(ctx, client, url, progress); err != nil {
			return Info{}, err
		}
	}
	if order, err = client.WaitOrder(ctx, order.URI); err != nil {
		return Info{}, fmt.Errorf("wait for the order: %w", err)
	}

	// 3. The certificate's own key, made here and never sent anywhere. The
	// CA only gets the CSR: the public key and the name, signed with the
	// private key to show it is held.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Info{}, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{domain}}, key)
	if err != nil {
		return Info{}, err
	}
	progress("validated; asking for the certificate")
	// bundle=true: the chain comes with the intermediate, which is what
	// fullchain.pem holds. CreateCertFromOrder rather than CreateOrderCert:
	// the latter expects a Location header on the finalize answer, which
	// RFC 8555 does not require and Pebble does not send; this one polls
	// the order's own URL instead.
	chain, _, err := client.CreateCertFromOrder(ctx, order, csr, true)
	if err != nil {
		return Info{}, fmt.Errorf("finalize: %w", err)
	}
	if err := checkChain(chain, domain, key); err != nil {
		return Info{}, err
	}

	if err := is.Store.Save(domain, chain, key); err != nil {
		return Info{}, err
	}
	info, err := is.Store.Info(domain)
	if err != nil {
		return Info{}, err
	}
	progress("saved; valid until %s", info.NotAfter.UTC().Format("2006-01-02 15:04 MST"))
	return info, nil
}

// authorize proves control of the name behind one authorization URL.
func (is *Issuer) authorize(ctx context.Context, client *acme.Client, url string, progress Progress) error {
	z, err := client.GetAuthorization(ctx, url)
	if err != nil {
		return fmt.Errorf("authorization: %w", err)
	}
	if z.Status == acme.StatusValid {
		return nil
	}

	var chal *acme.Challenge
	for _, c := range z.Challenges {
		if c.Type == "http-01" {
			chal = c
			break
		}
	}
	if chal == nil {
		return fmt.Errorf("the CA offers no http-01 challenge for %s", z.Identifier.Value)
	}
	if !tokenRe.MatchString(chal.Token) {
		return fmt.Errorf("the CA sent a challenge token that is not a plain name: %q", chal.Token)
	}

	// The content is the token plus the fingerprint of our account key:
	// proof for this account, not for anybody who could put a file there.
	body, err := client.HTTP01ChallengeResponse(chal.Token)
	if err != nil {
		return err
	}
	path := filepath.Join(is.Webroot, filepath.FromSlash(client.HTTP01ChallengePath(chal.Token)))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("challenge directory: %w", err)
	}
	// Readable by Nginx's workers, which run as another user.
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write challenge: %w", err)
	}
	// Once the CA has looked, the file has no use; it is removed whatever
	// the outcome.
	defer func() { _ = os.Remove(path) }()

	progress("challenge for %s is at http://%s%s; asking the CA to check", z.Identifier.Value, z.Identifier.Value, client.HTTP01ChallengePath(chal.Token))
	if _, err := client.Accept(ctx, chal); err != nil {
		return fmt.Errorf("accept challenge: %w", err)
	}
	if _, err := client.WaitAuthorization(ctx, z.URI); err != nil {
		// The error carries the CA's reason, such as the address it could
		// not reach or the answer it got instead of the file.
		return fmt.Errorf("the CA could not verify %s: %w", z.Identifier.Value, err)
	}
	return nil
}

// checkChain makes sure the CA sent a certificate for domain and for our
// key, before it replaces anything that works.
func checkChain(chain [][]byte, domain string, key *ecdsa.PrivateKey) error {
	if len(chain) == 0 {
		return errors.New("the CA sent no certificate")
	}
	cert, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return fmt.Errorf("the CA sent a certificate that cannot be read: %w", err)
	}
	if err := cert.VerifyHostname(domain); err != nil {
		return fmt.Errorf("the CA sent a certificate for another name: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return errors.New("the CA sent a certificate for another key")
	}
	return nil
}

// client returns an ACME client with the account registered. Registering
// is repeated on every Obtain: for a key the CA knows it is one request
// that returns the existing account, and it means a new CA (say, staging
// to production) needs nothing done by hand.
func (is *Issuer) client(ctx context.Context) (*acme.Client, error) {
	key, err := is.accountKey()
	if err != nil {
		return nil, err
	}
	hc, err := httpClient(is.CAFile)
	if err != nil {
		return nil, err
	}
	client := &acme.Client{Key: key, DirectoryURL: is.Directory, HTTPClient: hc, UserAgent: "liman"}

	acct := &acme.Account{}
	if is.Email != "" {
		acct.Contact = []string{"mailto:" + is.Email}
	}
	// AcceptTOS agrees to the CA's terms of service. Setting acme.directory
	// in config.yaml is the operator's choice of CA, and with it of its
	// terms; the README says so.
	if _, err := client.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("ACME account at %s: %w", is.Directory, err)
	}
	return client, nil
}

// httpClient trusts the system's CAs and, if given, those in caFile.
func httpClient(caFile string) (*http.Client, error) {
	if caFile == "" {
		return &http.Client{Timeout: 30 * time.Second}, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	data, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("acme.ca_file: %w", err)
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("acme.ca_file %s holds no PEM certificate", caFile)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}, nil
}

// accountKey loads the account key, creating it on first use.
//
// The account key is the account: whoever holds it can order and revoke
// certificates for the account's names. It gets the same treatment as the
// master key: 0600, refused if others can read it.
func (is *Issuer) accountKey() (crypto.Signer, error) {
	info, err := os.Stat(is.AccountKey)
	if errors.Is(err, fs.ErrNotExist) {
		return createAccountKey(is.AccountKey)
	}
	if err != nil {
		return nil, fmt.Errorf("ACME account key: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("ACME account key %s is readable by others (mode %o); fix it with: chmod 600 %s", is.AccountKey, perm, is.AccountKey)
	}
	data, err := os.ReadFile(is.AccountKey)
	if err != nil {
		return nil, fmt.Errorf("ACME account key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || !strings.HasSuffix(block.Type, "PRIVATE KEY") {
		return nil, fmt.Errorf("ACME account key %s is not a PEM private key", is.AccountKey)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ACME account key %s: %w", is.AccountKey, err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("ACME account key %s cannot sign", is.AccountKey)
	}
	return signer, nil
}

func createAccountKey(path string) (crypto.Signer, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create ACME account key: %w", err)
	}
	// O_EXCL: two processes starting at once cannot both create a key and
	// end up with different accounts.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create ACME account key: %w", err)
	}
	writeErr := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("create ACME account key: %w", err)
	}
	return key, nil
}
