// Package caddy is the Caddy dialect of the proxy package: one site block
// per app in a directory the Caddyfile imports, checked with caddy validate
// and applied with caddy reload.
//
// The Caddyfile needs one line, next to the sites of its own:
//
//	import /etc/caddy/liman.d/*.caddy
//
// Caddy obtains certificates itself. A domain a public CA can issue for is
// served over HTTPS, with plain HTTP redirecting to it; other names (such
// as *.localhost) are served over plain HTTP.
package caddy

import (
	"bytes"
	"fmt"
	"regexp"
	"text/template"

	"github.com/onrcanogul/liman/internal/proxy"
)

// Dialect describes Caddy to a proxy.Manager.
type Dialect struct{}

// Name implements proxy.Dialect.
func (Dialect) Name() string { return "Caddy" }

// Ext implements proxy.Dialect.
func (Dialect) Ext() string { return ".caddy" }

// Commands implements proxy.Dialect. Both read the Caddyfile, which pulls
// in liman's files through its import line.
func (Dialect) Commands() (test, reload []string) {
	return []string{"caddy", "validate", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"},
		[]string{"caddy", "reload", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"}
}

// ManagesCertificates implements proxy.Dialect.
func (Dialect) ManagesCertificates() bool { return true }

// Every block that answers sets the release header, the redirect too: the
// probe asks over plain HTTP and must see the release there.
//
// An explicit http:// block keeps Caddy from adding a redirect of its own,
// which would answer without the header.
const siteTemplate = `# Managed by liman for app "{{.App}}".
# Do not edit: this file is overwritten on every deploy.
{{- if or .AutoTLS .TLS}}

{{.Domain}} {
{{- if .TLS}}
	tls {{.CertFile}} {{.KeyFile}}
{{- end}}
	header X-Liman-Release "{{.Release}}"
	reverse_proxy {{.Upstream}}
}

http://{{.Domain}} {
	header X-Liman-Release "{{.Release}}"
	redir https://{host}{uri} permanent
}
{{- else}}

http://{{.Domain}} {
	header X-Liman-Release "{{.Release}}"
	reverse_proxy {{.Upstream}}
}
{{- end}}
`

var tmpl = template.Must(template.New("site").Parse(siteTemplate))

// Render implements proxy.Dialect.
func (Dialect) Render(site proxy.Site) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, site); err != nil {
		return nil, fmt.Errorf("render caddy config for %s: %w", site.App, err)
	}
	return buf.Bytes(), nil
}

var releaseRe = regexp.MustCompile(`header X-Liman-Release "([^"]*)"`)

// ReleaseOf implements proxy.Dialect.
func (Dialect) ReleaseOf(content []byte) string {
	if m := releaseRe.FindSubmatch(content); m != nil {
		return string(m[1])
	}
	return ""
}
