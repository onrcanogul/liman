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
	"strings"
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

// HookHandler returns the handler for the endpoints pipelines call: POST
// /hooks/deploy and POST /hooks/preview, and nothing else. It is meant for
// a listener of its own that a reverse proxy exposes to the internet, so
// that the management API stays local whatever that proxy is told.
func (s *Server) HookHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/deploy", s.handleTrigger)
	mux.HandleFunc("POST /hooks/preview", s.handlePreview)

	// The same middleware as the API, in the same order (see Handler),
	// except that only deploy and preview keys get through.
	var h http.Handler = mux
	h = s.authenticate(h, triggerEndpoint)
	h = s.rejectBrowsers(h)
	h = s.logRequests(h)
	h = s.limitConcurrency(h)
	h = s.secureHeaders(h)
	h = s.recoverPanics(h)
	return h
}

// pipelineRequest reads what both hooks take, and checks what they share:
// the body, the app and the key's right to it, the key's rate, and the
// registration. It has answered the client when ok is false.
func (s *Server) pipelineRequest(w http.ResponseWriter, r *http.Request, needDigest bool) (req api.TriggerRequest, reg store.Registration, ok bool) {
	ctx := r.Context()
	// Without its key the checksum would be a plain hash, which gives
	// short secrets away.
	if len(s.ChecksumKey) == 0 {
		writeError(w, http.StatusInternalServerError, errors.New("this daemon has no checksum key; triggers are off"))
		return req, reg, false
	}

	body, err := s.readBody(w, r, maxTriggerBytes)
	if err != nil {
		writeError(w, bodyErrorStatus(err), fmt.Errorf("read request: %w", err))
		return req, reg, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	// A misspelled field ("digets") is an error, not an empty digest.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read request: %w", err))
		return req, reg, false
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, errors.New("read request: one JSON object expected"))
		return req, reg, false
	}
	if !app.ValidName(req.App) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid app name %q", req.App))
		return req, reg, false
	}
	if (needDigest || req.Digest != "") && !engine.ValidDigest(req.Digest) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid digest %q: want sha256: and 64 hexadecimal digits", req.Digest))
		return req, reg, false
	}
	if !s.mayUse(w, r, req.App) {
		return req, reg, false
	}

	// Counted from here, whatever the outcome: asking the registry about
	// digests that do not exist costs too.
	m := metaOf(ctx)
	if wait, ok := s.triggers.allow(m.token+"\x00"+m.created.Format(time.RFC3339Nano), time.Now()); !ok {
		w.Header().Set("Retry-After", retryAfter(wait))
		writeError(w, http.StatusTooManyRequests, fmt.Errorf("this key may be used %d times a minute; try again in %s", triggersPerMinute, wait.Round(time.Second)))
		return req, reg, false
	}

	reg, found, err := s.Store.Registration(ctx, req.App)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return req, reg, false
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("%s is not registered; register it on the server with: limand app create %s --repo <repository> --domain <domain>", req.App, req.App))
		return req, reg, false
	}
	return req, reg, true
}

// handleTrigger deploys a registered app with the definition and the
// image a pipeline sends: app.yaml from the repository, the values of the
// variables it refers to, and the digest of the image it pushed.
func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// The hooks listener takes preview keys too; they deploy nothing.
	if metaOf(ctx).scope != store.ScopeDeploy {
		writeError(w, http.StatusForbidden, errors.New("this is a preview key; it can only preview (liman preview), not deploy"))
		return
	}
	req, reg, ok := s.pipelineRequest(w, r, true)
	if !ok {
		return
	}

	a, status, err := s.checkDefinition(req, reg)
	if err != nil {
		writeError(w, status, err)
		return
	}
	// As written, ${NAME} and all, for previews to compare with; then
	// with the values it runs with.
	templates := s.keyedFields(a.Fields())
	if err := a.FillEnv(req.Vars); err != nil {
		writeError(w, http.StatusBadRequest, err)
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
				App: a.Name, Release: release, Image: image, Checksum: checksum, Fields: keyed, Templates: templates,
			})
		},
	})
}

// zeroDigest stands in for the digest of a preview without one: the
// definition is validated with it, and nothing is asked about it.
var zeroDigest = "sha256:" + strings.Repeat("0", 64)

// handlePreview says what a deploy of this definition would change, and
// what is wrong with it, without deploying or recording anything. It is
// for pull requests: it needs no secrets, only the names of the variables
// it compares. The image is checked only when its digest is given.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, reg, ok := s.pipelineRequest(w, r, false)
	if !ok {
		return
	}
	// A pull request's pipeline must not hold the secrets: refusing them
	// says so to whoever wires it up.
	if len(req.Vars) > 0 {
		writeError(w, http.StatusBadRequest, errors.New("a preview takes no variable values: it compares app.yaml as written, and a pull request's pipeline should not hold the secrets"))
		return
	}

	p := api.Preview{App: req.App}
	reply := func() {
		p.OK = len(p.Problems) == 0
		writeJSON(w, http.StatusOK, p)
	}

	// What is wrong with the definition is the answer, not an error.
	a, _, err := s.checkDefinition(req, reg)
	if err != nil {
		p.Problems = append(p.Problems, err.Error())
		reply()
		return
	}
	values := make([]string, 0, len(a.Env))
	for _, v := range a.Env {
		values = append(values, v)
	}
	p.Variables = app.Variables([]byte(strings.Join(values, "\n")))
	templates := s.keyedFields(a.Fields())

	digest := req.Digest
	if digest == "" {
		digest = zeroDigest
	}
	image := reg.Repo + "@" + digest
	a.Image = image
	if err := a.Validate(); err != nil {
		p.Problems = append(p.Problems, strings.Split(err.Error(), "\n")...)
	}

	last, haveLast, err := s.Store.DeployStateOf(ctx, a.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if haveLast {
		p.Running = last.Release
		// A state saved before templates were kept has nothing to compare.
		if len(last.Templates) > 0 {
			p.Compared = true
			p.Changed, p.Added, p.Removed = splitChanges(last.Templates, templates)
		}
	}

	if req.Digest != "" {
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
		p.Image = &api.PreviewImage{Digest: req.Digest, Exists: exists, Running: haveLast && last.Image == image}
		if !exists {
			p.Problems = append(p.Problems, fmt.Sprintf("%s is not in the registry", image))
		}
	}
	reply()
}

// checkDefinition decodes the definition a pipeline sent and checks it
// against the registration. Its variables are not filled in, and its
// image is the registered repository. The status goes with the error.
func (s *Server) checkDefinition(req api.TriggerRequest, reg store.Registration) (*app.App, int, error) {
	if req.Definition == "" {
		return nil, http.StatusBadRequest, errors.New("no definition was sent; send app.yaml with it (--file app.yaml)")
	}
	a, err := app.Decode([]byte(req.Definition))
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if a.Name != req.App {
		return nil, http.StatusBadRequest, fmt.Errorf("the definition is for %q, the request for %q", a.Name, req.App)
	}
	// What a pipeline may not change. The answer says what the
	// registration holds: the key's owner may know it, and it says what
	// to fix.
	if repo := engine.Repository(a.Image); repo != reg.Repo {
		return nil, http.StatusForbidden, fmt.Errorf("image: %s is registered with repository %s, not %q; that is changed on the server, not by a pipeline", reg.Name, reg.Repo, repo)
	}
	if a.Domain != reg.Domain {
		return nil, http.StatusForbidden, fmt.Errorf("domain: %s is registered with %s, not %q; that is changed on the server, not by a pipeline", reg.Name, reg.Domain, a.Domain)
	}
	a.Image = reg.Repo
	return a, http.StatusOK, nil
}

// splitChanges sorts the paths that differ between before and after into
// changed, added and removed ones, each sorted.
func splitChanges(before, after map[string]string) (changed, added, removed []string) {
	for path, v := range after {
		old, ok := before[path]
		switch {
		case !ok:
			added = append(added, path)
		case old != v:
			changed = append(changed, path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			removed = append(removed, path)
		}
	}
	slices.Sort(changed)
	slices.Sort(added)
	slices.Sort(removed)
	return changed, added, removed
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
