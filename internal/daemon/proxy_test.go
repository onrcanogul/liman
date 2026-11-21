package daemon

import (
	"strings"
	"testing"

	"github.com/onrcanogul/liman/internal/config"
)

func TestNewProxyPicksTheDriversDialect(t *testing.T) {
	for driver, name := range map[string]string{"nginx": "Nginx", "caddy": "Caddy", "traefik": "Traefik"} {
		m, err := newProxy(config.Config{Proxy: config.Proxy{Driver: driver, ConfDir: "/etc/liman.d"}})
		if err != nil || m == nil || m.Name() != name {
			t.Errorf("newProxy(%s) = %v, %v; want %s", driver, m, err, name)
		}
	}
}

func TestNewProxyWithoutAProxy(t *testing.T) {
	if m, err := newProxy(config.Config{}); m != nil || err != nil {
		t.Errorf("newProxy() = %v, %v; want no proxy", m, err)
	}
}

func TestNewProxyRefusesWhatCannotWork(t *testing.T) {
	acme := config.ACME{Directory: "https://ca.example/dir"}
	tests := []struct {
		name    string
		cfg     config.Config
		wantErr string
	}{
		{"unknown driver", config.Config{Proxy: config.Proxy{Driver: "apache", ConfDir: "/x"}}, `unknown proxy "apache"; liman drives caddy, nginx, traefik`},
		// Two parties obtaining certificates for one domain would fight
		// over the CA's challenges.
		{"caddy with acme", config.Config{Proxy: config.Proxy{Driver: "caddy", ConfDir: "/x"}, ACME: acme}, "Caddy obtains certificates itself"},
		{"traefik with acme", config.Config{Proxy: config.Proxy{Driver: "traefik", ConfDir: "/x"}, ACME: acme}, "Traefik obtains certificates itself"},
	}
	for _, tt := range tests {
		if _, err := newProxy(tt.cfg); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: error = %v, want %q", tt.name, err, tt.wantErr)
		}
	}
	if _, err := newProxy(config.Config{Proxy: config.Proxy{Driver: "nginx", ConfDir: "/x"}, ACME: acme}); err != nil {
		t.Errorf("nginx with acme: error = %v", err)
	}
}
