package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/engine"
)

// maxTriggerBytes bounds the body of a trigger, which is two short strings.
const maxTriggerBytes = 4 << 10

// triggersPerMinute is how many deploys one deploy key may ask for in a
// minute. A pipeline pushes a few times an hour at most; a leaked key in a
// loop must not keep the server busy deploying.
const triggersPerMinute = 6

// HookHandler returns the handler for the trigger endpoint: POST
// /hooks/deploy, and nothing else. It is meant for a listener of its own
// that a reverse proxy exposes to the internet, so that the management API
// stays local whatever that proxy is told.
func (s *Server) HookHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/deploy", s.handleTrigger)

	// The same middleware as the API, in the same order (see Handler),
	// except that only deploy keys get through.
	var h http.Handler = mux
	h = s.authenticate(h, triggerEndpoint)
	h = s.rejectBrowsers(h)
	h = s.logRequests(h)
	h = s.limitConcurrency(h)
	h = s.secureHeaders(h)
	h = s.recoverPanics(h)
	return h
}

// handleTrigger deploys an app again with another image of its own
// repository: the one with the digest in the body. Everything else comes
// from the app's last deploy made by hand.
func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	body, err := s.readBody(w, r, maxTriggerBytes)
	if err != nil {
		writeError(w, bodyErrorStatus(err), fmt.Errorf("read trigger: %w", err))
		return
	}
	var req api.TriggerRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	// A misspelled field ("digets") is an error, not an empty digest.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read trigger: %w", err))
		return
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, errors.New("read trigger: one JSON object expected"))
		return
	}
	if !app.ValidName(req.App) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid app name %q", req.App))
		return
	}
	if !engine.ValidDigest(req.Digest) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid digest %q: want sha256: and 64 hexadecimal digits", req.Digest))
		return
	}
	if !s.mayUse(w, r, req.App) {
		return
	}

	// Counted from here, whatever the outcome: asking the registry about
	// digests that do not exist costs too.
	m := metaOf(ctx)
	if wait, ok := s.triggers.allow(m.token+"\x00"+m.created.Format(time.RFC3339Nano), time.Now()); !ok {
		w.Header().Set("Retry-After", retryAfter(wait))
		writeError(w, http.StatusTooManyRequests, fmt.Errorf("this key may trigger %d deploys a minute; try again in %s", triggersPerMinute, wait.Round(time.Second)))
		return
	}

	definition, found, err := s.Store.Definition(ctx, req.App)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("%s has no deploy to repeat: deploy it once with liman deploy (a stopped app needs that too)", req.App))
		return
	}
	a, err := app.Parse(definition)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("the kept definition of %s: %w; deploy it again with liman deploy", req.App, err))
		return
	}
	// The repository is the one in the definition, never one from the
	// request: a deploy key can choose an image of its app, not any image.
	image, err := engine.WithDigest(a.Image, req.Digest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	eng, err := s.Connect(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	exists, err := eng.ImageExists(ctx, image)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if !exists {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("%s is not in the registry: push the image first, to the repository in the app's definition", image))
		return
	}

	a.Image = image
	s.runDeploy(w, r, a, deployRun{})
}

// triggerLimiter allows each key triggersPerMinute triggers in any minute.
type triggerLimiter struct {
	mu sync.Mutex
	// recent holds, per key, the times of its triggers in the last minute,
	// oldest first. Only valid deploy keys get here, so it holds no more
	// entries than there are such keys.
	recent map[string][]time.Time
}

// allow records a trigger by key at now, if the key has one left; if not,
// it says how long until it has.
func (l *triggerLimiter) allow(key string, now time.Time) (wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.recent == nil {
		l.recent = make(map[string][]time.Time)
	}
	windowStart := now.Add(-time.Minute)
	times := l.recent[key]
	for len(times) > 0 && !times[0].After(windowStart) {
		times = times[1:]
	}
	if len(times) >= triggersPerMinute {
		l.recent[key] = times
		return times[0].Sub(windowStart), false
	}
	l.recent[key] = append(times, now)
	return 0, true
}
