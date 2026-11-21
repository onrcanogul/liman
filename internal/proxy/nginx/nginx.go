// Package nginx is the Nginx dialect of the proxy package: one server
// block per app in the directory nginx.conf includes, tested with nginx -t
// and applied with a reload.
package nginx

import (
	"bytes"
	"fmt"
	"regexp"
	"text/template"

	"github.com/onrcanogul/liman/internal/proxy"
)

// Dialect describes Nginx to a proxy.Manager.
type Dialect struct{}

// Name implements proxy.Dialect.
func (Dialect) Name() string { return "Nginx" }

// Ext implements proxy.Dialect: nginx.conf includes "liman.d/*.conf".
func (Dialect) Ext() string { return ".conf" }

// Commands implements proxy.Dialect. On a server the installer replaces
// them with the same commands through sudo.
func (Dialect) Commands() (test, reload []string) {
	return []string{"nginx", "-t"}, []string{"nginx", "-s", "reload"}
}

// ManagesCertificates implements proxy.Dialect: Nginx serves the
// certificates liman obtains, it does not obtain any.
func (Dialect) ManagesCertificates() bool { return false }

// The template uses text/template syntax: {{.Field}} inserts a value,
// {{if}}...{{end}} is a conditional and {{template "name" .}} includes a
// named block. A trailing "-" inside the braces ("-}}") trims the whitespace
// that follows, which keeps the output free of blank lines.
//
// text/template is the right package here, not html/template: the output is
// not HTML, and HTML escaping would corrupt it.
const siteTemplate = `# Managed by liman for app "{{.App}}".
# Do not edit: this file is overwritten on every deploy.

upstream liman-{{.App}} {
    server {{.Upstream}};
}

server {
    listen 80;
    server_name {{.Domain}};
    add_header X-Liman-Release "{{.Release}}" always;
{{- if .ACMERoot}}

    # Lets an ACME client (Let's Encrypt) verify ownership of the domain.
    location /.well-known/acme-challenge/ {
        root {{.ACMERoot}};
    }
{{- end}}

    location / {
{{- if .TLS}}
        return 301 https://$host$request_uri;
{{- else}}
{{template "proxy" .}}
{{- end}}
    }
}
{{- if .TLS}}

server {
    listen 443 ssl;
    server_name {{.Domain}};
    add_header X-Liman-Release "{{.Release}}" always;

    ssl_certificate     {{.CertFile}};
    ssl_certificate_key {{.KeyFile}};

    location / {
{{template "proxy" .}}
    }
}
{{- end}}
`

// The proxy block is shared by the HTTP and HTTPS servers. The headers tell
// the app what the original request looked like, since from the app's point
// of view every request now comes from Nginx.
const proxyTemplate = `{{define "proxy"}}        proxy_pass http://liman-{{.App}};
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;{{end}}`

// tmpl is parsed once, when the package is initialised. template.Must
// panics if the template is invalid, which turns a typo in the constant
// above into a failure at startup (and in every test) instead of at the
// first deploy.
var tmpl = template.Must(template.Must(template.New("site").Parse(siteTemplate)).Parse(proxyTemplate))

// Render implements proxy.Dialect.
func (Dialect) Render(site proxy.Site) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, site); err != nil {
		return nil, fmt.Errorf("render nginx config for %s: %w", site.App, err)
	}
	return buf.Bytes(), nil
}

// The quoted value of the add_header directive written by the template.
var releaseRe = regexp.MustCompile(`add_header X-Liman-Release "([^"]*)"`)

// ReleaseOf implements proxy.Dialect.
func (Dialect) ReleaseOf(content []byte) string {
	// FindSubmatch returns the whole match followed by the parenthesised
	// groups, or nil when nothing matches.
	if m := releaseRe.FindSubmatch(content); m != nil {
		return string(m[1])
	}
	return ""
}
