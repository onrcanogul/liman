// Package panel holds the web panel: a handful of static files that are
// compiled into the daemon and served by it.
//
// The panel is a client of the HTTP API like the CLI is. The files here
// contain no data and no credentials; the page asks the API for everything,
// with a token the user types in.
package panel

import (
	"embed"
	"io/fs"
	"net/http"
)

// The directive below makes the compiler copy the files of the static
// directory into the binary. limand stays a single file to install: there
// is no directory of assets that could be missing, or out of date, on the
// server.
//
//go:embed static
var files embed.FS

// contentSecurityPolicy tells the browser what the panel's page may do. It
// is the second line of defence against injected markup; the first is that
// the page never builds HTML out of data.
//
//	default-src 'none'      nothing is allowed unless listed below
//	script-src 'self'       scripts only from this daemon; no inline script,
//	                        so injected markup cannot run code
//	style-src 'self'        the same for style sheets
//	img-src 'self'          and for images
//	connect-src 'self'      the page may only talk to this daemon: even code
//	                        that somehow ran could not send the token away
//	base-uri 'none'         no <base> tag may redirect relative URLs
//	form-action 'none'      no form may be submitted anywhere
//	frame-ancestors 'none'  no other page may show the panel in a frame and
//	                        trick the user into clicking on it
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves the panel's files. Mount it with http.StripPrefix.
func Handler() http.Handler {
	// The embedded tree starts with the directory "static"; Sub makes that
	// directory the root. It can only fail if the name is wrong, which the
	// tests would catch, so a panic is appropriate.
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServerFS(static)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		// For browsers that predate frame-ancestors.
		h.Set("X-Frame-Options", "DENY")
		// The address of the panel says where the daemon is; it is not sent
		// along when a link leads elsewhere.
		h.Set("Referrer-Policy", "no-referrer")

		fileServer.ServeHTTP(w, r)
	})
}
