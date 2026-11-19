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
	"strconv"
	"sync"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/applock"
	"github.com/onrcanogul/liman/internal/buildinfo"
	"github.com/onrcanogul/liman/internal/deploy"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/pipeline"
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
}

// authFailures returns the failed-authentication limiter.
func (s *Server) authFailures() *authLimiter {
	// Once.Do runs the function exactly once, no matter how many goroutines
	// call it concurrently; the others wait until it has finished.
	s.initLimiter.Do(func() { s.limiter = newAuthLimiter() })
	return s.limiter
}

// Handler returns the HTTP handler that serves the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The pattern names the method and the path; {name} is a wildcard that
	// handlers read with r.PathValue. Requests with another method get 405
	// and unknown paths 404 without any code here.
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/deploys", s.handleDeploy)
	mux.HandleFunc("POST /v1/confirmations/{id}", s.handleConfirm)
	mux.HandleFunc("GET /v1/apps", s.handleApps)
	mux.HandleFunc("GET /v1/apps/{name}/logs", s.handleLogs)
	mux.HandleFunc("POST /v1/apps/{name}/rollback", s.handleRollback)
	mux.HandleFunc("DELETE /v1/apps/{name}", s.handleStop)
	mux.HandleFunc("GET /v1/history", s.handleHistory)

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

	query := r.URL.Query()
	dryRun := query.Get("dry_run") == "true"
	stepMode := query.Get("step") == "true"
	if dryRun && stepMode {
		writeError(w, http.StatusBadRequest, errors.New("dry_run and step cannot be combined"))
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

	opts := deploy.Options{DryRun: dryRun}
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
			Role:      "current",
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
	unlock()
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, errors.New("limit must be a number between 1 and 1000"))
			return
		}
		limit = n
	}

	// The filter is only ever a bound SQL parameter, which is safe with any
	// value; rejecting impossible names is still clearer than answering
	// with an empty list.
	filter := r.URL.Query().Get("app")
	if filter != "" && !app.ValidName(filter) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid app name %q", filter))
		return
	}

	stored, err := s.Store.Events(r.Context(), filter, limit)
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
