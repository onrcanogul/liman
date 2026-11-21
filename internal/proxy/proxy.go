// Package proxy routes apps through a reverse proxy that reads its
// configuration from files: Nginx, Caddy, Traefik.
//
// liman owns one file per app in a directory of its own and never reads or
// edits any other file of the proxy, so hand-written configuration stays
// untouched. Every such proxy is driven the same way: write the file, test
// the configuration, reload, and ask the proxy which release it serves.
// That is the Manager. What differs from one proxy to the next (the file's
// text, the commands) is a Dialect, one per subpackage.
//
// The rest of liman knows neither: deploys use the deploy.Proxy interface,
// which Manager satisfies, and only the daemon's wiring picks a dialect.
package proxy

// Site is everything a dialect needs to describe one app.
type Site struct {
	App    string
	Domain string

	// Upstream is the host:port the proxy forwards requests to.
	Upstream string

	// CertFile and KeyFile enable HTTPS when both are set: certificates
	// liman obtained, or that were put there by hand.
	CertFile string
	KeyFile  string

	// ACMERoot, when set, is served under /.well-known/acme-challenge/.
	ACMERoot string

	// AutoTLS asks a proxy that obtains certificates itself to serve the
	// domain over HTTPS. It is only set for domains a public CA can issue
	// for.
	AutoTLS bool

	// Release identifies the deploy this config belongs to. The proxy
	// reports it in the ReleaseHeader of every response, which lets liman
	// observe the moment a reload has actually taken effect.
	Release string
}

// TLS reports whether the site is served over HTTPS with certificate files.
func (s Site) TLS() bool {
	return s.CertFile != "" && s.KeyFile != ""
}

// ReleaseHeader is the response header that carries Site.Release.
const ReleaseHeader = "X-Liman-Release"

// Dialect is what one proxy needs to be driven by a Manager.
type Dialect interface {
	// Name is the proxy's name as people write it: "Nginx".
	Name() string
	// Ext is the extension of the files the proxy reads: ".conf".
	Ext() string
	// Render returns the file for site. The output must carry the release
	// in a form ReleaseOf finds again.
	Render(site Site) ([]byte, error)
	// ReleaseOf returns the release recorded in a file Render wrote, or ""
	// when content is empty or carries none.
	ReleaseOf(content []byte) string
	// Commands are the test and reload commands used when config.yaml
	// gives none. Nil means the proxy needs none: it watches its files.
	Commands() (test, reload []string)
	// ManagesCertificates reports whether the proxy obtains certificates
	// itself, in which case liman does not.
	ManagesCertificates() bool
}
