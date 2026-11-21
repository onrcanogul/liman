// Package traefik is the Traefik dialect of the proxy package: one file of
// dynamic configuration per app in a directory Traefik's file provider
// watches. Traefik needs no command; it notices the file by itself.
//
// Traefik's static configuration needs the directory:
//
//	providers:
//	  file:
//	    directory: /etc/traefik/liman.d
//	    watch: true
//
// The routers name no entry point, so they serve on all of them. HTTPS is
// Traefik's own business: an entry point with TLS and a certificate
// resolver (and, on the plain one, a redirect to it) covers every app.
//
// The file provider, rather than labels on the containers, keeps liman's
// order of things: Traefik only sends traffic to a release after liman has
// found it healthy, and needs no access to Docker.
package traefik

import (
	"bytes"
	"fmt"
	"regexp"
	"text/template"

	"github.com/onrcanogul/liman/internal/proxy"
)

// Dialect describes Traefik to a proxy.Manager.
type Dialect struct{}

// Name implements proxy.Dialect.
func (Dialect) Name() string { return "Traefik" }

// Ext implements proxy.Dialect: the file provider reads .yml files.
func (Dialect) Ext() string { return ".yml" }

// Commands implements proxy.Dialect: none, Traefik watches the directory.
// A broken file is not applied (Traefik keeps the last good configuration
// and logs the error), which the wait for the release notices.
func (Dialect) Commands() (test, reload []string) { return nil, nil }

// ManagesCertificates implements proxy.Dialect.
func (Dialect) ManagesCertificates() bool { return true }

// Names are prefixed with liman- so they cannot collide with the user's
// own routers. An empty release makes Traefik send no header at all, which
// is what "no release yet" looks like to the probe.
const siteTemplate = `# Managed by liman for app "{{.App}}".
# Do not edit: this file is overwritten on every deploy.
http:
  routers:
    liman-{{.App}}:
      rule: "Host(` + "`{{.Domain}}`" + `)"
      service: liman-{{.App}}
      middlewares:
        - liman-{{.App}}-release
  middlewares:
    liman-{{.App}}-release:
      headers:
        customResponseHeaders:
          X-Liman-Release: "{{.Release}}"
  services:
    liman-{{.App}}:
      loadBalancer:
        servers:
          - url: "http://{{.Upstream}}"
`

var tmpl = template.Must(template.New("site").Parse(siteTemplate))

// Render implements proxy.Dialect.
func (Dialect) Render(site proxy.Site) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, site); err != nil {
		return nil, fmt.Errorf("render traefik config for %s: %w", site.App, err)
	}
	return buf.Bytes(), nil
}

var releaseRe = regexp.MustCompile(`X-Liman-Release: "([^"]*)"`)

// ReleaseOf implements proxy.Dialect.
func (Dialect) ReleaseOf(content []byte) string {
	if m := releaseRe.FindSubmatch(content); m != nil {
		return string(m[1])
	}
	return ""
}
