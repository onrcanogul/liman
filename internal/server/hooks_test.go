package server_test

import (
	"errors"
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

func triggerBody(app, digest string) string {
	return `{"app":"` + app + `","digest":"` + digest + `"}`
}

func TestTriggerDeploysTheImageWithThatDigest(t *testing.T) {
	h := servertest.New(t)
	if _, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	first, second := "ghcr.io/user/orders-api@"+digest("a"), "ghcr.io/user/orders-api@"+digest("b")
	h.Engine.Registry = map[string]bool{first: true, second: true}
	_, ci := h.DeployKey(t, "orders-ci", "orders-api")

	for _, image := range []string{first, second} {
		d := image[strings.Index(image, "@")+1:]
		res, err := ci.Trigger(ctx(), "orders-api", d, io.Discard)
		if err != nil || !res.OK {
			t.Fatalf("Trigger(%s) = %+v, %v", d, res, err)
		}
		if !slices.Contains(h.Engine.Calls, "pull "+image) {
			t.Errorf("%s was not pulled; calls: %v", image, h.Engine.Calls)
		}
	}

	// The second trigger started from the definition written by hand, not
	// from the first trigger's: both are releases of the same app, with
	// the digests asked for.
	rels, err := h.Client.Releases(ctx(), "orders-api", 10)
	if err != nil || len(rels) != 3 {
		t.Fatalf("Releases() = %+v, %v; want 3", rels, err)
	}
	if rels[0].Image != second || rels[1].Image != first || rels[0].Status != "succeeded" {
		t.Errorf("releases = %+v; want %s, then %s", rels, second, first)
	}

	// The audit trail says which key did it.
	entries, err := h.Client.Audit(ctx(), "orders-api", 1)
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Actor, "token:orders-ci@") || !strings.Contains(entries[0].Target, second) {
		t.Errorf("Audit() = %+v, %v", entries, err)
	}
}

func TestTriggerRefusals(t *testing.T) {
	h := servertest.New(t)
	if _, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	// A dry run and a failed deploy are no deploy to repeat.
	if _, err := h.Client.Deploy(ctx(), []byte(definitionFor("billing")), api.DeployOptions{DryRun: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	h.Engine.PullErr = errors.New("no such image")
	if _, err := h.Client.Deploy(ctx(), []byte(definitionFor("shop")), api.DeployOptions{}, io.Discard); !errors.Is(err, api.ErrDeployFailed) {
		t.Fatalf("Deploy() = %v; want a failed deploy", err)
	}
	h.Engine.PullErr = nil

	known := digest("a")
	h.Engine.Registry = map[string]bool{"ghcr.io/user/orders-api@" + known: true}
	key, _ := h.DeployKey(t, "ci", "orders-api", "billing", "shop")
	other, _ := h.DeployKey(t, "billing-ci", "billing")

	tests := []struct {
		name, token, body string
		status            int
		contains          string
	}{
		{"admin token", h.Client.Token, triggerBody("orders-api", known), http.StatusForbidden, "only deploy keys"},
		{"another app's key", other, triggerBody("orders-api", known), http.StatusForbidden, "limited to billing"},
		{"bad digest", key, triggerBody("orders-api", "sha256:abc"), http.StatusBadRequest, "invalid digest"},
		{"bad name", key, triggerBody("../x", known), http.StatusBadRequest, "invalid app name"},
		{"unknown field", key, `{"app":"orders-api","digets":"x"}`, http.StatusBadRequest, "unknown field"},
		{"two objects", key, triggerBody("orders-api", known) + "{}", http.StatusBadRequest, "one JSON object"},
		{"not in the registry", key, triggerBody("orders-api", digest("c")), http.StatusUnprocessableEntity, "not in the registry"},
		{"after a dry run only", key, triggerBody("billing", known), http.StatusNotFound, "deploy it once"},
		{"after a failed deploy only", key, triggerBody("shop", known), http.StatusNotFound, "deploy it once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := trigger(t, h, tt.token, tt.body)
			if status != tt.status || !strings.Contains(body, tt.contains) {
				t.Errorf("trigger = %d %s; want %d with %q", status, body, tt.status, tt.contains)
			}
		})
	}

	t.Run("registry unreachable", func(t *testing.T) {
		h.Engine.ImageExistsErr = errors.New("connection refused")
		defer func() { h.Engine.ImageExistsErr = nil }()
		if status, body := trigger(t, h, key, triggerBody("orders-api", known)); status != http.StatusBadGateway {
			t.Errorf("trigger = %d %s; want 502", status, body)
		}
	})

	// Nothing was deployed by any of it.
	if rels, err := h.Client.Releases(ctx(), "", 10); err != nil || len(rels) != 2 {
		t.Errorf("Releases() = %+v, %v; want the two deploys by hand", rels, err)
	}
}

func TestDeployKeysWorkNowhereElse(t *testing.T) {
	h := servertest.New(t)
	key, _ := h.DeployKey(t, "ci", "orders-api")
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
	h := servertest.New(t)
	if _, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	key, _ := h.DeployKey(t, "ci", "orders-api")
	other, _ := h.DeployKey(t, "ci-2", "orders-api")

	// Digests the registry does not have: each is refused, and counts.
	missing := triggerBody("orders-api", digest("e"))
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

func TestStoppedAppIsNotBroughtBackByATrigger(t *testing.T) {
	h := servertest.New(t)
	if _, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Client.Stop(ctx(), "orders-api"); err != nil {
		t.Fatal(err)
	}
	h.Engine.Registry = map[string]bool{"ghcr.io/user/orders-api@" + digest("a"): true}
	key, _ := h.DeployKey(t, "ci", "orders-api")
	if status, body := trigger(t, h, key, triggerBody("orders-api", digest("a"))); status != http.StatusNotFound {
		t.Errorf("trigger after stop = %d %s; want 404", status, body)
	}
}
