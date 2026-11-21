package daemon

import (
	"fmt"
	"slices"
	"strings"

	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/proxy"
	"github.com/onrcanogul/liman/internal/proxy/caddy"
	"github.com/onrcanogul/liman/internal/proxy/nginx"
	"github.com/onrcanogul/liman/internal/proxy/traefik"
)

// dialects are the proxies liman can drive, by the name proxy.driver gives.
// This is the only place that knows them: everything else works with a
// *proxy.Manager, whatever proxy is behind it.
var dialects = map[string]proxy.Dialect{
	"nginx":   nginx.Dialect{},
	"caddy":   caddy.Dialect{},
	"traefik": traefik.Dialect{},
}

// newProxy returns the manager for the proxy cfg describes, or nil when
// there is none. It refuses settings that cannot work with that proxy.
func newProxy(cfg config.Config) (*proxy.Manager, error) {
	if !cfg.Proxy.Enabled() {
		return nil, nil
	}
	dialect, ok := dialects[cfg.Proxy.Driver]
	if !ok {
		return nil, fmt.Errorf("%s.driver: unknown proxy %q; liman drives %s", cfg.Proxy.Section(), cfg.Proxy.Driver, knownDrivers())
	}
	if dialect.ManagesCertificates() && cfg.ACME.Enabled() {
		return nil, fmt.Errorf("acme: %s obtains certificates itself; remove the acme section", dialect.Name())
	}
	return proxy.New(cfg.Proxy, dialect), nil
}

// knownDrivers lists the driver names, sorted, for messages.
func knownDrivers() string {
	names := make([]string, 0, len(dialects))
	for name := range dialects {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// proxyName names the proxy for the log: "Nginx", or "none".
func proxyName(m *proxy.Manager) string {
	if m == nil {
		return "none"
	}
	return m.Name()
}

// engineName names the container engine for the log.
func engineName(e config.Engine) string {
	if e.Driver == "" {
		return "docker"
	}
	return e.Driver
}
