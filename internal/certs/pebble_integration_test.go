//go:build integration

// This test obtains real certificates from Pebble, Let's Encrypt's ACME
// test server, running in Docker. It needs Docker and the image
// ghcr.io/letsencrypt/pebble:
//
//	go test -tags integration ./internal/certs
//	make test-integration

package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pebbleImage = "ghcr.io/letsencrypt/pebble:latest"

// pebble starts Pebble with its HTTP-01 checks sent to httpPort on this
// machine, for every name in names. It returns the directory URL and the
// file of the CA that signs Pebble's own HTTPS certificate.
func pebble(t *testing.T, httpPort int, names ...string) (directory, caFile string) {
	t.Helper()
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	dir := t.TempDir()
	config := fmt.Sprintf(`{"pebble": {
		"listenAddress": "0.0.0.0:14000",
		"managementListenAddress": "0.0.0.0:15000",
		"certificate": "test/certs/localhost/cert.pem",
		"privateKey": "test/certs/localhost/key.pem",
		"httpPort": %d, "tlsPort": 5001,
		"ocspResponderURL": "", "externalAccountBindingRequired": false}}`, httpPort)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	name := fmt.Sprintf("liman-pebble-test-%d", os.Getpid())
	_ = exec.Command("docker", "rm", "-f", name).Run()
	args := []string{"run", "-d", "--name", name, "-p", "127.0.0.1::14000",
		// No sleeping between checks, and no nonces rejected at random:
		// the test is about the flow, not about Pebble's patience tests.
		"-e", "PEBBLE_VA_NOSLEEP=1", "-e", "PEBBLE_WFE_NONCEREJECT=0",
		"-v", dir + ":/lab:ro"}
	// The names resolve, inside the container, to the machine running the
	// test, where the challenge server listens.
	for _, n := range names {
		args = append(args, "--add-host", n+":host-gateway")
	}
	args = append(args, pebbleImage, "-config", "/lab/config.json")
	docker(args...)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	// "127.0.0.1:55123" -> the published port of 14000.
	hostPort := docker("port", name, "14000/tcp")
	_, port, _ := net.SplitHostPort(strings.Split(hostPort, "\n")[0])

	caFile = filepath.Join(t.TempDir(), "pebble.minica.pem")
	docker("cp", name+":/test/certs/pebble.minica.pem", caFile)

	directory = "https://localhost:" + port + "/dir"
	client, err := httpClient(caFile)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := client.Get(directory)
		if err == nil {
			var dirDoc map[string]any
			decodeErr := json.NewDecoder(res.Body).Decode(&dirDoc)
			_ = res.Body.Close()
			if decodeErr == nil && dirDoc["newOrder"] != nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Pebble did not come up: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return directory, caFile
}

// challengeServer serves webroot the way Nginx does with root, on all
// interfaces (the container reaches it through the Docker host gateway).
func challengeServer(t *testing.T, webroot string) int {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.FileServer(http.Dir(webroot)), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func TestObtainFromPebble(t *testing.T) {
	const domain = "whoami.liman.test"
	home := t.TempDir()
	webroot := filepath.Join(home, "acme")
	port := challengeServer(t, webroot)
	directory, caFile := pebble(t, port, domain)

	is := &Issuer{
		Directory:  directory,
		Email:      "admin@liman.test",
		CAFile:     caFile,
		AccountKey: filepath.Join(home, "acme-account.key"),
		Webroot:    webroot,
		Store:      Store{Dir: filepath.Join(home, "certs")},
	}

	var lines []string
	progress := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	ctx := context.Background()
	first, err := is.Obtain(ctx, domain, progress)
	if err != nil {
		t.Fatalf("Obtain() error = %v\nprogress:\n%s", err, strings.Join(lines, "\n"))
	}
	if len(first.Names) != 1 || first.Names[0] != domain || !strings.HasPrefix(first.Issuer, "Pebble Intermediate CA") {
		t.Errorf("Obtain() = %+v", first)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "/.well-known/acme-challenge/") {
		t.Errorf("progress does not show the challenge:\n%s", strings.Join(lines, "\n"))
	}

	// What Nginx will load: the chain (leaf + intermediate) and its key.
	chain, keyPath := is.Store.Paths(domain)
	data, err := os.ReadFile(chain)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "BEGIN CERTIFICATE"); n != 2 {
		t.Errorf("fullchain.pem holds %d certificates, want 2 (leaf and intermediate)", n)
	}
	block, _ := pem.Decode(data)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(keyData)
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if priv, isEC := key.(*ecdsa.PrivateKey); !ok || !isEC || !pub.Equal(priv.Public()) {
		t.Error("privkey.pem does not belong to the certificate")
	}

	// The challenge file is gone again.
	if left, _ := filepath.Glob(filepath.Join(webroot, ".well-known", "acme-challenge", "*")); len(left) != 0 {
		t.Errorf("challenge files left behind: %v", left)
	}

	// Again, with the account that now exists: a new certificate.
	second, err := is.Obtain(ctx, domain, nil)
	if err != nil {
		t.Fatalf("second Obtain() error = %v", err)
	}
	if second.NotBefore.Before(first.NotBefore) {
		t.Errorf("second certificate %+v is older than the first %+v", second, first)
	}
	newData, _ := os.ReadFile(chain)
	if string(newData) == string(data) {
		t.Error("the second Obtain did not replace the certificate")
	}
}

func TestObtainFailsWhenTheChallengeIsNotServed(t *testing.T) {
	const domain = "nothing.liman.test"
	home := t.TempDir()
	// The server serves a directory the Issuer does not write to.
	port := challengeServer(t, filepath.Join(home, "elsewhere"))
	directory, caFile := pebble(t, port, domain)

	is := &Issuer{
		Directory:  directory,
		CAFile:     caFile,
		AccountKey: filepath.Join(home, "acme-account.key"),
		Webroot:    filepath.Join(home, "acme"),
		Store:      Store{Dir: filepath.Join(home, "certs")},
	}
	_, err := is.Obtain(context.Background(), domain, nil)
	if err == nil || !strings.Contains(err.Error(), "could not verify "+domain) {
		t.Fatalf("Obtain() error = %v, want the CA's refusal", err)
	}
	if is.Store.Has(domain) {
		t.Error("a certificate was saved although validation failed")
	}
}
