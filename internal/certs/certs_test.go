package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selfSigned returns a certificate for names valid from notBefore to
// notAfter, signed by its own key, and that key.
func selfSigned(t *testing.T, notBefore, notAfter time.Time, names ...string) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "Test CA"},
		Issuer:       pkix.Name{CommonName: "Test CA"},
		DNSNames:     names,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

func TestStoreSaveAndRead(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "certs")}
	now := time.Now().Truncate(time.Second)
	der, key := selfSigned(t, now, now.Add(90*24*time.Hour), "whoami.example.com")

	if s.Has("whoami.example.com") {
		t.Fatal("Has() before Save")
	}
	if _, err := s.Info("whoami.example.com"); !errors.Is(err, ErrNoCertificate) {
		t.Errorf("Info() before Save error = %v, want ErrNoCertificate", err)
	}

	// Two certificates: the leaf first, then (here: the same again, as a
	// stand-in) the intermediate.
	if err := s.Save("whoami.example.com", [][]byte{der, der}, key); err != nil {
		t.Fatal(err)
	}
	if !s.Has("whoami.example.com") {
		t.Error("Has() after Save = false")
	}
	info, err := s.Info("whoami.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if info.Domain != "whoami.example.com" || info.Issuer != "Test CA" || !info.NotAfter.Equal(now.Add(90*24*time.Hour)) || len(info.Names) != 1 || info.Names[0] != "whoami.example.com" {
		t.Errorf("Info() = %+v", info)
	}

	chain, keyPath := s.Paths("whoami.example.com")
	data, _ := os.ReadFile(chain)
	if n := strings.Count(string(data), "BEGIN CERTIFICATE"); n != 2 {
		t.Errorf("fullchain.pem holds %d certificates, want 2", n)
	}
	for path, want := range map[string]os.FileMode{chain: 0o644, keyPath: 0o600} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s mode = %v, %v; want %o", path, st.Mode().Perm(), err, want)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(chain), ".*"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}

	if err := s.Save("../escape", [][]byte{der}, key); err == nil {
		t.Error("Save() accepted a name that is not a domain")
	}
}

func TestStoreList(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: dir}
	now := time.Now()
	for _, d := range []string{"b.example.com", "a.example.com"} {
		der, key := selfSigned(t, now, now.Add(time.Hour), d)
		if err := s.Save(d, [][]byte{der}, key); err != nil {
			t.Fatal(err)
		}
	}
	// Not ours, or not complete: skipped without complaint.
	_ = os.MkdirAll(filepath.Join(dir, "not a domain"), 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "empty.example.com"), 0o755)
	// A broken certificate is reported, and the rest is still listed.
	_ = os.MkdirAll(filepath.Join(dir, "broken.example.com"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "broken.example.com", "fullchain.pem"), []byte("garbage"), 0o644)

	list, err := s.List()
	if err == nil || !strings.Contains(err.Error(), "broken.example.com") {
		t.Errorf("List() error = %v, want one about broken.example.com", err)
	}
	if len(list) != 2 || list[0].Domain != "a.example.com" || list[1].Domain != "b.example.com" {
		t.Errorf("List() = %+v, want a and b, in that order", list)
	}

	if list, err := (Store{Dir: filepath.Join(dir, "missing")}).List(); err != nil || len(list) != 0 {
		t.Errorf("List() of a missing directory = %v, %v; want nothing", list, err)
	}
}

func TestInfoDue(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	info := Info{NotBefore: start, NotAfter: start.Add(90 * 24 * time.Hour)}
	tests := []struct {
		at      time.Duration // after NotBefore
		due     bool
		expired bool
	}{
		{0, false, false},
		{59 * 24 * time.Hour, false, false},
		{60*24*time.Hour + time.Second, true, false}, // less than 30 days left
		{89 * 24 * time.Hour, true, false},
		{90 * 24 * time.Hour, true, true},
	}
	for _, tt := range tests {
		now := start.Add(tt.at)
		if got := info.Due(now); got != tt.due {
			t.Errorf("Due at +%v = %v, want %v", tt.at, got, tt.due)
		}
		if got := info.Expired(now); got != tt.expired {
			t.Errorf("Expired at +%v = %v, want %v", tt.at, got, tt.expired)
		}
	}
}

func TestAccountKeyIsCreatedOnceAndKeptPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "acme-account.key")
	is := &Issuer{AccountKey: path}

	first, err := is.accountKey()
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("account key mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}
	second, err := is.accountKey()
	if err != nil {
		t.Fatal(err)
	}
	if !first.Public().(*ecdsa.PublicKey).Equal(second.Public()) {
		t.Error("the account key changed between two loads")
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := is.accountKey(); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Errorf("accountKey() of a 0644 file error = %v, want a refusal", err)
	}
}

func TestCheckChain(t *testing.T) {
	now := time.Now()
	der, key := selfSigned(t, now, now.Add(time.Hour), "whoami.example.com")
	_, otherKey := selfSigned(t, now, now.Add(time.Hour), "whoami.example.com")

	if err := checkChain([][]byte{der}, "whoami.example.com", key); err != nil {
		t.Errorf("checkChain() of a good chain = %v", err)
	}
	if err := checkChain([][]byte{der}, "other.example.com", key); err == nil {
		t.Error("checkChain() accepted a certificate for another name")
	}
	if err := checkChain([][]byte{der}, "whoami.example.com", otherKey); err == nil {
		t.Error("checkChain() accepted a certificate for another key")
	}
	if err := checkChain(nil, "whoami.example.com", key); err == nil {
		t.Error("checkChain() accepted an empty chain")
	}
}

func TestTokenMustBeAPlainName(t *testing.T) {
	for token, ok := range map[string]bool{
		"FT3WMjCt_b8fWDk-9GKOZddk_gN_prPjHoIfKtFne2s": true,
		"../../etc/passwd": false,
		"a/b":              false,
		"":                 false,
		"with space":       false,
	} {
		if got := tokenRe.MatchString(token); got != ok {
			t.Errorf("token %q accepted = %v, want %v", token, got, ok)
		}
	}
}
