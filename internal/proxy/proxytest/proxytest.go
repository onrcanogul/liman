// Package proxytest holds the tests every proxy.Dialect must pass. A new
// dialect is ready when it passes them; its own tests then only need to
// show what its files look like.
package proxytest

import (
	"bytes"
	"strings"
	"testing"

	"github.com/onrcanogul/liman/internal/proxy"
)

// Contract checks what a proxy.Manager relies on in d.
func Contract(t *testing.T, d proxy.Dialect) {
	t.Helper()

	if d.Name() == "" {
		t.Error("Name() is empty; it names the proxy in every step")
	}
	if ext := d.Ext(); !strings.HasPrefix(ext, ".") || len(ext) < 2 || strings.ContainsAny(ext[1:], "./\\") {
		t.Errorf("Ext() = %q, want an extension such as .conf", ext)
	}
	// A temporary file is named "."+app+".tmp-123" while it is written:
	// the proxy must not pick it up as a config.
	if strings.HasSuffix(".web.tmp-123", d.Ext()) {
		t.Errorf("Ext() %q would match the temporary files", d.Ext())
	}

	sites := map[string]proxy.Site{
		"plain HTTP": {App: "web", Domain: "web.example.com", Upstream: "127.0.0.1:40001", Release: "20261121-101500-123"},
		"dry run":    {App: "web", Domain: "web.example.com", Upstream: "127.0.0.1:<free port>", Release: "20261121-101500-123"},
		"certificate files": {App: "web", Domain: "web.example.com", Upstream: "127.0.0.1:40001", Release: "20261121-101500-123",
			CertFile: "/certs/web.example.com/fullchain.pem", KeyFile: "/certs/web.example.com/privkey.pem", ACMERoot: "/acme"},
		"certificate by the proxy": {App: "web", Domain: "web.example.com", Upstream: "127.0.0.1:40001", Release: "20261121-101500-123", AutoTLS: true},
		"no release yet":           {App: "web", Domain: "web.example.com", Upstream: "127.0.0.1:40001"},
	}
	for name, site := range sites {
		t.Run(name, func(t *testing.T) {
			out, err := d.Render(site)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			// What makes the switch observable, and what a rollback and an
			// undo read back from the file on disk.
			if got := d.ReleaseOf(out); got != site.Release {
				t.Errorf("ReleaseOf(Render()) = %q, want %q", got, site.Release)
			}
			for _, want := range []string{site.Domain, site.Upstream, proxy.ReleaseHeader} {
				if !bytes.Contains(out, []byte(want)) {
					t.Errorf("Render() does not contain %q:\n%s", want, out)
				}
			}
			// The file is compared with the one on disk to show a diff and
			// to tell whether anything changed.
			again, _ := d.Render(site)
			if !bytes.Equal(out, again) {
				t.Error("Render() gives a different file for the same site")
			}
		})
	}

	other := sites["plain HTTP"]
	other.Release = "20261121-101600-456"
	a, _ := d.Render(sites["plain HTTP"])
	b, _ := d.Render(other)
	if bytes.Equal(a, b) {
		t.Error("Render() gives the same file for two releases")
	}

	for _, foreign := range []string{"", "server { listen 80; }", "# nothing of liman's here\n"} {
		if got := d.ReleaseOf([]byte(foreign)); got != "" {
			t.Errorf("ReleaseOf(%q) = %q, want empty", foreign, got)
		}
	}
}
