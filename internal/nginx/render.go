// Package nginx generates and applies the reverse proxy configuration.
//
// liman owns one file per app in a directory of its own. It never reads or
// edits any other Nginx file, so hand-written configuration stays untouched.
package nginx

import (
	"bytes"
	"fmt"
	"text/template"
)

// Site is everything the template needs to describe one app.
type Site struct {
	App    string
	Domain string

	// Upstream is the host:port Nginx forwards requests to.
	Upstream string

	// CertFile and KeyFile enable HTTPS when both are set.
	CertFile string
	KeyFile  string

	// ACMERoot, when set, is served under /.well-known/acme-challenge/.
	ACMERoot string

	// Release identifies the deploy this config belongs to. Nginx reports
	// it in a response header, which lets liman observe the moment a reload
	// has actually taken effect.
	Release string
}

// ReleaseHeader is the response header that carries Site.Release.
const ReleaseHeader = "X-Liman-Release"

// TLS reports whether the site is served over HTTPS.
func (s Site) TLS() bool {
	return s.CertFile != "" && s.KeyFile != ""
}

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

// Render returns the Nginx configuration for site.
func Render(site Site) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, site); err != nil {
		return nil, fmt.Errorf("render nginx config for %s: %w", site.App, err)
	}
	return buf.Bytes(), nil
}
