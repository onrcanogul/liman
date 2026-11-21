package caddy

import (
	"testing"

	"github.com/onrcanogul/liman/internal/proxy"
	"github.com/onrcanogul/liman/internal/proxy/proxytest"
)

func TestContract(t *testing.T) {
	proxytest.Contract(t, Dialect{})
}

func TestRender(t *testing.T) {
	site := proxy.Site{App: "web", Domain: "web.localhost", Upstream: "127.0.0.1:40001", Release: "r2"}
	tests := []struct {
		name string
		edit func(*proxy.Site)
		want string
	}{
		{"plain HTTP", func(*proxy.Site) {}, `# Managed by liman for app "web".
# Do not edit: this file is overwritten on every deploy.

http://web.localhost {
	header X-Liman-Release "r2"
	reverse_proxy 127.0.0.1:40001
}
`},
		{"certificate by Caddy", func(s *proxy.Site) { s.Domain, s.AutoTLS = "api.example.com", true }, `# Managed by liman for app "web".
# Do not edit: this file is overwritten on every deploy.

api.example.com {
	header X-Liman-Release "r2"
	reverse_proxy 127.0.0.1:40001
}

http://api.example.com {
	header X-Liman-Release "r2"
	redir https://{host}{uri} permanent
}
`},
		{"certificate files", func(s *proxy.Site) {
			s.Domain, s.CertFile, s.KeyFile = "api.example.com", "/certs/fullchain.pem", "/certs/privkey.pem"
		}, `# Managed by liman for app "web".
# Do not edit: this file is overwritten on every deploy.

api.example.com {
	tls /certs/fullchain.pem /certs/privkey.pem
	header X-Liman-Release "r2"
	reverse_proxy 127.0.0.1:40001
}

http://api.example.com {
	header X-Liman-Release "r2"
	redir https://{host}{uri} permanent
}
`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := site
			tt.edit(&s)
			got, err := Dialect{}.Render(s)
			if err != nil || string(got) != tt.want {
				t.Errorf("Render() = %v\n%s\nwant\n%s", err, got, tt.want)
			}
		})
	}
}
