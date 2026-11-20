// Package server implements the daemon side of the liman HTTP API. The wire
// types and the client live in package api, which the CLI can import
// without dragging in Docker, Nginx or the database.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/applock"
	"github.com/onrcanogul/liman/internal/backup"
	"github.com/onrcanogul/liman/internal/buildinfo"
	"github.com/onrcanogul/liman/internal/deploy"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/pipeline"
	"github.com/onrcanogul/liman/internal/secrets"
	"github.com/onrcanogul/liman/internal/store"
)

// maxDefinitionBytes bounds the size of an uploaded app.yaml. Without a
// limit a client could make the daemon read an endless body into memory.
const maxDefinitionBytes = 1 << 20 // 1 MiB

// Server implements the daemon's HTTP API.
type Server struct {
	// Connect returns the container engine. It is called per request so the
	// daemon keeps running, and reports a clear error, while Docker is down.
	Connect func(ctx context.Context) (engine.Engine, error)

	// Proxy manages the reverse proxy; nil when none is configured.
	Proxy deploy.Proxy

	Store *store.Store

	// Locks is the set of apps being changed. The daemon shares one with
	// the health monitor; when nil the server uses its own.
	Locks *applock.Locks

	// HealthOf, when set, returns the health monitor's verdict on an app.
	// The last result is false if the monitor knows nothing about it.
	HealthOf func(app string) (health, detail string, restarts int, ok bool)

	// Monitoring describes the health monitor for GET /v1/info. Nil means
	// the monitor is turned off.
	Monitoring *api.MonitorInfo

	// Started is when the daemon started, for GET /v1/info.
	Started time.Time

	// Panel, when set, serves the web panel under /ui/. Its files are
	// public: they contain no data, and the page they make up has to ask
	// the API for everything, with a token, like any other client.
	Panel http.Handler

	// Backups makes and checks backups of the database. Nil means this
	// daemon offers none over the API.
	Backups *backup.Service

	// Secrets holds the apps' secret values, encrypted. Nil means the
	// daemon has no secret storage; apps that refer to secrets cannot be
	// deployed then.
	Secrets *secrets.Vault

	// Log receives one line per request. Nil discards them.
	Log *slog.Logger

	// ConfirmTimeout is how long a step-by-step deploy waits for the client
	// to answer. Zero means ten minutes.
	ConfirmTimeout time.Duration

	// MaxInFlight caps the number of requests handled at the same time.
	// Zero means a default.
	MaxInFlight int

	// BodyReadTimeout is how long a client gets to send a request body.
	// Zero means a default.
	BodyReadTimeout time.Duration

	// Drain, when set, overrides how long the previous release is given to
	// finish its requests after the proxy has been switched. It is a
	// pointer so that "not set" and "zero: do not wait" can be told apart.
	Drain *time.Duration

	// Certificates, when set, lists the certificates in nginx.cert_dir,
	// for GET /v1/certs.
	Certificates func(ctx context.Context) (api.Certificates, error)
	// Keys, when set, lists the master keys and what depends on each, for
	// GET /v1/keys. It reads every backup, so it is not cheap.
	Keys func(ctx context.Context) (api.Keys, error)
	// KeyRotationUnfinished is reported by GET /v1/info.
	KeyRotationUnfinished bool

	// Certs obtains a certificate after a deploy for a domain that has
	// none. Nil means HTTPS only comes from certificates already on disk.
	Certs deploy.Certs

	// NewDeployer builds the deployer for a request. Nil means deploy.New;
	// tests replace it to control ports, timing and release names.
	NewDeployer func(e engine.Engine, out io.Writer) *deploy.Deployer

	// mu guards the map below. HTTP handlers run concurrently, one
	// goroutine per request, and a map must never be read and written at
	// the same time.
	mu sync.Mutex
	// confirms holds the questions that are waiting for an answer.
	confirms map[string]chan bool

	// ownLocks is used when Locks is nil.
	ownLocks applock.Locks

	// limiter locks out clients that keep failing to authenticate. It is
	// created on first use; initLimiter makes that safe when the first
	// requests arrive at the same time.
	limiter     *authLimiter
	initLimiter sync.Once

	// rejected summarises requests that were turned away wholesale. Its
	// zero value is ready to use.
	rejected rejections

	// reads keeps repeated successful reads out of the log. Its zero value
	// is ready to use.
	reads readLog

	// used remembers when each token's last use was written to the
	// database, so that it is written at most once a minute.
	used readLog
}

// authFailures returns the failed-authentication limiter.
func (s *Server) authFailures() *authLimiter {
	// Once.Do runs the function exactly once, no matter how many goroutines
	// call it concurrently; the others wait until it has finished.
	s.initLimiter.Do(func() { s.limiter = newAuthLimiter() })
	return s.limiter
}

// access says what an endpoint gives access to, for tokens limited to some
// apps. Every route has to say: the zero value is no answer, and guard
// keeps limited tokens away from such a route. A route added without
// thinking about it is closed to them rather than open.
type access int

const (
	accessUnset access = iota
	// accessAny: nothing that belongs to an app, or only the caller's own
	// token (health, info).
	accessAny
	// accessAppInPath: the {name} in the path is the app.
	accessAppInPath
	// accessAppInBody: the app is named in the request body; the handler
	// checks it with mayUse once it has read it.
	accessAppInBody
	// accessListing: a list across apps; the handler leaves out what the
	// token may not see.
	accessListing
	// accessOwner: belongs to the token that created it (a confirmation).
	accessOwner
	// accessDaemon: the daemon as a whole (tokens, backups, keys,
	// certificates). Closed to limited tokens.
	accessDaemon
)

type route struct {
	pattern string
	access  access
	handler http.HandlerFunc
}

// routes is the API: every endpoint, with what it gives access to.
func (s *Server) routes() []route {
	return []route{
		{"GET /v1/health", accessAny, s.handleHealth},
		{"GET /v1/info", accessAny, s.handleInfo},
		{"POST /v1/deploys", accessAppInBody, s.handleDeploy},
		{"POST /v1/confirmations/{id}", accessOwner, s.handleConfirm},
		{"GET /v1/apps", accessListing, s.handleApps},
		{"GET /v1/apps/{name}/logs", accessAppInPath, s.handleLogs},
		{"POST /v1/apps/{name}/rollback", accessAppInPath, s.handleRollback},
		{"DELETE /v1/apps/{name}", accessAppInPath, s.handleStop},
		{"GET /v1/apps/{name}/secrets", accessAppInPath, s.handleSecretList},
		{"PUT /v1/apps/{name}/secrets/{key}", accessAppInPath, s.handleSecretSet},
		{"DELETE /v1/apps/{name}/secrets/{key}", accessAppInPath, s.handleSecretDelete},
		{"GET /v1/apps/{name}/health", accessAppInPath, s.handleHealthHistory},
		{"GET /v1/secrets", accessListing, s.handleAllSecrets},
		{"GET /v1/history", accessListing, s.handleHistory},
		{"GET /v1/releases", accessListing, s.handleReleases},
		{"GET /v1/tokens", accessDaemon, s.handleTokens},
		{"DELETE /v1/tokens/{name}", accessDaemon, s.handleTokenRevoke},
		{"GET /v1/backups", accessDaemon, s.handleBackupList},
		{"POST /v1/backups", accessDaemon, s.handleBackupCreate},
		{"POST /v1/backups/{name}/verify", accessDaemon, s.handleBackupVerify},
		{"GET /v1/certs", accessDaemon, s.handleCertificates},
		{"GET /v1/keys", accessDaemon, s.handleKeys},
	}
}

// guard puts in front of a handler what its access needs. Tokens that may
// use every app pass every guard: for them nothing changes.
func (s *Server) guard(a access, h http.HandlerFunc) http.HandlerFunc {
	switch a {
	case accessAny, accessAppInBody, accessListing, accessOwner:
		return h
	case accessAppInPath:
		return func(w http.ResponseWriter, r *http.Request) {
			if s.mayUse(w, r, r.PathValue("name")) {
				h(w, r)
			}
		}
	default: // accessDaemon, and accessUnset
		return func(w http.ResponseWriter, r *http.Request) {
			if apps := metaOf(r.Context()).apps; len(apps) > 0 {
				writeError(w, http.StatusForbidden, fmt.Errorf("this token is limited to %s; this endpoint is about the whole daemon", strings.Join(apps, ", ")))
				return
			}
			h(w, r)
		}
	}
}

// mayUse reports whether the request's token may act on app, and answers
// 403 if not. The answer is the same whether app exists or not, and names
// only the token's own apps: a limited token cannot find out what else
// runs here by trying names.
func (s *Server) mayUse(w http.ResponseWriter, r *http.Request, app string) bool {
	apps := metaOf(r.Context()).apps
	if len(apps) == 0 || slices.Contains(apps, app) {
		return true
	}
	writeError(w, http.StatusForbidden, fmt.Errorf("this token is limited to %s", strings.Join(apps, ", ")))
	return false
}

// visibleApps is the set of apps a listing may show for this request:
// nil for every app, otherwise the token's apps.
func visibleApps(r *http.Request) []string {
	return metaOf(r.Context()).apps
}

// visible reports whether app may appear in a listing for this request.
func visible(r *http.Request, app string) bool {
	apps := visibleApps(r)
	return apps == nil || slices.Contains(apps, app)
}

// Handler returns the HTTP handler that serves the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The pattern names the method and the path; {name} is a wildcard that
	// handlers read with r.PathValue. Requests with another method get 405
	// and unknown paths 404 without any code here. Every route goes through
	// guard, with what it gives access to.
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.pattern, s.guard(rt.access, rt.handler))
	}

	// Middleware wraps the handler from the inside out; a request passes
	// through the list below from top to bottom. The order is deliberate:
	//
	//   recoverPanics     outermost, so it also covers the other middleware
	//   secureHeaders     so even error responses carry the headers
	//   limitConcurrency  before logging: requests shed under load are
	//                     counted and summarised, not logged one by one
	//   logRequests       before authentication, so rejected requests are
	//                     logged too: they are the interesting ones
	//   rejectBrowsers    cheap, and before authentication
	//   authenticate      last: everything behind it is trusted
	var h http.Handler = mux
	h = s.authenticate(h)
	h = s.rejectBrowsers(h)
	h = s.logRequests(h)
	h = s.limitConcurrency(h)
	h = s.secureHeaders(h)
	h = s.recoverPanics(h)

	if s.Panel == nil {
		return h
	}

	// The panel sits next to the API, not behind its middleware: its files
	// must load before anybody has typed a token. Everything that is not
	// the panel still goes to the API handler, so an unknown path is
	// answered exactly as it was before the panel existed.
	root := http.NewServeMux()
	root.Handle("/", h)
	root.Handle("/ui/", s.panelHandler())
	// {$} matches the path "/" and nothing longer.
	root.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusFound))
	return root
}

// panelHandler wraps the panel's files in the middleware that still applies
// to them. They are neither authenticated nor logged: anybody who can reach
// the daemon can request them, and a log line per request would let that
// anybody fill the disk.
func (s *Server) panelHandler() http.Handler {
	files := http.StripPrefix("/ui", s.Panel)
	// The panel's files are small and need no token. A client that asks
	// for one and then never reads the answer would hold its place among
	// the concurrent requests forever; with a deadline on the write, the
	// connection is cut instead. (The API cannot have a blanket deadline
	// like this: a deploy or a followed log legitimately takes long.)
	h := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
		files.ServeHTTP(w, r)
	}))
	h = s.limitConcurrency(h)
	h = s.secureHeaders(h)
	h = s.recoverPanics(h)
	return h
}

// --- handlers -------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.Health{Status: "ok", Version: buildinfo.Version})
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	body, err := s.readBody(w, r, maxDefinitionBytes)
	if err != nil {
		writeError(w, bodyErrorStatus(err), fmt.Errorf("read app definition: %w", err))
		return
	}

	// The client validates too, but the server cannot trust that: anything
	// that crosses the network is checked again.
	a, err := app.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// The app is in the body, so this is the first moment it can be
	// checked; nothing has been looked up or touched yet. A dry run too:
	// its plan shows the app's secrets by name.
	if !s.mayUse(w, r, a.Name) {
		return
	}

	query := r.URL.Query()
	dryRun := query.Get("dry_run") == "true"
	stepMode := query.Get("step") == "true"
	if dryRun && stepMode {
		writeError(w, http.StatusBadRequest, errors.New("dry_run and step cannot be combined"))
		return
	}

	// Add the app's secrets to its environment. This comes before anything
	// is locked, connected or started: a deploy that cannot have its
	// configuration must fail without having touched anything.
	secretFiles, status, err := s.addSecrets(ctx, a, dryRun)
	if err != nil {
		writeError(w, status, err)
		return
	}

	var eng engine.Engine
	// unlock releases the app; it does nothing for a dry run, which takes
	// no lock.
	unlock := func() {}
	if !dryRun {
		// One deploy per app at a time. Two concurrent deploys would each
		// treat the other's container as "previous" and remove it.
		release, ok := s.lock(a.Name)
		if !ok {
			writeError(w, http.StatusConflict, fmt.Errorf("another operation on %s is in progress", a.Name))
			return
		}
		// sync.OnceFunc returns a function that runs release the first
		// time it is called and does nothing afterwards. The lock is
		// released explicitly below, before the result is sent; the defer
		// covers every early return and a panic.
		unlock = sync.OnceFunc(release)
		defer unlock()

		if eng, err = s.Connect(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
	}

	// Everything up to here answered with a plain status code and a JSON
	// error. From here on the status is 200 and the outcome travels inside
	// the stream: once the first byte is sent the status cannot change.
	stream := newEventStream(w)

	deployer := s.newDeployer(eng, stream)

	opts := deploy.Options{DryRun: dryRun, SecretFiles: secretFiles}
	if !dryRun {
		opts.OnEvent = s.recordEvent(a.Name)
		if stepMode {
			opts.Confirm = s.confirmer(ctx, stream)
		}
	}

	started := time.Now()
	res, err := deployer.Deploy(ctx, a, opts)

	result := &api.DeployResult{
		OK:        err == nil,
		App:       a.Name,
		Release:   res.Release,
		Container: res.Container,
		HostPort:  res.HostPort,
		Domain:    a.Domain,
		Routed:    err == nil && !dryRun && s.Proxy != nil,
		DryRun:    dryRun,
	}
	if err != nil {
		result.Error = err.Error()
	}

	if !dryRun {
		s.recordRelease(a, res.Release, started, err)
	}

	// Release the app before telling the client that the deploy is over.
	// The other way round, a client that reacts to the result at once (a
	// script deploying twice in a row) could be back with its next request
	// while this handler still holds the lock, and be refused with a 409
	// for a deploy that had, as far as it could tell, finished. (Found by
	// the load test: about one sequential deploy in five thousand.)
	unlock()
	stream.send(api.DeployEvent{Type: api.EventResult, Result: result})
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	name, ok := appName(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	dryRun := query.Get("dry_run") == "true"
	stepMode := query.Get("step") == "true"
	if dryRun && stepMode {
		writeError(w, http.StatusBadRequest, errors.New("dry_run and step cannot be combined"))
		return
	}

	unlock := func() {}
	if !dryRun {
		release, ok := s.lock(name)
		if !ok {
			writeError(w, http.StatusConflict, fmt.Errorf("another operation on %s is in progress", name))
			return
		}
		unlock = sync.OnceFunc(release)
		defer unlock()
	}

	// Unlike a deploy, even a dry run needs the engine: what a rollback
	// does depends entirely on which containers exist.
	eng, containers, status, err := s.containersOf(ctx, name)
	if err != nil {
		writeError(w, status, err)
		return
	}
	// Checked here, before the stream starts, so that "there is nothing to
	// roll back to" is an ordinary error with a status code of its own
	// rather than a failed operation inside a 200 response.
	if len(containers) < 2 {
		writeError(w, http.StatusConflict, fmt.Errorf("%s: %w; deploy the version you want instead", name, deploy.ErrNoPrevious))
		return
	}

	stream := newEventStream(w)
	deployer := s.newDeployer(eng, stream)

	opts := deploy.Options{DryRun: dryRun}
	if !dryRun {
		opts.OnEvent = s.recordEvent(name)
		if stepMode {
			opts.Confirm = s.confirmer(ctx, stream)
		}
	}

	res, err := deployer.Rollback(ctx, name, opts)

	result := &api.DeployResult{
		OK:        err == nil,
		App:       name,
		Release:   res.Release,
		Container: res.Container,
		HostPort:  res.HostPort,
		Routed:    err == nil && !dryRun && s.Proxy != nil,
		DryRun:    dryRun,
	}
	if err != nil {
		result.Error = err.Error()
	}

	// As in handleDeploy: the app is released before the result is sent.
	unlock()
	stream.send(api.DeployEvent{Type: api.EventResult, Result: result})
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	body, err := s.readBody(w, r, 1024)
	if err != nil {
		writeError(w, bodyErrorStatus(err), fmt.Errorf("read request body: %w", err))
		return
	}

	var req api.ConfirmRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	// A request with fields this version does not know is more likely a
	// mistake than something to guess the meaning of.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	s.mu.Lock()
	answer, ok := s.confirms[r.PathValue("id")]
	s.mu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no such confirmation; it may have been answered or timed out"))
		return
	}

	// The channel is buffered with room for one answer, so the first send
	// never blocks. A second answer for the same question finds the buffer
	// full; the default branch drops it instead of hanging this request.
	select {
	case answer <- req.Approve:
	default:
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	eng, err := s.Connect(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}

	containers, err := eng.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// The list is ordered by app and then oldest first, so the last
	// container seen for an app is its newest: the one in service. Any
	// other is a previous release, stopped and kept for a rollback.
	containers = slices.DeleteFunc(containers, func(c engine.Container) bool { return !visible(r, c.App) })

	current := map[string]string{}
	for _, c := range containers {
		current[c.App] = c.Name
	}

	// make with length 0 (rather than a nil slice) so that an empty result
	// is encoded as [] and not as null.
	out := make([]api.Container, 0, len(containers))
	for _, c := range containers {
		entry := api.Container{
			App:       c.App,
			Container: c.Name,
			Image:     c.Image,
			State:     c.State,
			Status:    c.Status,
			HostPort:  c.HostPort,

			Release:     c.Release,
			Domain:      c.Domain,
			Healthcheck: c.HealthPath,
			Created:     c.Created,

			Role: "current",
		}
		if current[c.App] != c.Name {
			entry.Role = "previous"
			out = append(out, entry)
			// The monitor's verdict is about the release in service.
			continue
		}
		if s.HealthOf != nil {
			if health, detail, restarts, ok := s.HealthOf(c.App); ok {
				entry.Health, entry.HealthDetail, entry.Restarts = health, detail, restarts
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	name, ok := appName(w, r)
	if !ok {
		return
	}

	eng, containers, status, err := s.containersOf(ctx, name)
	if err != nil {
		writeError(w, status, err)
		return
	}
	// Ordered oldest first; the newest container is the interesting one.
	newest := containers[len(containers)-1]
	follow := r.URL.Query().Get("follow") == "true"

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	out := &flushWriter{w: w, rc: http.NewResponseController(w)}

	// With follow this blocks until the container stops or the client goes
	// away; the latter cancels ctx, which ends the engine's stream. Both
	// output streams of the container are merged into the one response.
	if err := eng.Logs(ctx, newest.Name, follow, out, out); err != nil && ctx.Err() == nil {
		// The status line is long gone, so the error can only be appended.
		_, _ = fmt.Fprintf(out, "\nliman: %v\n", err)
	}
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	name, ok := appName(w, r)
	if !ok {
		return
	}

	release, ok := s.lock(name)
	if !ok {
		writeError(w, http.StatusConflict, fmt.Errorf("another operation on %s is in progress", name))
		return
	}
	// As in handleDeploy: released before the success answer is sent.
	unlock := sync.OnceFunc(release)
	defer unlock()

	eng, containers, status, err := s.containersOf(ctx, name)
	if err != nil {
		writeError(w, status, err)
		return
	}

	result := api.StopResult{Removed: []string{}}

	// Take the app out of the proxy first. The other way round there would
	// be a moment in which Nginx forwards requests to a container that is
	// already gone.
	if s.Proxy != nil {
		removed, err := s.unroute(ctx, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if removed {
			result.ProxyConfig = s.Proxy.Path(name)
		}
	}

	for _, c := range containers {
		if err := eng.Remove(ctx, c.Name); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		result.Removed = append(result.Removed, c.Name)
	}

	// Secrets are kept on purpose: deploying the app again needs them, and
	// a stop must not throw away values that may not exist anywhere else.
	// The answer names them, so that whoever stopped the app knows they are
	// still there. The containers are gone by now; a failure to list the
	// secrets must not turn that success into an error.
	if kept, err := s.Store.ListSecrets(ctx, name); err != nil {
		s.logger().Warn("stopped app, but could not list its secrets", "app", name, "error", err.Error())
	} else {
		for _, k := range kept {
			result.KeptSecrets = append(result.KeptSecrets, k.Name)
		}
	}
	unlock()
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit, filter, ok := listQuery(w, r)
	if !ok {
		return
	}

	apps, ok := s.listApps(w, r, filter)
	if !ok {
		return
	}
	stored, err := s.Store.EventsOf(r.Context(), apps, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// The stored type and the wire type have the same fields today, but
	// they are separate on purpose: the database schema and the API can
	// then change independently. With identical fields a conversion is all
	// it takes, and the compiler rejects it the day they drift apart.
	events := make([]api.Event, 0, len(stored))
	for _, ev := range stored {
		events = append(events, api.Event(ev))
	}
	writeJSON(w, http.StatusOK, events)
}

// --- read-only views for the panel --------------------------------------------

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	meta := metaOf(r.Context())
	writeJSON(w, http.StatusOK, api.Info{
		Token:          meta.token,
		Scope:          meta.scope,
		TokenExpiresAt: optionalTime(meta.expires),
		TokenApps:      meta.apps,
		Version:        buildinfo.Version,
		Commit:         buildinfo.Commit,
		Built:          buildinfo.Date,
		StartedAt:      s.Started,
		Now:            time.Now(),
		Proxy:          s.Proxy != nil,
		Secrets:        s.Secrets != nil,
		Backups:        s.Backups != nil,
		Certificates:   s.Certificates != nil,
		ACME:           s.Certs != nil,

		KeyRotationUnfinished: s.KeyRotationUnfinished,
		Monitor:               s.Monitoring,
	})
}

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	limit, filter, ok := listQuery(w, r)
	if !ok {
		return
	}

	apps, ok := s.listApps(w, r, filter)
	if !ok {
		return
	}
	stored, err := s.Store.ReleasesOf(r.Context(), apps, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	releases := make([]api.Release, 0, len(stored))
	for _, rel := range stored {
		releases = append(releases, api.Release(rel))
	}
	writeJSON(w, http.StatusOK, releases)
}

// healthRanges are the stretches of time a health history can be asked
// for, each with the width of one bucket. Every range has 48 buckets.
//
// The set is fixed on purpose. A free "from", "to" and "width" would let a
// client ask for a week in one-second buckets: 600 000 rows of JSON from
// one cheap request.
var healthRanges = map[string]time.Duration{
	"6h":  450 * time.Second,
	"24h": 30 * time.Minute,
	"7d":  210 * time.Minute,
}

const healthBucketCount = 48

func (s *Server) handleHealthHistory(w http.ResponseWriter, r *http.Request) {
	name, ok := appName(w, r)
	if !ok {
		return
	}

	span := r.URL.Query().Get("range")
	if span == "" {
		span = "24h"
	}
	width, ok := healthRanges[span]
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("range must be one of 6h, 24h and 7d"))
		return
	}

	// The buckets are aligned to the clock, not to the moment of the
	// request: the last one ends at the next multiple of the width. Asked
	// again ten seconds later, the same checks fall into the same buckets
	// and the picture stays still instead of shimmering.
	until := time.Now().Truncate(width).Add(width)
	since := until.Add(-healthBucketCount * width)

	stored, err := s.Store.HealthBuckets(r.Context(), name, since, until, width)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	out := api.HealthHistory{
		App:           name,
		Since:         since.UTC(),
		Until:         until.UTC(),
		BucketSeconds: int(width / time.Second),
		Buckets:       make([]api.HealthBucket, 0, len(stored)),
	}
	for _, b := range stored {
		out.Buckets = append(out.Buckets, api.HealthBucket(b))
	}
	writeJSON(w, http.StatusOK, out)
}

// listQuery reads the "limit" and "app" parameters shared by the listings.
// If one is invalid it answers 400 and returns false.
func listQuery(w http.ResponseWriter, r *http.Request) (limit int, filter string, ok bool) {
	limit = 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, errors.New("limit must be a number between 1 and 1000"))
			return 0, "", false
		}
		limit = n
	}

	// The filter is only ever a bound SQL parameter, which is safe with any
	// value; rejecting impossible names is still clearer than answering
	// with an empty list.
	filter = r.URL.Query().Get("app")
	if filter != "" && !app.ValidName(filter) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid app name %q", filter))
		return 0, "", false
	}
	return limit, filter, true
}

// listApps turns a listing's ?app= filter into the set of apps to read,
// within what the token may see. Filtering in the query, not afterwards,
// keeps the limit meaningful: other apps' rows do not use it up.
func (s *Server) listApps(w http.ResponseWriter, r *http.Request, filter string) ([]string, bool) {
	if filter != "" {
		if !s.mayUse(w, r, filter) {
			return nil, false
		}
		return []string{filter}, true
	}
	return visibleApps(r), true
}

// --- backups -------------------------------------------------------------------
//
// Making and checking backups is offered over the API. Restoring is not:
// it replaces the database the daemon has open, so it needs the daemon
// stopped and is done with "limand backup restore" on the server.

func (s *Server) handleBackupList(w http.ResponseWriter, _ *http.Request) {
	if !s.haveBackups(w) {
		return
	}
	entries, err := s.Backups.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	out := api.BackupList{Dir: s.Backups.Dir, Keep: s.Backups.Keep, Backups: make([]api.Backup, 0, len(entries))}
	for _, e := range entries {
		out.Backups = append(out.Backups, api.Backup{Name: e.Name, Encrypted: e.Encrypted, SizeBytes: e.Size, CreatedAt: e.Created})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	if !s.haveBackups(w) {
		return
	}

	entry, rep, removed, err := s.Backups.Create(r.Context())
	switch {
	case errors.Is(err, backup.ErrBusy):
		writeError(w, http.StatusConflict, err)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	s.logger().Info("backup created", "name", entry.Name, "bytes", entry.Size, "removed_older", removed)
	writeJSON(w, http.StatusCreated, api.BackupResult{
		Backup:  api.Backup{Name: entry.Name, Encrypted: entry.Encrypted, SizeBytes: entry.Size, CreatedAt: entry.Created},
		Report:  backupReport(rep),
		Removed: removed,
	})
}

func (s *Server) handleBackupVerify(w http.ResponseWriter, r *http.Request) {
	if !s.haveBackups(w) {
		return
	}

	rep, err := s.Backups.Verify(r.Context(), r.PathValue("name"))
	switch {
	case errors.Is(err, backup.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
		return
	case err != nil:
		// The file exists but is damaged or not a database: that is the
		// answer to the question, not a failure of the daemon.
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, backupReport(rep))
}

func (s *Server) handleCertificates(w http.ResponseWriter, r *http.Request) {
	if s.Certificates == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this daemon has no certificate directory (nginx.cert_dir)"))
		return
	}
	out, err := s.Certificates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	if s.Keys == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this daemon has no secret storage"))
		return
	}
	out, err := s.Keys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// haveBackups answers 503 and returns false if the daemon offers no backups.
func (s *Server) haveBackups(w http.ResponseWriter) bool {
	if s.Backups == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this daemon does not offer backups"))
		return false
	}
	return true
}

func backupReport(rep backup.Report) api.BackupReport {
	return api.BackupReport{
		// A wrong signature, or secrets that could not be tested, do not
		// count as fine.
		OK:            rep.Usable(),
		Signature:     rep.Signature,
		SchemaVersion: rep.Summary.SchemaVersion,
		Rows:          rep.Summary.Rows,
		Secrets:       rep.Secrets,
		KeyChecked:    rep.KeyChecked,
		Undecryptable: rep.Undecryptable,
		Encrypted:     rep.Encrypted,
		SignedBy:      rep.SignedBy,
		MissingKeys:   rep.MissingKeys,
	}
}

// --- secrets -----------------------------------------------------------------

func (s *Server) handleSecretList(w http.ResponseWriter, r *http.Request) {
	name, ok := appName(w, r)
	if !ok {
		return
	}
	if !s.haveSecrets(w) {
		return
	}

	infos, err := s.Secrets.List(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Names and dates only. No endpoint returns a value: a secret can be
	// replaced or removed through the API, but not read back.
	out := make([]api.Secret, 0, len(infos))
	for _, info := range infos {
		out = append(out, api.Secret{Name: info.Name, UpdatedAt: info.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSecretSet(w http.ResponseWriter, r *http.Request) {
	name, ok := appName(w, r)
	if !ok {
		return
	}
	key, ok := secretName(w, r)
	if !ok || !s.haveSecrets(w) {
		return
	}

	// One byte more than the limit is read, so that a value that is too
	// large is recognised as such instead of being silently cut off.
	value, err := s.readBody(w, r, secrets.MaxValueBytes+1)
	if err != nil {
		writeError(w, bodyErrorStatus(err), fmt.Errorf("read secret value: %w", err))
		return
	}

	if err := s.Secrets.Set(r.Context(), name, key, value); err != nil {
		// Everything Set rejects is about the input (empty, too large, a
		// NUL byte); a storage failure is the exception.
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// The value is never logged; that it changed is worth a line.
	s.logger().Info("secret set", "app", name, "secret", key)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	name, ok := appName(w, r)
	if !ok {
		return
	}
	key, ok := secretName(w, r)
	if !ok || !s.haveSecrets(w) {
		return
	}

	err := s.Secrets.Delete(r.Context(), name, key)
	switch {
	case errors.Is(err, secrets.ErrNotSet):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
	default:
		s.logger().Info("secret deleted", "app", name, "secret", key)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleAllSecrets lists the secrets of every app. The per-app listing
// only helps somebody who knows the app's name; a secret stored before the
// first deploy, or kept after the app was stopped, shows up only here.
func (s *Server) handleAllSecrets(w http.ResponseWriter, r *http.Request) {
	if !s.haveSecrets(w) {
		return
	}
	infos, err := s.Secrets.ListAll(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]api.Secret, 0, len(infos))
	for _, info := range infos {
		if visible(r, info.App) {
			out = append(out, api.Secret{App: info.App, Name: info.Name, UpdatedAt: info.UpdatedAt})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTokens lists the tokens. Any token may: names, scopes and dates
// open nothing, and a read-only token is meant to see everything.
func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.Store.Tokens(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	self, now := metaOf(r.Context()).token, time.Now()
	out := make([]api.Token, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, api.Token{
			Name:       t.Name,
			Scope:      t.Scope,
			CreatedAt:  t.CreatedAt,
			ExpiresAt:  optionalTime(t.ExpiresAt),
			Expired:    t.Expired(now),
			LastUsedAt: optionalTime(t.LastUsedAt),
			Apps:       t.Apps,
			Current:    t.Name == self,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTokenRevoke revokes a token. Creating one is deliberately not
// offered over the API: a stolen admin token could otherwise make itself
// spares that outlive its own revocation. Closing a door is offered
// wherever the operator is; opening one takes the server.
func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Names are at most 64 characters; anything longer cannot exist, and
	// is not worth a query or a log line.
	if name == "" || len(name) > 64 {
		writeError(w, http.StatusNotFound, fmt.Errorf("token %q: %w", truncate(name, 64), store.ErrNotFound))
		return
	}
	err := s.Store.RevokeToken(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
	default:
		// Who closed which door is worth a line of its own, at a level
		// that is not filtered out.
		s.logger().Warn("token revoked", "token", name, "by", metaOf(r.Context()).token)
		w.WriteHeader(http.StatusNoContent)
	}
}

// optionalTime is nil for the zero time, for fields that are left out of
// the JSON when there is nothing to say.
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// haveSecrets answers 503 and returns false if the daemon has no vault.
func (s *Server) haveSecrets(w http.ResponseWriter) bool {
	if s.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this daemon has no secret storage"))
		return false
	}
	return true
}

// secretName returns the secret named in the request path, or answers 400.
func secretName(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.PathValue("key")
	if !secrets.ValidName(key) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid secret name %q", key))
		return "", false
	}
	return key, true
}

// addSecrets resolves the secrets an app refers to. Those under secrets go
// into its environment; those under secret_files are returned, to be
// written into the container, and their <NAME>_FILE goes into the
// environment. On failure it returns the HTTP status that fits.
//
// In a dry run the values are not decrypted, only checked for existence:
// the plan shows the names (masked like every other variable), and a dry
// run that reads secrets would be a way to probe them.
func (s *Server) addSecrets(ctx context.Context, a *app.App, dryRun bool) (map[string][]byte, int, error) {
	names := append(append([]string{}, a.Secrets...), a.SecretFiles...)
	if len(names) == 0 {
		return nil, http.StatusOK, nil
	}
	if s.Secrets == nil {
		return nil, http.StatusServiceUnavailable, errors.New("the app refers to secrets, but this daemon has no secret storage")
	}

	values := map[string]string{}
	if dryRun {
		missing, err := s.Secrets.Missing(ctx, a.Name, names)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		if len(missing) > 0 {
			return nil, http.StatusBadRequest, &secrets.MissingError{App: a.Name, Names: missing}
		}
		for _, name := range names {
			values[name] = ""
		}
	} else {
		resolved, err := s.Secrets.Resolve(ctx, a.Name, names)
		if errors.Is(err, secrets.ErrNotSet) {
			return nil, http.StatusBadRequest, err
		}
		if err != nil {
			// A value that is stored but cannot be decrypted: the key was
			// replaced or the data was changed. That is the daemon's
			// problem, not the request's.
			return nil, http.StatusInternalServerError, err
		}
		values = resolved
	}

	// Build a new map instead of writing into a.Env: the definition may
	// have had no env section at all, which leaves the map nil, and a nil
	// map cannot be written to.
	env := make(map[string]string, len(a.Env)+len(names))
	for k, v := range a.Env {
		env[k] = v
	}
	for _, name := range a.Secrets {
		env[name] = values[name]
	}
	var files map[string][]byte
	if len(a.SecretFiles) > 0 {
		files = make(map[string][]byte, len(a.SecretFiles))
	}
	for _, name := range a.SecretFiles {
		// The environment only says where the file is; the value itself
		// never enters it.
		env[app.SecretFileEnv(name)] = app.SecretFilePath(name)
		files[name] = []byte(values[name])
	}
	a.Env = env
	return files, http.StatusOK, nil
}

// --- helpers used by handlers ---------------------------------------------

// appName returns the app named in the request path. If the name is not a
// valid app name it answers 400 and returns false.
//
// The name comes straight from the URL and goes on to be compared with
// container labels and turned into a file name for the proxy config. A
// value such as "../../nginx" must never get that far.
func appName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !app.ValidName(name) {
		// %q quotes the value, so control characters in it cannot reach a
		// terminal or a log line unescaped.
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid app name %q", name))
		return "", false
	}
	return name, true
}

func (s *Server) newDeployer(e engine.Engine, out io.Writer) *deploy.Deployer {
	var d *deploy.Deployer
	if s.NewDeployer != nil {
		d = s.NewDeployer(e, out)
	} else {
		d = deploy.New(e, out)
	}
	d.Proxy = s.Proxy
	d.Certs = s.Certs
	if s.Drain != nil {
		d.Drain = *s.Drain
	}
	return d
}

// lock marks app as busy. It returns false if it already is; otherwise it
// returns the function that releases the app again.
func (s *Server) lock(app string) (unlock func(), ok bool) {
	if s.Locks != nil {
		return s.Locks.TryLock(app)
	}
	return s.ownLocks.TryLock(app)
}

// confirmer returns the pipeline callback that asks the client before each
// step. The question goes out on the deploy's own response stream; the
// answer comes back on a separate request, handled by handleConfirm.
func (s *Server) confirmer(ctx context.Context, stream *eventStream) func(pipeline.Step) (bool, error) {
	timeout := s.ConfirmTimeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}

	return func(step pipeline.Step) (bool, error) {
		id, err := randomID()
		if err != nil {
			return false, err
		}
		answer := make(chan bool, 1)

		s.mu.Lock()
		if s.confirms == nil {
			s.confirms = map[string]chan bool{}
		}
		s.confirms[id] = answer
		s.mu.Unlock()

		// Whatever happens below, the question must not stay registered.
		defer func() {
			s.mu.Lock()
			delete(s.confirms, id)
			s.mu.Unlock()
		}()

		stream.send(api.DeployEvent{Type: api.EventConfirm, ConfirmID: id, Step: step.Name})

		// Wait for whichever comes first: the answer, the client giving up
		// (its connection closing cancels ctx), or the timeout. Without the
		// last two a forgotten terminal would hold the app's lock forever.
		select {
		case approved := <-answer:
			return approved, nil
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(timeout):
			return false, fmt.Errorf("no answer within %s", timeout)
		}
	}
}

// recordEvent returns the pipeline callback that stores executed steps.
func (s *Server) recordEvent(appName string) func(release string, ev pipeline.Event) {
	return func(release string, ev pipeline.Event) {
		rec := store.Event{
			Time:       ev.Started,
			App:        appName,
			Release:    release,
			Step:       ev.Step,
			Action:     string(ev.Action),
			OK:         ev.Err == nil,
			DurationMS: ev.Duration.Milliseconds(),
		}
		if ev.Err != nil {
			rec.Error = ev.Err.Error()
		}

		// The request context may already be cancelled (the client pressed
		// Ctrl+C and the pipeline is rolling back). Those undo events are
		// exactly the ones worth keeping, so the write gets its own context.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// A failed write must not fail an otherwise fine deploy, but it
		// must not go unnoticed either.
		if err := s.Store.AppendEvent(ctx, rec); err != nil {
			s.logger().Error("history not recorded", "app", appName, "step", ev.Step, "error", err)
		}
	}
}

func (s *Server) recordRelease(a *app.App, release string, started time.Time, deployErr error) {
	rel := store.Release{
		App:        a.Name,
		Release:    release,
		Image:      a.Image,
		Status:     store.StatusSucceeded,
		StartedAt:  started,
		FinishedAt: time.Now(),
	}
	if deployErr != nil {
		rel.Status = store.StatusFailed
		rel.Error = deployErr.Error()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.Store.RecordRelease(ctx, rel); err != nil {
		s.logger().Error("release not recorded", "app", a.Name, "release", release, "error", err)
	}
}

// containersOf connects to the engine and returns the containers of an app.
// On failure it also returns the HTTP status that describes the problem.
func (s *Server) containersOf(ctx context.Context, appName string) (engine.Engine, []engine.Container, int, error) {
	eng, err := s.Connect(ctx)
	if err != nil {
		return nil, nil, http.StatusServiceUnavailable, err
	}

	all, err := eng.List(ctx)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, err
	}

	containers := engine.ForApp(all, appName)
	if len(containers) == 0 {
		return nil, nil, http.StatusNotFound, fmt.Errorf("%s: %w", appName, engine.ErrNotFound)
	}
	return eng, containers, http.StatusOK, nil
}

// unroute removes the app's proxy config and reloads the proxy. If the
// reload cannot be done the config is put back, so the file on disk keeps
// matching what the proxy is actually serving.
func (s *Server) unroute(ctx context.Context, appName string) (removed bool, err error) {
	old, err := s.Proxy.Current(appName)
	if err != nil {
		return false, err
	}
	if old == nil {
		return false, nil // never routed, nothing to do
	}

	if err := s.Proxy.Restore(appName, nil); err != nil {
		return false, err
	}

	err = s.Proxy.Test(ctx)
	if err == nil {
		err = s.Proxy.Reload(ctx)
	}
	if err != nil {
		// errors.Join keeps both: why the reload failed and, if it also
		// happened, why the file could not be put back.
		return false, errors.Join(err, s.Proxy.Restore(appName, old))
	}
	return true, nil
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

func randomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// writeJSON sends v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	// Headers must be set before WriteHeader; after it they are ignored.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, api.ErrorResponse{Error: err.Error()})
}
