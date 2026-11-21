package server_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/server/servertest"
	"github.com/onrcanogul/liman/internal/store"
)

// digest returns a valid digest made of c.
func digest(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// pipelineApp is app.yaml as a repository would hold it: the repository of
// the image without a version, and a secret by name only.
const pipelineApp = `
name: orders-api
image: ghcr.io/user/orders-api
domain: api.example.com
port: 3000
env:
  LOG_LEVEL: info
  DATABASE_URL: ${DATABASE_URL}
healthcheck:
  path: /health
`

const repo = "ghcr.io/user/orders-api"

// registered returns a harness with orders-api registered, both digests in
// its registry, and a client with a deploy key for it.
func registered(t *testing.T) (*servertest.Harness, string, *api.Client) {
	t.Helper()
	h := servertest.New(t)
	h.Register(t, "orders-api", repo, "api.example.com")
	h.Engine.Registry = map[string]bool{repo + "@" + digest("a"): true, repo + "@" + digest("b"): true}
	key, ci := h.DeployKey(t, "orders-ci", "orders-api")
	return h, key, ci
}

func triggerReq(d, definition, dbURL string) api.TriggerRequest {
	return api.TriggerRequest{App: "orders-api", Digest: d, Definition: definition, Vars: map[string]string{"DATABASE_URL": dbURL}}
}

// trigger posts body to the trigger endpoint with token and returns the
// status and the body of the answer.
func trigger(t *testing.T, h *servertest.Harness, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.HookURL+"/hooks/deploy", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b)
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTriggerDeploysWhatThePipelineSends(t *testing.T) {
	h, _, ci := registered(t)
	const secret = "postgres://app:s3cret@db/orders"

	// The first deploy of the app comes from the pipeline: nothing was
	// deployed by hand.
	res, err := ci.Trigger(ctx(), triggerReq(digest("a"), pipelineApp, secret), io.Discard)
	if err != nil || !res.OK || res.Unchanged || len(res.Changed) != 0 {
		t.Fatalf("first Trigger() = %+v, %v", res, err)
	}
	spec := h.Engine.Specs[len(h.Engine.Specs)-1]
	if spec.Image != repo+"@"+digest("a") || spec.Env["DATABASE_URL"] != secret || spec.Env["LOG_LEVEL"] != "info" {
		t.Errorf("started %s with %v", spec.Image, spec.Env)
	}

	// The same again (a pipeline run twice): nothing happens.
	starts := len(h.Engine.Specs)
	res, err = ci.Trigger(ctx(), triggerReq(digest("a"), "# rerun\n"+pipelineApp, secret), io.Discard)
	if err != nil || !res.OK || !res.Unchanged || len(h.Engine.Specs) != starts {
		t.Errorf("the same Trigger() again = %+v, %v; %d containers started", res, err, len(h.Engine.Specs)-starts)
	}

	// A new image only.
	res, err = ci.Trigger(ctx(), triggerReq(digest("b"), pipelineApp, secret), io.Discard)
	if err != nil || !res.OK || res.Unchanged || len(res.Changed) != 0 {
		t.Errorf("Trigger() with a new image = %+v, %v", res, err)
	}

	// A changed value and a changed secret: named, never shown.
	changed := strings.Replace(pipelineApp, "LOG_LEVEL: info", "LOG_LEVEL: debug", 1)
	res, err = ci.Trigger(ctx(), triggerReq(digest("b"), changed, "postgres://app:n3w@db/orders"), io.Discard)
	if err != nil || !res.OK || !slices.Equal(res.Changed, []string{"env.DATABASE_URL", "env.LOG_LEVEL"}) {
		t.Errorf("Trigger() with changed values = %+v, %v", res, err)
	}
	entries, err := h.Client.Audit(ctx(), "orders-api", 1)
	if err != nil || len(entries) != 1 || !strings.Contains(entries[0].Target, "changed: env.DATABASE_URL, env.LOG_LEVEL") || !strings.HasPrefix(entries[0].Actor, "token:orders-ci@") {
		t.Errorf("Audit() = %+v, %v", entries, err)
	}

	// What is kept says what ran, not what the values were.
	state, found, err := h.Store.DeployStateOf(ctx(), "orders-api")
	if err != nil || !found || state.Image != repo+"@"+digest("b") {
		t.Fatalf("DeployStateOf() = %+v, %v, %v", state, found, err)
	}
	if kept := jsonOf(t, state); strings.Contains(kept, "n3w") || strings.Contains(kept, "debug") {
		t.Errorf("the state keeps values: %s", kept)
	}
}

func TestTriggerRefusals(t *testing.T) {
	h, key, _ := registered(t)
	h.Register(t, "billing", "ghcr.io/user/billing", "billing.example.com")
	other, _ := h.DeployKey(t, "billing-ci", "billing")
	// A key for an app whose registration is gone afterwards: it went
	// with it.
	h.Register(t, "gone", "ghcr.io/user/gone", "gone.example.com")
	goneKey, _ := h.DeployKey(t, "gone-ci", "gone")
	if _, err := h.Store.Unregister(ctx(), "gone"); err != nil {
		t.Fatal(err)
	}

	ok := triggerReq(digest("a"), pipelineApp, "x")
	with := func(change func(*api.TriggerRequest)) string {
		r := ok
		r.Vars = map[string]string{"DATABASE_URL": "x"}
		change(&r)
		return jsonOf(t, r)
	}
	edited := func(from, to string) string {
		return with(func(r *api.TriggerRequest) { r.Definition = strings.Replace(pipelineApp, from, to, 1) })
	}

	tests := []struct {
		name, token, body string
		status            int
		contains          string
	}{
		{"admin token", h.Client.Token, jsonOf(t, ok), http.StatusForbidden, "only deploy and preview keys"},
		{"another app's key", other, jsonOf(t, ok), http.StatusForbidden, "limited to billing"},
		{"key of an unregistered app", goneKey, with(func(r *api.TriggerRequest) {
			r.App = "gone"
			r.Definition = strings.Replace(pipelineApp, "orders-api", "gone", 2)
		}), http.StatusUnauthorized, "invalid token"},
		{"bad digest", key, with(func(r *api.TriggerRequest) { r.Digest = "sha256:abc" }), http.StatusBadRequest, "invalid digest"},
		{"unknown field", key, `{"app":"orders-api","digets":"x"}`, http.StatusBadRequest, "unknown field"},
		{"no definition", key, with(func(r *api.TriggerRequest) { r.Definition = "" }), http.StatusBadRequest, "--file app.yaml"},
		{"another repository", key, edited("image: ghcr.io/user/orders-api", "image: ghcr.io/evil/miner"), http.StatusForbidden, "registered with repository ghcr.io/user/orders-api"},
		{"another domain", key, edited("domain: api.example.com", "domain: evil.example.com"), http.StatusForbidden, "registered with api.example.com"},
		{"definition of another app", key, edited("name: orders-api", "name: billing"), http.StatusBadRequest, "the definition is for"},
		{"missing variable", key, with(func(r *api.TriggerRequest) { r.Vars = nil }), http.StatusBadRequest, "DATABASE_URL, not set"},
		{"invalid definition", key, edited("port: 3000", "port: 0"), http.StatusBadRequest, "port: must be between"},
		{"not in the registry", key, with(func(r *api.TriggerRequest) { r.Digest = digest("c") }), http.StatusUnprocessableEntity, "not in the registry"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A key of its own per case: the refusals count towards the
			// key's limit of triggers a minute.
			token := tt.token
			if token == key {
				token, _ = h.DeployKey(t, fmt.Sprintf("case-%d", i), "orders-api")
			}
			status, body := trigger(t, h, token, tt.body)
			if status != tt.status || !strings.Contains(body, tt.contains) {
				t.Errorf("trigger = %d %s; want %d with %q", status, body, tt.status, tt.contains)
			}
		})
	}

	t.Run("registry unreachable", func(t *testing.T) {
		h.Engine.ImageExistsErr = errors.New("connection refused")
		defer func() { h.Engine.ImageExistsErr = nil }()
		fresh, _ := h.DeployKey(t, "unreachable", "orders-api")
		if status, body := trigger(t, h, fresh, jsonOf(t, ok)); status != http.StatusBadGateway {
			t.Errorf("trigger = %d %s; want 502", status, body)
		}
	})

	// Nothing was deployed by any of it.
	if len(h.Engine.Specs) != 0 {
		t.Errorf("%d containers were started", len(h.Engine.Specs))
	}
}

func TestTriggerDeploysAgainAfterSomethingElseChangedTheApp(t *testing.T) {
	h, _, ci := registered(t)
	run := func(d string) *api.DeployResult {
		t.Helper()
		res, err := ci.Trigger(ctx(), triggerReq(d, pipelineApp, "x"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	run(digest("a"))
	run(digest("b"))

	// A rollback puts a back: the trigger with b must deploy b again.
	if _, err := h.Client.Rollback(ctx(), "orders-api", api.DeployOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if res := run(digest("b")); res.Unchanged {
		t.Error("after a rollback the trigger found nothing to do")
	}

	// A stopped app is deployed again by the next push: it is registered.
	if _, err := h.Client.Stop(ctx(), "orders-api"); err != nil {
		t.Fatal(err)
	}
	if res := run(digest("b")); res.Unchanged || !res.OK {
		t.Errorf("after a stop the trigger = %+v", res)
	}

	// A deploy by hand, with an admin token.
	byHand := strings.Replace(pipelineApp, "image: ghcr.io/user/orders-api", "image: ghcr.io/user/orders-api:1.0", 1)
	byHand = strings.Replace(byHand, "${DATABASE_URL}", "y", 1)
	if _, err := h.Client.Deploy(ctx(), []byte(byHand), api.DeployOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if res := run(digest("b")); res.Unchanged {
		t.Error("after a deploy by hand the trigger found nothing to do")
	}

	// A container removed behind liman's back.
	for name := range h.Engine.Containers {
		delete(h.Engine.Containers, name)
	}
	if res := run(digest("b")); res.Unchanged {
		t.Error("with its container gone the trigger found nothing to do")
	}
}

func TestRegisteredDomainIsReserved(t *testing.T) {
	h, _, _ := registered(t)
	def := strings.Replace(definitionFor("billing"), "domain: billing.example.com", "domain: api.example.com", 1)
	_, err := h.Client.Deploy(ctx(), []byte(def), api.DeployOptions{}, io.Discard)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || !strings.Contains(err.Error(), "used by app orders-api") {
		t.Errorf("Deploy() on a registered domain = %v; want 409", err)
	}
}

func TestDeployKeysWorkNowhereElse(t *testing.T) {
	h, key, _ := registered(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/info"},
		{http.MethodGet, "/v1/apps"},
		{http.MethodGet, "/v1/apps/orders-api/secrets"},
		{http.MethodPost, "/v1/deploys"},
		{http.MethodDelete, "/v1/apps/orders-api"},
	} {
		if status, body := statusOf(t, h, key, c.method, c.path); status != http.StatusForbidden || !strings.Contains(body, "deploy key") {
			t.Errorf("%s %s = %d %s; want 403", c.method, c.path, status, body)
		}
	}
	// And the hooks listener has nothing but the trigger.
	req, _ := http.NewRequest(http.MethodGet, h.HookURL+"/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/apps on the hooks listener = %d; want 404", resp.StatusCode)
	}
}

func TestTriggersPerKeyAreLimited(t *testing.T) {
	h, key, _ := registered(t)
	other, _ := h.DeployKey(t, "ci-2", "orders-api")

	// A digest the registry does not have: each is refused, and counts.
	missing := jsonOf(t, triggerReq(digest("e"), pipelineApp, "x"))
	for i := range 6 {
		if status, body := trigger(t, h, key, missing); status != http.StatusUnprocessableEntity {
			t.Fatalf("trigger %d = %d %s; want 422", i+1, status, body)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, h.HookURL+"/hooks/deploy", strings.NewReader(missing))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Errorf("seventh trigger = %d, Retry-After %q; want 429 with a Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}

	// The limit is the key's, not the app's.
	if status, body := trigger(t, h, other, missing); status != http.StatusUnprocessableEntity {
		t.Errorf("another key = %d %s; want 422", status, body)
	}
}

// previewKey returns a client with a preview key for orders-api.
func previewKey(t *testing.T, h *servertest.Harness, name string) (string, *api.Client) {
	t.Helper()
	token, err := h.Store.CreateScopedToken(ctx(), name, store.ScopePreview, time.Time{}, "orders-api")
	if err != nil {
		t.Fatal(err)
	}
	return token, &api.Client{BaseURL: h.HookURL, Token: token}
}

func TestPreviewComparesTheDefinitionAsWritten(t *testing.T) {
	h, _, ci := registered(t)
	_, pr := previewKey(t, h, "orders-pr")
	preview := func(definition, d string) *api.Preview {
		t.Helper()
		p, err := pr.Preview(ctx(), api.TriggerRequest{App: "orders-api", Digest: d, Definition: definition})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Before any trigger there is nothing to compare with.
	if p := preview(pipelineApp, ""); !p.OK || p.Compared || p.Running != "" || !slices.Equal(p.Variables, []string{"DATABASE_URL"}) {
		t.Errorf("first preview = %+v", p)
	}

	if _, err := ci.Trigger(ctx(), triggerReq(digest("a"), pipelineApp, "secret-1"), io.Discard); err != nil {
		t.Fatal(err)
	}
	starts := len(h.Engine.Specs)

	// The same file: no change, whatever the secret's value is now.
	p := preview(pipelineApp, digest("a"))
	if !p.OK || !p.Compared || p.Running == "" || len(p.Changed)+len(p.Added)+len(p.Removed) != 0 || p.Image == nil || !p.Image.Exists || !p.Image.Running {
		t.Errorf("preview of the same = %+v (image %+v)", p, p.Image)
	}

	// A changed value, an added one, a removed one, a renamed variable.
	edited := strings.Replace(pipelineApp, "LOG_LEVEL: info", "LOG_LEVEL: debug\n  NEW_FLAG: on", 1)
	edited = strings.Replace(edited, "${DATABASE_URL}", "${DB_URL}", 1)
	edited = strings.Replace(edited, "healthcheck:\n  path: /health\n", "", 1)
	p = preview(edited, digest("b"))
	if !p.OK || !slices.Equal(p.Changed, []string{"env.DATABASE_URL", "env.LOG_LEVEL"}) || !slices.Equal(p.Added, []string{"env.NEW_FLAG"}) || !slices.Equal(p.Removed, []string{"healthcheck.path"}) {
		t.Errorf("preview of an edited file = %+v", p)
	}
	if p.Image == nil || !p.Image.Exists || p.Image.Running || !slices.Equal(p.Variables, []string{"DB_URL"}) {
		t.Errorf("preview image %+v, variables %v", p.Image, p.Variables)
	}

	// What is wrong is the answer, not an error.
	p = preview(strings.Replace(pipelineApp, "port: 3000", "port: 0", 1), digest("c"))
	if p.OK || len(p.Problems) != 2 || !strings.Contains(p.Problems[0], "port") || !strings.Contains(p.Problems[1], "not in the registry") {
		t.Errorf("preview of a broken definition = %+v", p)
	}
	p = preview(strings.Replace(pipelineApp, "domain: api.example.com", "domain: evil.example.com", 1), "")
	if p.OK || len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "registered with api.example.com") {
		t.Errorf("preview with another domain = %+v", p)
	}

	// Nothing was deployed or recorded by any of it.
	if len(h.Engine.Specs) != starts {
		t.Errorf("a preview started %d containers", len(h.Engine.Specs)-starts)
	}
	if entries, _ := h.Client.Audit(ctx(), "orders-api", 10); len(entries) != 1 {
		t.Errorf("audit has %d entries; want the one deploy", len(entries))
	}
}

func TestPreviewKeysPreviewOnly(t *testing.T) {
	h, deployKey, _ := registered(t)
	key, _ := previewKey(t, h, "orders-pr")

	if status, body := trigger(t, h, key, jsonOf(t, triggerReq(digest("a"), pipelineApp, "x"))); status != http.StatusForbidden || !strings.Contains(body, "preview key") {
		t.Errorf("a preview key on /hooks/deploy = %d %s; want 403", status, body)
	}
	if status, body := statusOf(t, h, key, http.MethodGet, "/v1/apps"); status != http.StatusForbidden || !strings.Contains(body, "preview key") {
		t.Errorf("a preview key on the API = %d %s; want 403", status, body)
	}
	post := func(token, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, h.HookURL+"/hooks/preview", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	// A deploy key may preview; an admin token may not.
	clean := jsonOf(t, api.TriggerRequest{App: "orders-api", Definition: pipelineApp})
	if status, body := post(deployKey, clean); status != http.StatusOK {
		t.Errorf("a deploy key previewing = %d %s", status, body)
	}
	if status, _ := post(h.Client.Token, clean); status != http.StatusForbidden {
		t.Errorf("an admin token previewing = %d; want 403", status)
	}
	// Secrets are refused, so that nobody wires them into a pull request.
	if status, body := post(key, jsonOf(t, triggerReq("", pipelineApp, "secret"))); status != http.StatusBadRequest || !strings.Contains(body, "takes no variable values") {
		t.Errorf("a preview with variable values = %d %s; want 400", status, body)
	}
}

const workerApp = `
name: mailer
image: ghcr.io/user/mailer
kind: worker
command: [node, mailer.js]
healthcheck:
  settle: 20ms
`

func TestTriggerDeploysAWorker(t *testing.T) {
	h := servertest.New(t)
	const workerRepo = "ghcr.io/user/mailer"
	if err := h.Store.Register(ctx(), store.Registration{Name: "mailer", Repo: workerRepo, Kind: "worker"}); err != nil {
		t.Fatal(err)
	}
	h.Engine.Registry = map[string]bool{workerRepo + "@" + digest("a"): true}
	_, ci := h.DeployKey(t, "mailer-ci", "mailer")

	res, err := ci.Trigger(ctx(), api.TriggerRequest{App: "mailer", Digest: digest("a"), Definition: workerApp}, io.Discard)
	if err != nil || !res.OK || !res.Worker || res.Routed || res.HostPort != 0 {
		t.Fatalf("Trigger() = %+v, %v; want a worker, routed nowhere", res, err)
	}
	spec := h.Engine.Specs[len(h.Engine.Specs)-1]
	if spec.ContainerPort != 0 || spec.Kind != "worker" || strings.Join(spec.Command, " ") != "node mailer.js" {
		t.Errorf("started %+v", spec)
	}
}

// A worker is registered as one, and its key cannot put it on the
// internet; nor can a web app's key make it a worker.
func TestTriggerCannotChangeTheKind(t *testing.T) {
	h, key, _ := registered(t)
	if err := h.Store.Register(ctx(), store.Registration{Name: "mailer", Repo: "ghcr.io/user/mailer", Kind: "worker"}); err != nil {
		t.Fatal(err)
	}
	h.Engine.Registry["ghcr.io/user/mailer@"+digest("a")] = true
	workerKey, _ := h.DeployKey(t, "mailer-ci", "mailer")

	asWeb := `
name: mailer
image: ghcr.io/user/mailer
domain: mail.example.com
port: 80
`
	status, body := trigger(t, h, workerKey, jsonOf(t, api.TriggerRequest{App: "mailer", Digest: digest("a"), Definition: asWeb}))
	if status != http.StatusForbidden || !strings.Contains(body, "registered as a worker") {
		t.Errorf("a worker's key deploying a web app: %d %s", status, body)
	}

	asWorker := `
name: orders-api
image: ghcr.io/user/orders-api
kind: worker
`
	status, body = trigger(t, h, key, jsonOf(t, api.TriggerRequest{App: "orders-api", Digest: digest("a"), Definition: asWorker}))
	if status != http.StatusForbidden || !strings.Contains(body, "registered as a web app") {
		t.Errorf("a web app's key deploying a worker: %d %s", status, body)
	}
}
