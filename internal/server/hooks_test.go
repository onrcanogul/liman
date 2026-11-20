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

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/server/servertest"
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
	// A key for an app whose registration is gone afterwards.
	h.Register(t, "gone", "ghcr.io/user/gone", "gone.example.com")
	goneKey, _ := h.DeployKey(t, "gone-ci", "gone")
	if err := h.Store.Unregister(ctx(), "gone"); err != nil {
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
		{"admin token", h.Client.Token, jsonOf(t, ok), http.StatusForbidden, "only deploy keys"},
		{"another app's key", other, jsonOf(t, ok), http.StatusForbidden, "limited to billing"},
		{"not registered", goneKey, with(func(r *api.TriggerRequest) {
			r.App = "gone"
			r.Definition = strings.Replace(pipelineApp, "orders-api", "gone", 2)
		}), http.StatusNotFound, "limand app create gone"},
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
