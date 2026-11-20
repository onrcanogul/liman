package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/store"
)

// ChecksumLabel is what the checksum key is derived from the master key
// with.
const ChecksumLabel = "liman definition checksum v1"

// maxTriggerBytes bounds the body of a trigger: a definition, as large as
// one sent to POST /v1/deploys may be, and the values of its variables.
const maxTriggerBytes = maxDefinitionBytes + 64<<10

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

// handleTrigger deploys a registered app with the definition and the
// image a pipeline sends: app.yaml from the repository, the values of the
// variables it refers to, and the digest of the image it pushed.
func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Without its key the checksum would be a plain hash, which gives
	// short secrets away.
	if len(s.ChecksumKey) == 0 {
		writeError(w, http.StatusInternalServerError, errors.New("this daemon has no checksum key; triggers are off"))
		return
	}

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

	reg, found, err := s.Store.Registration(ctx, req.App)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("%s is not registered; register it on the server with: limand app create %s --repo <repository> --domain <domain>", req.App, req.App))
		return
	}

	a, status, err := s.triggeredApp(req, reg)
	if err != nil {
		writeError(w, status, err)
		return
	}

	// The checksum covers the definition with the image's repository, not
	// its digest: whether the image changed is a question of its own.
	fields := a.Fields()
	checksum := s.checksum(app.Canonical(fields))
	image, err := engine.WithDigest(reg.Repo, req.Digest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	a.Image = image
	if err := a.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid app definition:\n%w", err))
		return
	}

	eng, err := s.Connect(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}

	last, haveLast, err := s.Store.DeployStateOf(ctx, a.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// The same definition and the same image, still running: a pipeline
	// run again. Nothing to do, and nothing to record.
	if haveLast && last.Checksum == checksum && last.Image == image && s.running(ctx, eng, a.Name, last.Release) {
		stream := newEventStream(w)
		stream.send(api.DeployEvent{Type: api.EventOutput, Text: fmt.Sprintf("%s already runs this definition and image (release %s); nothing to do\n", a.Name, last.Release)})
		stream.send(api.DeployEvent{Type: api.EventResult, Result: &api.DeployResult{OK: true, App: a.Name, Release: last.Release, Domain: a.Domain, Unchanged: true}})
		return
	}

	exists, err := eng.ImageExists(ctx, image)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if !exists {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("%s is not in the registry: push the image first, to %s", image, reg.Repo))
		return
	}

	keyed := s.keyedFields(fields)
	var changed []string
	if haveLast {
		changed = changedFields(last.Fields, keyed)
	}
	s.runDeploy(w, r, a, deployRun{
		changed: changed,
		onSuccess: func(ctx context.Context, release string) error {
			return s.Store.SaveDeployState(ctx, store.DeployState{
				App: a.Name, Release: release, Image: image, Checksum: checksum, Fields: keyed,
			})
		},
	})
}

// triggeredApp decodes the definition of a trigger, checks it against the
// registration and fills in its variables. The status goes with the error.
func (s *Server) triggeredApp(req api.TriggerRequest, reg store.Registration) (*app.App, int, error) {
	if req.Definition == "" {
		return nil, http.StatusBadRequest, errors.New("the trigger has no definition; send app.yaml with it (liman trigger --file app.yaml)")
	}
	a, err := app.Decode([]byte(req.Definition))
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if a.Name != req.App {
		return nil, http.StatusBadRequest, fmt.Errorf("the definition is for %q, the trigger for %q", a.Name, req.App)
	}
	// What a deploy key may not change. The answer says what the
	// registration holds: the key's owner may know it, and it says what
	// to fix.
	if repo := engine.Repository(a.Image); repo != reg.Repo {
		return nil, http.StatusForbidden, fmt.Errorf("image: %s is registered with repository %s, not %q; that is changed on the server, not by a pipeline", reg.Name, reg.Repo, repo)
	}
	if a.Domain != reg.Domain {
		return nil, http.StatusForbidden, fmt.Errorf("domain: %s is registered with %s, not %q; that is changed on the server, not by a pipeline", reg.Name, reg.Domain, a.Domain)
	}
	if err := a.FillEnv(req.Vars); err != nil {
		return nil, http.StatusBadRequest, err
	}
	a.Image = reg.Repo
	return a, http.StatusOK, nil
}

// checksum is the HMAC of data with the checksum key, in hexadecimal.
func (s *Server) checksum(data []byte) string {
	mac := hmac.New(sha256.New, s.ChecksumKey)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// keyedFields replaces each value by its HMAC, so that what is kept says
// whether a value changed, never what it is.
func (s *Server) keyedFields(fields map[string]string) map[string]string {
	keyed := make(map[string]string, len(fields))
	for path, value := range fields {
		// The path is part of what is signed: the same value under two
		// names must not look the same.
		keyed[path] = s.checksum([]byte(path + "\x00" + value))
	}
	return keyed
}

// changedFields lists the paths whose value differs between before and
// after, added and removed ones included, sorted.
func changedFields(before, after map[string]string) []string {
	var changed []string
	for path, v := range after {
		if before[path] != v {
			changed = append(changed, path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			changed = append(changed, path)
		}
	}
	slices.Sort(changed)
	return changed
}

// running reports whether release of app is the running container. An
// app whose container was removed behind liman's back is deployed again.
func (s *Server) running(ctx context.Context, eng engine.Engine, name, release string) bool {
	all, err := eng.List(ctx)
	if err != nil {
		return false
	}
	for _, c := range engine.ForApp(all, name) {
		if c.Release == release && c.State == "running" {
			return true
		}
	}
	return false
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
