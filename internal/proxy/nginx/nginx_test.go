package nginx

import (
	"testing"

	"github.com/onrcanogul/liman/internal/proxy"
	"github.com/onrcanogul/liman/internal/proxy/proxytest"
)

func TestContract(t *testing.T) {
	proxytest.Contract(t, Dialect{})
}

func TestRenderHTTPOnly(t *testing.T) {
	got, err := Dialect{}.Render(proxy.Site{App: "orders-api", Domain: "api.example.com", Upstream: "127.0.0.1:40001", Release: "r2"})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// The whole output is compared: in a config file every character
	// matters, and a golden text makes template changes visible in review.
	want := `# Managed by liman for app "orders-api".
# Do not edit: this file is overwritten on every deploy.

upstream liman-orders-api {
    server 127.0.0.1:40001;
}

server {
    listen 80;
    server_name api.example.com;
    add_header X-Liman-Release "r2" always;

    location / {
        proxy_pass http://liman-orders-api;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
`
	if string(got) != want {
		t.Errorf("Render() =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderWithTLSAndACME(t *testing.T) {
	got, err := Dialect{}.Render(proxy.Site{
		App:      "orders-api",
		Domain:   "api.example.com",
		Upstream: "127.0.0.1:40001",
		CertFile: "/certs/api.example.com/fullchain.pem",
		KeyFile:  "/certs/api.example.com/privkey.pem",
		ACMERoot: "/var/lib/liman/acme",
		Release:  "r2",
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	want := `# Managed by liman for app "orders-api".
# Do not edit: this file is overwritten on every deploy.

upstream liman-orders-api {
    server 127.0.0.1:40001;
}

server {
    listen 80;
    server_name api.example.com;
    add_header X-Liman-Release "r2" always;

    # Lets an ACME client (Let's Encrypt) verify ownership of the domain.
    location /.well-known/acme-challenge/ {
        root /var/lib/liman/acme;
    }

    location / {
        return 301 https://$host$request_uri;
    }
}

server {
    listen 443 ssl;
    server_name api.example.com;
    add_header X-Liman-Release "r2" always;

    ssl_certificate     /certs/api.example.com/fullchain.pem;
    ssl_certificate_key /certs/api.example.com/privkey.pem;

    location / {
        proxy_pass http://liman-orders-api;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
`
	if string(got) != want {
		t.Errorf("Render() =\n%s\nwant\n%s", got, want)
	}
}

func TestReleaseOf(t *testing.T) {
	conf, err := Dialect{}.Render(proxy.Site{App: "web", Domain: "web.example.com", Upstream: "127.0.0.1:1", Release: "20261119-183544"})
	if err != nil {
		t.Fatal(err)
	}

	if got := (Dialect{}).ReleaseOf(conf); got != "20261119-183544" {
		t.Errorf("ReleaseOf(rendered) = %q, want the release it was rendered with", got)
	}
	if got := (Dialect{}).ReleaseOf(nil); got != "" {
		t.Errorf("ReleaseOf(nil) = %q, want empty", got)
	}
	if got := (Dialect{}).ReleaseOf([]byte("server { listen 80; }")); got != "" {
		t.Errorf("ReleaseOf(foreign config) = %q, want empty", got)
	}
}
