// The tests live in package server_test (not api) for two reasons: they use
// the package the way an outside caller would, and apitest imports api, so
// tests inside package api could not import apitest without a cycle.
package server_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/panel"
	"github.com/onrcanogul/liman/internal/secrets"
	"github.com/onrcanogul/liman/internal/server"
	"github.com/onrcanogul/liman/internal/server/servertest"
	"github.com/onrcanogul/liman/internal/store"
)

const definition = `
name: orders-api
image: ghcr.io/user/orders-api:1.4
domain: api.example.com
port: 3000
healthcheck:
  path: /health
`

func ctx() context.Context { return context.Background() }

// --- authentication --------------------------------------------------------

func TestAuthentication(t *testing.T) {
	h := servertest.New(t)

	tests := []struct {
		name       string
		header     string // Authorization header; empty means none
		path       string
		wantStatus int
	}{
		{"valid token", "Bearer " + h.Client.Token, "/v1/apps", http.StatusOK},
		{"no header", "", "/v1/apps", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + h.Client.Token, "/v1/apps", http.StatusUnauthorized},
		{"unknown token", "Bearer " + store.TokenPrefix + strings.Repeat("0", 64), "/v1/apps", http.StatusUnauthorized},
		{"token without prefix", "Bearer nonsense", "/v1/apps", http.StatusUnauthorized},
		{"health needs no token", "", "/v1/health", http.StatusOK},
		{"unknown path still needs a token", "", "/v1/nope", http.StatusUnauthorized},
		{"unknown path with token", "Bearer " + h.Client.Token, "/v1/nope", http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, h.URL+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") != "Bearer" {
				t.Error("a 401 response must name the expected scheme in WWW-Authenticate")
			}
		})
	}
}

func TestRevokedTokenStopsWorking(t *testing.T) {
	h := servertest.New(t)

	if _, err := h.Client.Apps(ctx()); err != nil {
		t.Fatalf("Apps() before revoke error = %v", err)
	}
	if err := h.Store.RevokeToken(ctx(), "test"); err != nil {
		t.Fatal(err)
	}

	_, err := h.Client.Apps(ctx())

	var apiErr *api.Error
	// errors.As finds an error of a given type in the chain and assigns it,
	// which gives access to its fields.
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Errorf("Apps() after revoke error = %v, want a 401", err)
	}
}

func TestWrongMethodIsRejected(t *testing.T) {
	h := servertest.New(t)

	req, _ := http.NewRequest(http.MethodPut, h.URL+"/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer "+h.Client.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// --- deploy ----------------------------------------------------------------

func TestDeployStreamsOutputAndRecordsEverything(t *testing.T) {
	h := servertest.New(t)
	var out bytes.Buffer

	res, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, &out)
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if !res.OK || res.App != "orders-api" || res.HostPort != 40001 || res.Release == "" || res.Routed {
		t.Errorf("result = %+v", res)
	}
	for _, want := range []string{"==> [1/5] Pull image ghcr.io/user/orders-api:1.4", "==> [5/5] Retire previous containers of orders-api"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, out.String())
		}
	}
	if len(h.Engine.Containers) != 1 {
		t.Errorf("%d containers running, want 1", len(h.Engine.Containers))
	}

	events, err := h.Client.History(ctx(), "orders-api", 50)
	if err != nil || len(events) != 5 {
		t.Fatalf("History() = %d events, %v; want 5", len(events), err)
	}
	if events[0].Release != res.Release || !events[0].OK {
		t.Errorf("first event = %+v", events[0])
	}

	releases, err := h.Store.Releases(ctx(), "orders-api", 10)
	if err != nil || len(releases) != 1 {
		t.Fatalf("Releases() = %v, %v; want one", releases, err)
	}
	if rel := releases[0]; rel.Status != store.StatusSucceeded || rel.Image != "ghcr.io/user/orders-api:1.4" || rel.Release != res.Release {
		t.Errorf("release = %+v", rel)
	}
}

func TestDeployFailureIsReportedAndRolledBack(t *testing.T) {
	h := servertest.New(t)
	h.Engine.Add(engine.Container{Name: "orders-api-old", App: "orders-api"})
	h.HealthErr = errors.New("connection refused")
	var out bytes.Buffer

	res, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, &out)

	if !errors.Is(err, api.ErrDeployFailed) {
		t.Fatalf("Deploy() error = %v, want ErrDeployFailed", err)
	}
	if !strings.Contains(err.Error(), "Health check: not healthy after 100ms: connection refused") {
		t.Errorf("error %q does not carry the daemon's explanation", err)
	}
	if res == nil || res.OK || res.Routed {
		t.Errorf("result = %+v, want a failed result", res)
	}
	if !strings.Contains(out.String(), "<== Undo: Start container") {
		t.Errorf("output does not show the rollback:\n%s", out.String())
	}
	// Only the old container is left.
	if _, ok := h.Engine.Containers["orders-api-old"]; !ok || len(h.Engine.Containers) != 1 {
		t.Errorf("containers = %v, want only the previous one", h.Engine.Containers)
	}

	releases, _ := h.Store.Releases(ctx(), "orders-api", 10)
	if len(releases) != 1 || releases[0].Status != store.StatusFailed || releases[0].Error == "" {
		t.Errorf("releases = %+v, want one failed release with its error", releases)
	}
}

func TestDeployRejectsInvalidDefinition(t *testing.T) {
	h := servertest.New(t)

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantErr    string
	}{
		{"invalid field", strings.Replace(definition, ":1.4", "", 1), http.StatusBadRequest, "must include a tag"},
		{"not yaml", "name: [", http.StatusBadRequest, "parse yaml"},
		{"unknown key", definition + "replicas: 3\n", http.StatusBadRequest, "replicas"},
		{"too large", "# " + strings.Repeat("x", 2<<20), http.StatusRequestEntityTooLarge, "too large"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.Client.Deploy(ctx(), []byte(tt.body), api.DeployOptions{}, io.Discard)

			var apiErr *api.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("Deploy() error = %v, want an API error", err)
			}
			if apiErr.Status != tt.wantStatus || !strings.Contains(apiErr.Message, tt.wantErr) {
				t.Errorf("error = %d %q, want %d containing %q", apiErr.Status, apiErr.Message, tt.wantStatus, tt.wantErr)
			}
			if len(h.Engine.Calls) != 0 {
				t.Errorf("engine was used for an invalid definition: %v", h.Engine.Calls)
			}
		})
	}
}

func TestDeployWhenDockerIsDown(t *testing.T) {
	h := servertest.New(t)
	h.ConnectErr = servertest.ErrDockerDown

	_, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard)

	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("Deploy() error = %v, want a 503", err)
	}

	// The failed attempt must not leave the app locked.
	h.ConnectErr = nil
	if _, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard); err != nil {
		t.Errorf("Deploy() after Docker came back error = %v", err)
	}
}

func TestDryRunNeedsNoDockerAndChangesNothing(t *testing.T) {
	h := servertest.New(t)
	h.ConnectErr = servertest.ErrDockerDown
	var out bytes.Buffer

	res, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{DryRun: true}, &out)
	if err != nil {
		t.Fatalf("Deploy(dry run) error = %v", err)
	}

	if !res.DryRun || res.HostPort != 0 {
		t.Errorf("result = %+v, want a dry-run result without a port", res)
	}
	if !strings.Contains(out.String(), "$ docker pull ghcr.io/user/orders-api:1.4") {
		t.Errorf("output does not show the manual command:\n%s", out.String())
	}

	events, _ := h.Client.History(ctx(), "", 50)
	releases, _ := h.Store.Releases(ctx(), "orders-api", 10)
	if len(events) != 0 || len(releases) != 0 {
		t.Errorf("dry run recorded %d events and %d releases, want none", len(events), len(releases))
	}
}

func TestDeployWithProxy(t *testing.T) {
	h := servertest.New(t)
	proxy := h.WithProxy(t, "true")

	res, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard)
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if !res.Routed || res.Domain != "api.example.com" {
		t.Errorf("result = %+v, want it routed to api.example.com", res)
	}
	conf, err := os.ReadFile(proxy.Path("orders-api"))
	if err != nil || !strings.Contains(string(conf), "server 127.0.0.1:40001;") {
		t.Errorf("proxy config = %q, %v; want it to point at the new container", conf, err)
	}
}

// --- step-by-step mode ------------------------------------------------------

func TestStepModeAsksForEveryStep(t *testing.T) {
	h := servertest.New(t)

	var asked []string
	opts := api.DeployOptions{Confirm: func(step string) (bool, error) {
		asked = append(asked, step)
		return true, nil
	}}

	res, err := h.Client.Deploy(ctx(), []byte(definition), opts, io.Discard)
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if !res.OK || len(asked) != 5 || asked[0] != "Pull image ghcr.io/user/orders-api:1.4" {
		t.Errorf("asked %d times (%v), result %+v; want 5 questions and success", len(asked), asked, res)
	}
}

func TestStepModeDeclineRollsBack(t *testing.T) {
	h := servertest.New(t)

	opts := api.DeployOptions{Confirm: func(step string) (bool, error) {
		return !strings.HasPrefix(step, "Health check"), nil
	}}

	_, err := h.Client.Deploy(ctx(), []byte(definition), opts, io.Discard)

	if !errors.Is(err, api.ErrDeployFailed) || !strings.Contains(err.Error(), `aborted before "Health check"`) {
		t.Fatalf("Deploy() error = %v, want an abort before the health check", err)
	}
	if len(h.Engine.Containers) != 0 {
		t.Errorf("%d containers left after the abort, want none", len(h.Engine.Containers))
	}
}

func TestStepModeTimesOutWithoutAnAnswer(t *testing.T) {
	h := servertest.New(t)
	h.Server.ConfirmTimeout = 50 * time.Millisecond

	// A client that never answers: it reads the stream but ignores the
	// question. The raw HTTP API is used because api.Client always answers.
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/v1/deploys?step=true", strings.NewReader(definition))
	req.Header.Set("Authorization", "Bearer "+h.Client.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "no answer within 50ms") {
		t.Errorf("stream does not report the timeout:\n%s", body)
	}

	// The lock must be free again.
	if _, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard); err != nil {
		t.Errorf("Deploy() after the timeout error = %v", err)
	}
}

func TestConfirmUnknownID(t *testing.T) {
	h := servertest.New(t)

	req, _ := http.NewRequest(http.MethodPost, h.URL+"/v1/confirmations/does-not-exist", strings.NewReader(`{"approve":true}`))
	req.Header.Set("Authorization", "Bearer "+h.Client.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- locking ----------------------------------------------------------------

func TestSecondDeployOfSameAppIsRejected(t *testing.T) {
	h := servertest.New(t)

	// The first deploy runs in step mode and parks at its first question.
	// asked is closed when it gets there; release lets it continue.
	asked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	firstDone := make(chan error, 1)
	go func() {
		opts := api.DeployOptions{Confirm: func(string) (bool, error) {
			once.Do(func() { close(asked) })
			<-release
			return true, nil
		}}
		_, err := h.Client.Deploy(ctx(), []byte(definition), opts, io.Discard)
		firstDone <- err
	}()

	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the first deploy never asked its first question")
	}

	// While it waits, a second deploy of the same app must be refused...
	_, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Errorf("second Deploy() error = %v, want a 409", err)
	}
	// ...and so must stopping it.
	if _, err := h.Client.Stop(ctx(), "orders-api"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Errorf("Stop() during a deploy error = %v, want a 409", err)
	}
	// A different app is unaffected.
	other := strings.Replace(definition, "name: orders-api", "name: other", 1)
	if _, err := h.Client.Deploy(ctx(), []byte(other), api.DeployOptions{DryRun: true}, io.Discard); err != nil {
		t.Errorf("Deploy() of another app error = %v", err)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Deploy() error = %v", err)
	}

	// Once the first has finished, the app can be deployed again.
	if _, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard); err != nil {
		t.Errorf("Deploy() after the first finished error = %v", err)
	}
}

// --- client disconnect ------------------------------------------------------

func TestClientDisconnectRollsBack(t *testing.T) {
	h := servertest.New(t)
	h.Engine.Add(engine.Container{Name: "orders-api-old", App: "orders-api"})

	// The client gives up at the question before the last step, as Ctrl+C
	// would. By then the new container has been started.
	clientCtx, cancel := context.WithCancel(ctx())
	opts := api.DeployOptions{Confirm: func(step string) (bool, error) {
		if strings.HasPrefix(step, "Retire previous") {
			cancel()
			return false, clientCtx.Err()
		}
		return true, nil
	}}

	_, err := h.Client.Deploy(clientCtx, []byte(definition), opts, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Deploy() error = %v, want context.Canceled", err)
	}

	// The daemon keeps working after the client is gone; give it a moment
	// to finish the rollback, then check the result.
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, _ := h.Store.Events(ctx(), "orders-api", 50)
		undone := false
		for _, ev := range events {
			if ev.Action == "undo" && ev.OK {
				undone = true
			}
		}
		if undone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon did not roll back after the client disconnected")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, ok := h.Engine.Containers["orders-api-old"]; !ok || len(h.Engine.Containers) != 1 {
		t.Errorf("containers = %v, want only the previous one", h.Engine.Containers)
	}
}

// --- apps, logs, stop, history ---------------------------------------------

func TestApps(t *testing.T) {
	h := servertest.New(t)

	got, err := h.Client.Apps(ctx())
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("Apps() with nothing running = %v, %v; want an empty, non-nil list", got, err)
	}

	h.Engine.Add(engine.Container{
		Name: "web-r1", App: "web", Image: "web:1.0", Status: "Up 2 minutes", HostPort: 40123,
		Release: "r1", Domain: "web.example.com", HealthPath: "/health",
	})

	got, err = h.Client.Apps(ctx())
	if err != nil || len(got) != 1 {
		t.Fatalf("Apps() = %+v, %v; want one container", got, err)
	}
	if got[0].Created.IsZero() {
		t.Error("Apps() reports no creation time")
	}
	want := api.Container{
		App: "web", Container: "web-r1", Image: "web:1.0", State: "running", Status: "Up 2 minutes", HostPort: 40123,
		Release: "r1", Domain: "web.example.com", Healthcheck: "/health", Created: got[0].Created, Role: "current",
	}
	if got[0] != want {
		t.Errorf("Apps() = %+v, want %+v", got[0], want)
	}

	h.ConnectErr = servertest.ErrDockerDown
	var apiErr *api.Error
	if _, err := h.Client.Apps(ctx()); !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Errorf("Apps() with Docker down error = %v, want a 503", err)
	}
}

func TestLogs(t *testing.T) {
	h := servertest.New(t)
	h.Engine.Add(engine.Container{Name: "web-r1", App: "web"})
	h.Engine.Add(engine.Container{Name: "web-r2", App: "web"})
	h.Engine.LogOutput = "listening on :3000\n"
	var out bytes.Buffer

	if err := h.Client.Logs(ctx(), "web", false, &out); err != nil {
		t.Fatalf("Logs() error = %v", err)
	}

	if out.String() != "listening on :3000\n" {
		t.Errorf("logs = %q", out.String())
	}
	if last := h.Engine.Calls[len(h.Engine.Calls)-1]; last != "logs web-r2" {
		t.Errorf("last engine call = %q, want the logs of the newest container", last)
	}

	var apiErr *api.Error
	if err := h.Client.Logs(ctx(), "nope", false, io.Discard); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Errorf("Logs() of an unknown app error = %v, want a 404", err)
	}
}

func TestStop(t *testing.T) {
	h := servertest.New(t)
	proxy := h.WithProxy(t, "true")
	h.Engine.Add(engine.Container{Name: "web-r1", App: "web"})
	h.Engine.Add(engine.Container{Name: "web-r2", App: "web"})
	h.Engine.Add(engine.Container{Name: "other-r1", App: "other"})
	if err := proxy.Write("web", []byte("server {}\n")); err != nil {
		t.Fatal(err)
	}

	res, err := h.Client.Stop(ctx(), "web")
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if len(res.Removed) != 2 || res.ProxyConfig != proxy.Path("web") {
		t.Errorf("result = %+v, want two containers and the proxy config removed", res)
	}
	if _, ok := h.Engine.Containers["other-r1"]; !ok || len(h.Engine.Containers) != 1 {
		t.Errorf("containers = %v, want only other-r1", h.Engine.Containers)
	}
	if _, err := os.Stat(proxy.Path("web")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("proxy config still exists (stat error: %v)", err)
	}

	var apiErr *api.Error
	if _, err := h.Client.Stop(ctx(), "web"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Errorf("Stop() twice error = %v, want a 404", err)
	}
}

func TestStopKeepsEverythingWhenReloadFails(t *testing.T) {
	h := servertest.New(t)
	proxy := h.WithProxy(t, "false")
	h.Engine.Add(engine.Container{Name: "web-r1", App: "web"})
	if err := proxy.Write("web", []byte("server {}\n")); err != nil {
		t.Fatal(err)
	}

	_, err := h.Client.Stop(ctx(), "web")

	if err == nil || !strings.Contains(err.Error(), "nginx reload failed") {
		t.Fatalf("Stop() error = %v, want the reload failure", err)
	}
	// The proxy still serves the old config, so the file must be back and
	// the container it points at must still be running.
	if conf, _ := os.ReadFile(proxy.Path("web")); string(conf) != "server {}\n" {
		t.Errorf("config after the failed stop = %q, want the original restored", conf)
	}
	if len(h.Engine.Containers) != 1 {
		t.Error("the container was removed although the proxy still routes to it")
	}
}

func TestHistoryLimit(t *testing.T) {
	h := servertest.New(t)

	tests := []struct {
		query      string
		wantStatus int
	}{
		{"", http.StatusOK},
		{"?limit=5", http.StatusOK},
		{"?limit=0", http.StatusBadRequest},
		{"?limit=abc", http.StatusBadRequest},
		{"?limit=100000", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, h.URL+"/v1/history"+tt.query, nil)
			req.Header.Set("Authorization", "Bearer "+h.Client.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}

func TestHealth(t *testing.T) {
	h := servertest.New(t)
	// A client without a token.
	anonymous := &api.Client{BaseURL: h.URL}

	got, err := anonymous.Health(ctx())
	if err != nil || got.Status != "ok" || got.Version == "" {
		t.Errorf("Health() = %+v, %v; want status ok and a version", got, err)
	}
}

func TestClientExplainsUnreachableDaemon(t *testing.T) {
	c := &api.Client{BaseURL: "http://127.0.0.1:1", Token: "x"} // nothing listens on port 1

	_, err := c.Apps(ctx())

	if err == nil || !strings.Contains(err.Error(), "is limand running?") {
		t.Errorf("Apps() error = %v, want a hint that the daemon is not running", err)
	}
}

func TestClientExplainsMissingToken(t *testing.T) {
	h := servertest.New(t)
	anonymous := &api.Client{BaseURL: h.URL}

	_, err := anonymous.Apps(ctx())

	if err == nil || !strings.Contains(err.Error(), "limand token create") {
		t.Errorf("Apps() error = %v, want a hint about creating a token", err)
	}
}

// --- protection --------------------------------------------------------------

// request sends an authenticated request with extra headers and returns the
// response; the caller closes the body.
func request(t *testing.T, h *servertest.Harness, method, path string, headers map[string]string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, h.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.Client.Token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAppNameInPathIsValidated(t *testing.T) {
	h := servertest.New(t)
	proxy := h.WithProxy(t, "true")

	// A file next to the config directory that a traversal would reach.
	victim := filepath.Join(filepath.Dir(filepath.Dir(proxy.Path("x"))), "victim.conf")
	if err := os.WriteFile(victim, []byte("precious\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make the attack as easy as possible: a container that carries the
	// hostile name as its app label, so the "does this app exist" check
	// would not stop the request.
	h.Engine.Add(engine.Container{Name: "c1", App: "../victim"})

	hostile := []string{
		"../victim",
		"..%2Fvictim", // the same, percent-encoded
		"web;rm -rf",
		"WEB",
		"a b",
		strings.Repeat("a", 64),
	}

	for _, name := range hostile {
		for _, target := range []struct{ method, suffix string }{
			{http.MethodDelete, ""},
			{http.MethodGet, "/logs"},
		} {
			// PathEscape encodes the name so it stays one path segment and
			// the request really reaches the handler with that value.
			path := "/v1/apps/" + url.PathEscape(name) + target.suffix
			if strings.Contains(name, "%") {
				path = "/v1/apps/" + name + target.suffix
			}

			resp := request(t, h, target.method, path, nil)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s = %d, want it refused with 400 (or not routed, 404)", target.method, path, resp.StatusCode)
			}
		}
	}

	if got, err := os.ReadFile(victim); err != nil || string(got) != "precious\n" {
		t.Errorf("the file outside the config directory is now %q (error %v)", got, err)
	}
	if len(h.Engine.Calls) != 0 {
		t.Errorf("the engine was used for hostile names: %v", h.Engine.Calls)
	}
}

func TestHistoryRejectsInvalidAppFilter(t *testing.T) {
	h := servertest.New(t)

	resp := request(t, h, http.MethodGet, "/v1/history?app="+url.QueryEscape("x' OR '1'='1"), nil)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHostileDefinitionsAreRejected(t *testing.T) {
	h := servertest.New(t)
	proxy := h.WithProxy(t, "true")

	// Each of these tries to smuggle syntax into a place where it would be
	// interpreted: the Nginx config, a file path, a shell-looking command.
	tests := []struct {
		name string
		edit func(string) string
	}{
		{"nginx directive in domain", func(d string) string {
			return strings.Replace(d, "domain: api.example.com", `domain: "api.example.com; return 200 pwned"`, 1)
		}},
		{"newline in domain", func(d string) string {
			return strings.Replace(d, "domain: api.example.com", `domain: "api.example.com\n} server { listen 81"`, 1)
		}},
		{"path traversal in name", func(d string) string {
			return strings.Replace(d, "name: orders-api", `name: "../../etc/cron.d/x"`, 1)
		}},
		{"docker flags in image", func(d string) string {
			return strings.Replace(d, "image: ghcr.io/user/orders-api:1.4", `image: "ghcr.io/user/orders-api:1.4 --privileged"`, 1)
		}},
		{"header injection in health path", func(d string) string {
			return strings.Replace(d, "path: /health", `path: "/health\r\nX-Injected: 1"`, 1)
		}},
		{"yaml that expands into a huge document", func(string) string {
			// Aliases that reference aliases (a "billion laughs" document).
			return "a: &a [x,x,x,x,x,x,x,x,x]\nb: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]\nc: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]\nd: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]\ne: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d]\nname: *e\n"
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.Client.Deploy(ctx(), []byte(tt.edit(definition)), api.DeployOptions{}, io.Discard)

			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
				t.Fatalf("Deploy() error = %v, want a 400", err)
			}
		})
	}

	if len(h.Engine.Calls) != 0 {
		t.Errorf("the engine was used for a hostile definition: %v", h.Engine.Calls)
	}
	if entries, _ := os.ReadDir(filepath.Dir(proxy.Path("x"))); len(entries) != 0 {
		t.Errorf("a proxy config was written for a hostile definition: %v", entries)
	}
}

func TestRequestsFromWebPagesAreRefused(t *testing.T) {
	h := servertest.New(t)

	// What a browser sends when a page on another site calls the API.
	for _, path := range []string{"/v1/apps", "/v1/health"} {
		resp := request(t, h, http.MethodGet, path, map[string]string{"Origin": "https://evil.example"})
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s with an Origin header = %d, want 403", path, resp.StatusCode)
		}
		// No permission for the page to read the answer either.
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want it absent", got)
		}
	}
}

func TestResponsesCarryProtectiveHeaders(t *testing.T) {
	h := servertest.New(t)

	// On a success, on an error and on a rejected request alike.
	for _, path := range []string{"/v1/apps", "/v1/apps/nope/logs", "/v1/does-not-exist"} {
		resp := request(t, h, http.MethodGet, path, nil)
		_ = resp.Body.Close()

		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s: Cache-Control = %q, want no-store", path, got)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s: X-Content-Type-Options = %q, want nosniff", path, got)
		}
	}
}

func TestRepeatedBadTokensLockTheClientOut(t *testing.T) {
	h := servertest.New(t)
	guess := func(token string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, h.URL+"/v1/apps", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}

	// Ten wrong guesses are answered normally...
	for i := range 10 {
		if resp := guess(fmt.Sprintf("liman_%064d", i)); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d, want 401", i+1, resp.StatusCode)
		}
	}

	// ...the eleventh is not even examined.
	resp := guess("liman_" + strings.Repeat("0", 64))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("guess 11 = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 429 response must say when to retry")
	}

	// The lockout is for those who fail. Whoever holds a valid token is
	// not affected by it: otherwise anybody able to send ten bad requests
	// from the operator's address (a web page in the operator's browser
	// can) could keep the operator out for as long as they liked. That a
	// right guess is answered differently gives nothing away: a right
	// guess is a valid token.
	if resp := guess(h.Client.Token); resp.StatusCode != http.StatusOK {
		t.Errorf("the valid token during a lockout = %d, want 200", resp.StatusCode)
	}

	// The health check stays reachable: monitoring must not be collateral.
	healthResp, err := http.Get(h.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Errorf("health during a lockout = %d, want 200", healthResp.StatusCode)
	}
}

func TestSuccessfulRequestsAreNeverLockedOut(t *testing.T) {
	h := servertest.New(t)

	for i := range 50 {
		if _, err := h.Client.Apps(ctx()); err != nil {
			t.Fatalf("request %d with a valid token failed: %v", i+1, err)
		}
	}
}

func TestTooManyConcurrentRequestsAreShed(t *testing.T) {
	h := servertest.New(t, func(s *server.Server) { s.MaxInFlight = 2 })

	// Occupy both slots with step-mode deploys parked at their first
	// question.
	parkCtx, release := context.WithCancel(ctx())
	defer release()

	var parked sync.WaitGroup
	asked := make(chan struct{}, 2)
	for _, name := range []string{"app-one", "app-two"} {
		parked.Add(1)
		go func() {
			defer parked.Done()
			def := strings.Replace(definition, "name: orders-api", "name: "+name, 1)
			opts := api.DeployOptions{Confirm: func(string) (bool, error) {
				asked <- struct{}{}
				<-parkCtx.Done()
				return false, parkCtx.Err()
			}}
			_, _ = h.Client.Deploy(parkCtx, []byte(def), opts, io.Discard)
		}()
	}
	for range 2 {
		select {
		case <-asked:
		case <-time.After(5 * time.Second):
			t.Fatal("the parked deploys did not reach their first question")
		}
	}

	// A third request finds no free slot.
	resp, err := http.Get(h.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Errorf("request beyond the limit = %d (Retry-After %q), want 503 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}

	// Once the slots are free again, requests are served.
	release()
	parked.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(h.URL + "/v1/health")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %d after the parked requests ended", resp.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSlowRequestBodyIsCutOff(t *testing.T) {
	h := servertest.New(t, func(s *server.Server) { s.BodyReadTimeout = 100 * time.Millisecond })

	// Speak HTTP by hand: announce a 1000-byte body, send a few bytes and
	// then nothing, as a client trying to hold the connection open would.
	conn, err := net.Dial("tcp", strings.TrimPrefix(h.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	_, err = fmt.Fprintf(conn, "POST /v1/deploys HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\n\r\nname: sl", h.Client.Token)
	if err != nil {
		t.Fatal(err)
	}

	// The server must answer on its own, without waiting for the rest.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply := make([]byte, 64)
	n, err := conn.Read(reply)
	if err != nil {
		t.Fatalf("the server did not answer the stalled request: %v", err)
	}
	if got := string(reply[:n]); !strings.HasPrefix(got, "HTTP/1.1 408") {
		t.Errorf("answer starts with %q, want a 408 Request Timeout", got)
	}
	if len(h.Engine.Calls) != 0 {
		t.Errorf("the engine was used for a request that never arrived in full: %v", h.Engine.Calls)
	}
}

func TestBodyDeadlineDoesNotCutOffALongDeploy(t *testing.T) {
	// The deadline for reading the body is far shorter than the deploy.
	h := servertest.New(t, func(s *server.Server) { s.BodyReadTimeout = 50 * time.Millisecond })

	opts := api.DeployOptions{Confirm: func(string) (bool, error) {
		// Thinking time well beyond the body deadline, at every step.
		time.Sleep(120 * time.Millisecond)
		return true, nil
	}}

	res, err := h.Client.Deploy(ctx(), []byte(definition), opts, io.Discard)

	// If the read deadline were still armed after the body was read, the
	// server would cancel the request and the deploy would roll back.
	if err != nil || !res.OK {
		t.Fatalf("Deploy() = %+v, %v; want the long deploy to complete", res, err)
	}
}

func TestConfirmRejectsUnknownFields(t *testing.T) {
	h := servertest.New(t)

	req, _ := http.NewRequest(http.MethodPost, h.URL+"/v1/confirmations/abc", strings.NewReader(`{"approve":true,"force":true}`))
	req.Header.Set("Authorization", "Bearer "+h.Client.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOversizedHeadersAreRefused(t *testing.T) {
	h := servertest.New(t)

	// Go's test server uses the default header limit (1 MiB); the daemon
	// lowers it. This test documents the default the daemon overrides.
	resp := request(t, h, http.MethodGet, "/v1/apps", map[string]string{"X-Padding": strings.Repeat("a", 2<<20)})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("status = %d, want 431", resp.StatusCode)
	}
}

func TestAppIsUnlockedBeforeTheResultIsSent(t *testing.T) {
	h := servertest.New(t)

	// Deploy the same app back to back, each request sent the moment the
	// previous one reported its result. If the daemon announced the result
	// while still holding the app's lock, some of these would be refused.
	// (The load test found this at a rate of about 1 in 5000.)
	for i := range 3000 {
		_, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard)
		if err != nil {
			t.Fatalf("deploy %d directly after the previous one: %v", i+1, err)
		}
	}

	// The same holds for stop followed by a deploy.
	for i := range 300 {
		if _, err := h.Client.Stop(ctx(), "orders-api"); err != nil {
			t.Fatalf("stop %d: %v", i+1, err)
		}
		if _, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard); err != nil {
			t.Fatalf("deploy %d directly after a stop: %v", i+1, err)
		}
	}
}

// --- rollback ---------------------------------------------------------------

// deployTwice leaves r1 stopped and kept, and r2 in service.
func deployTwice(t *testing.T, h *servertest.Harness) {
	t.Helper()
	for i := range 2 {
		if _, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard); err != nil {
			t.Fatalf("deploy %d: %v", i+1, err)
		}
	}
}

func TestRollback(t *testing.T) {
	h := servertest.New(t)
	proxy := h.WithProxy(t, "true")
	deployTwice(t, h)
	var out bytes.Buffer

	res, err := h.Client.Rollback(ctx(), "orders-api", api.DeployOptions{}, &out)
	if err != nil {
		t.Fatalf("Rollback() error = %v\n%s", err, out.String())
	}

	if !res.OK || res.Release != "r1" || res.Container != "orders-api-r1" || !res.Routed {
		t.Errorf("result = %+v, want release r1 routed", res)
	}
	if got := h.Engine.Running(); len(got) != 1 || got[0] != "orders-api-r1" {
		t.Errorf("running = %v, want only orders-api-r1", got)
	}
	conf, _ := os.ReadFile(proxy.Path("orders-api"))
	if !strings.Contains(string(conf), `X-Liman-Release "r1"`) {
		t.Errorf("proxy config does not report release r1:\n%s", conf)
	}
	if !strings.Contains(out.String(), "==> [1/5] Start previous container orders-api-r1") {
		t.Errorf("output does not show the steps:\n%s", out.String())
	}

	// The steps of the rollback are in the history.
	events, _ := h.Client.History(ctx(), "orders-api", 5)
	if len(events) != 5 || events[0].Step != "Start previous container orders-api-r1" || events[0].Release != "r1" {
		t.Errorf("last five history entries = %+v, want the rollback's steps for r1", events)
	}

	// The listing shows a single, current container again.
	apps, _ := h.Client.Apps(ctx())
	if len(apps) != 1 || apps[0].Role != "current" || apps[0].Container != "orders-api-r1" {
		t.Errorf("Apps() = %+v, want only orders-api-r1 as current", apps)
	}
}

func TestRollbackIsRefusedBeforeItStarts(t *testing.T) {
	h := servertest.New(t)
	h.Engine.Add(engine.Container{Name: "single-r1", App: "single"})

	tests := []struct {
		name       string
		app        string
		wantStatus int
		wantErr    string
	}{
		{"unknown app", "nope", http.StatusNotFound, "container not found"},
		{"only one release", "single", http.StatusConflict, "no previous release is kept"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.Client.Rollback(ctx(), tt.app, api.DeployOptions{}, io.Discard)

			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Status != tt.wantStatus || !strings.Contains(apiErr.Message, tt.wantErr) {
				t.Errorf("Rollback() error = %v, want status %d containing %q", err, tt.wantStatus, tt.wantErr)
			}
		})
	}

	// A hostile name never reaches the engine.
	resp := request(t, h, http.MethodPost, "/v1/apps/..%2Fx/rollback", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
		t.Errorf("rollback with a traversal name = %d, want it refused", resp.StatusCode)
	}

	// None of the refusals may leave the app locked.
	if _, err := h.Client.Stop(ctx(), "single"); err != nil {
		t.Errorf("Stop() after a refused rollback error = %v", err)
	}
}

func TestRollbackDryRunAndListingRoles(t *testing.T) {
	h := servertest.New(t)
	deployTwice(t, h)

	apps, err := h.Client.Apps(ctx())
	if err != nil || len(apps) != 2 {
		t.Fatalf("Apps() = %+v, %v; want two containers", apps, err)
	}
	if apps[0].Container != "orders-api-r1" || apps[0].Role != "previous" || apps[0].State != "exited" {
		t.Errorf("first entry = %+v, want the stopped previous release", apps[0])
	}
	if apps[1].Container != "orders-api-r2" || apps[1].Role != "current" {
		t.Errorf("second entry = %+v, want the current release", apps[1])
	}

	var out bytes.Buffer
	res, err := h.Client.Rollback(ctx(), "orders-api", api.DeployOptions{DryRun: true}, &out)
	if err != nil || !res.DryRun {
		t.Fatalf("Rollback(dry run) = %+v, %v", res, err)
	}
	if got := h.Engine.Running(); len(got) != 1 || got[0] != "orders-api-r2" {
		t.Errorf("running after a dry run = %v, want nothing changed", got)
	}
	events, _ := h.Client.History(ctx(), "orders-api", 50)
	if len(events) != 10 {
		t.Errorf("history has %d entries, want only the 10 of the two deploys", len(events))
	}
}

func TestRollbackAndDeployExcludeEachOther(t *testing.T) {
	h := servertest.New(t)
	deployTwice(t, h)

	// A step-mode rollback parked at its first question holds the app.
	asked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		opts := api.DeployOptions{Confirm: func(string) (bool, error) {
			once.Do(func() { close(asked) })
			<-release
			return false, nil
		}}
		_, err := h.Client.Rollback(ctx(), "orders-api", opts, io.Discard)
		done <- err
	}()

	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the rollback never asked its first question")
	}

	_, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Errorf("Deploy() during a rollback error = %v, want a 409", err)
	}

	close(release)
	if err := <-done; err == nil {
		t.Error("the declined rollback reported success")
	}
}

// --- secrets -----------------------------------------------------------------

const definitionWithSecrets = definition + `env:
  LOG_LEVEL: "info"
secrets:
  - DATABASE_URL
  - API_KEY
`

func TestSecretsAreWriteOnly(t *testing.T) {
	h := servertest.New(t)

	if err := h.Client.SetSecret(ctx(), "orders-api", "DATABASE_URL", []byte("postgres://user:hunter2@db/app")); err != nil {
		t.Fatalf("SetSecret() error = %v", err)
	}
	if err := h.Client.SetSecret(ctx(), "orders-api", "API_KEY", []byte("k-123")); err != nil {
		t.Fatalf("SetSecret() error = %v", err)
	}

	list, err := h.Client.Secrets(ctx(), "orders-api")
	if err != nil || len(list) != 2 || list[0].Name != "API_KEY" || list[1].Name != "DATABASE_URL" || list[0].UpdatedAt.IsZero() {
		t.Fatalf("Secrets() = %+v, %v; want the two names with dates", list, err)
	}

	// No response of the API may ever contain a value. Read the raw
	// bodies of every endpoint that mentions secrets.
	for _, path := range []string{"/v1/apps/orders-api/secrets", "/v1/apps", "/v1/history"} {
		resp := request(t, h, http.MethodGet, path, nil)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		for _, value := range []string{"hunter2", "k-123"} {
			if strings.Contains(string(body), value) {
				t.Errorf("GET %s returned a secret value", path)
			}
		}
	}
	// And there is no way to ask for one.
	resp := request(t, h, http.MethodGet, "/v1/apps/orders-api/secrets/API_KEY", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET of a single secret = %d, want 405 (there is no read endpoint)", resp.StatusCode)
	}

	// Secrets of another app are not listed.
	if list, _ := h.Client.Secrets(ctx(), "another"); len(list) != 0 {
		t.Errorf("Secrets() of another app = %+v, want none", list)
	}

	if err := h.Client.DeleteSecret(ctx(), "orders-api", "API_KEY"); err != nil {
		t.Fatalf("DeleteSecret() error = %v", err)
	}
	var apiErr *api.Error
	if err := h.Client.DeleteSecret(ctx(), "orders-api", "API_KEY"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Errorf("DeleteSecret() twice error = %v, want a 404", err)
	}
}

func TestSecretsAreEncryptedInTheDatabase(t *testing.T) {
	h := servertest.New(t)
	const value = "very-recognisable-plaintext-0123456789"

	if err := h.Client.SetSecret(ctx(), "orders-api", "API_KEY", []byte(value)); err != nil {
		t.Fatal(err)
	}

	// What the store holds is not the value...
	sealed, found, err := h.Store.GetSecret(ctx(), "orders-api", "API_KEY")
	if err != nil || !found {
		t.Fatalf("GetSecret() = found %v, %v", found, err)
	}
	if strings.Contains(string(sealed), value) {
		t.Error("the database row contains the plaintext")
	}
	// ...and it is longer than the value: format byte, nonce and tag.
	if len(sealed) != len(value)+1+12+16 {
		t.Errorf("sealed length = %d, want %d (value + 29 bytes of overhead)", len(sealed), len(value)+29)
	}
}

func TestSecretInputIsValidated(t *testing.T) {
	h := servertest.New(t)

	tests := []struct {
		name       string
		app        string
		key        string
		value      string
		wantStatus int
	}{
		{"invalid app name", "Orders_API", "KEY", "v", http.StatusBadRequest},
		{"invalid secret name", "orders-api", "MY-KEY", "v", http.StatusBadRequest},
		{"empty value", "orders-api", "KEY", "", http.StatusBadRequest},
		{"value with NUL", "orders-api", "KEY", "a\x00b", http.StatusBadRequest},
		{"value too large", "orders-api", "KEY", strings.Repeat("x", 64<<10+1), http.StatusBadRequest},
		{"value far too large", "orders-api", "KEY", strings.Repeat("x", 1<<20), http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.Client.SetSecret(ctx(), tt.app, tt.key, []byte(tt.value))

			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Status != tt.wantStatus {
				t.Errorf("SetSecret() error = %v, want status %d", err, tt.wantStatus)
			}
		})
	}
	if n, _ := h.Store.CountSecrets(ctx()); n != 0 {
		t.Errorf("%d secrets were stored from rejected requests", n)
	}

	// A value of exactly the limit is accepted.
	if err := h.Client.SetSecret(ctx(), "orders-api", "BIG", []byte(strings.Repeat("x", 64<<10))); err != nil {
		t.Errorf("SetSecret() at the size limit error = %v", err)
	}
}

func TestSecretsNeedAToken(t *testing.T) {
	h := servertest.New(t)
	anonymous := &api.Client{BaseURL: h.URL}

	var apiErr *api.Error
	if err := anonymous.SetSecret(ctx(), "orders-api", "KEY", []byte("v")); !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Errorf("SetSecret() without a token error = %v, want a 401", err)
	}
	if _, err := anonymous.Secrets(ctx(), "orders-api"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Errorf("Secrets() without a token error = %v, want a 401", err)
	}
}

func TestDeployInjectsSecretsIntoTheEnvironment(t *testing.T) {
	h := servertest.New(t)
	if err := h.Client.SetSecret(ctx(), "orders-api", "DATABASE_URL", []byte("postgres://user:hunter2@db/app")); err != nil {
		t.Fatal(err)
	}
	if err := h.Client.SetSecret(ctx(), "orders-api", "API_KEY", []byte("k-123")); err != nil {
		t.Fatal(err)
	}
	// A secret the definition does not refer to must not be passed on.
	if err := h.Client.SetSecret(ctx(), "orders-api", "UNUSED", []byte("not-for-this-deploy")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	if _, err := h.Client.Deploy(ctx(), []byte(definitionWithSecrets), api.DeployOptions{}, &out); err != nil {
		t.Fatalf("Deploy() error = %v\n%s", err, out.String())
	}

	env := h.Engine.Specs[0].Env
	want := map[string]string{"LOG_LEVEL": "info", "DATABASE_URL": "postgres://user:hunter2@db/app", "API_KEY": "k-123"}
	if len(env) != len(want) {
		t.Errorf("container environment has %d variables, want %d: %v", len(env), len(want), keys(env))
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("container environment %s is wrong", k)
		}
	}

	// The values reach the container and nothing else: not the progress
	// output, not the history, not the release record.
	events, _ := h.Client.History(ctx(), "", 50)
	releases, _ := h.Store.Releases(ctx(), "orders-api", 10)
	recorded := fmt.Sprint(out.String(), events, releases)
	for _, value := range []string{"hunter2", "k-123"} {
		if strings.Contains(recorded, value) {
			t.Errorf("a secret value appears in the output, the history or the release record")
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestDeployWithMissingSecretsIsRefusedBeforeAnythingHappens(t *testing.T) {
	h := servertest.New(t)
	// Only one of the two secrets the definition refers to is set.
	if err := h.Client.SetSecret(ctx(), "orders-api", "API_KEY", []byte("k-123")); err != nil {
		t.Fatal(err)
	}

	for _, dryRun := range []bool{false, true} {
		_, err := h.Client.Deploy(ctx(), []byte(definitionWithSecrets), api.DeployOptions{DryRun: dryRun}, io.Discard)

		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
			t.Fatalf("Deploy(dryRun=%v) error = %v, want a 400", dryRun, err)
		}
		for _, want := range []string{"secrets that are not set: DATABASE_URL", "liman secret set orders-api <NAME>"} {
			if !strings.Contains(apiErr.Message, want) {
				t.Errorf("error %q does not contain %q", apiErr.Message, want)
			}
		}
	}

	if len(h.Engine.Calls) != 0 {
		t.Errorf("the engine was used although secrets were missing: %v", h.Engine.Calls)
	}
	// The refusal did not leave the app locked.
	if err := h.Client.SetSecret(ctx(), "orders-api", "DATABASE_URL", []byte("postgres://x")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Client.Deploy(ctx(), []byte(definitionWithSecrets), api.DeployOptions{}, io.Discard); err != nil {
		t.Errorf("Deploy() once the secret is set error = %v", err)
	}
}

func TestDryRunShowsSecretNamesMaskedAndDecryptsNothing(t *testing.T) {
	h := servertest.New(t)
	_ = h.Client.SetSecret(ctx(), "orders-api", "DATABASE_URL", []byte("postgres://user:hunter2@db/app"))
	_ = h.Client.SetSecret(ctx(), "orders-api", "API_KEY", []byte("k-123"))
	var out bytes.Buffer

	if _, err := h.Client.Deploy(ctx(), []byte(definitionWithSecrets), api.DeployOptions{DryRun: true}, &out); err != nil {
		t.Fatalf("Deploy(dry run) error = %v", err)
	}

	if want := "-e API_KEY=*** -e DATABASE_URL=*** -e LOG_LEVEL=***"; !strings.Contains(out.String(), want) {
		t.Errorf("dry run does not show the masked variables %q:\n%s", want, out.String())
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Error("the dry run output contains a secret value")
	}
}

func TestChangedSecretAppliesOnNextDeployOnly(t *testing.T) {
	h := servertest.New(t)
	_ = h.Client.SetSecret(ctx(), "orders-api", "DATABASE_URL", []byte("old-url"))
	_ = h.Client.SetSecret(ctx(), "orders-api", "API_KEY", []byte("k-1"))
	if _, err := h.Client.Deploy(ctx(), []byte(definitionWithSecrets), api.DeployOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}

	// Changing the secret does not touch the running container...
	_ = h.Client.SetSecret(ctx(), "orders-api", "DATABASE_URL", []byte("new-url"))
	if got := h.Engine.Specs[0].Env["DATABASE_URL"]; got != "old-url" {
		t.Errorf("the running container's value changed to %q", got)
	}

	// ...the next deploy picks it up.
	if _, err := h.Client.Deploy(ctx(), []byte(definitionWithSecrets), api.DeployOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := h.Engine.Specs[1].Env["DATABASE_URL"]; got != "new-url" {
		t.Errorf("the new container has %q, want the new value", got)
	}
}

// --- panel and the read-only views behind it ---------------------------------

// plain sends a request without a token and without following redirects.
func plain(t *testing.T, h *servertest.Harness, path string) *http.Response {
	t.Helper()

	client := &http.Client{
		// The default client follows redirects; returning this error makes
		// it hand back the redirect itself instead.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(h.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func TestPanelIsServedWithoutATokenAndTheAPIStillNeedsOne(t *testing.T) {
	h := servertest.New(t, func(s *server.Server) { s.Panel = panel.Handler() })

	if resp := plain(t, h, "/ui/"); resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("GET /ui/ = %d (policy %q), want 200 with a content security policy", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	if resp := plain(t, h, "/ui/app.js"); resp.StatusCode != http.StatusOK || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("GET /ui/app.js = %d, want 200 with the protective headers", resp.StatusCode)
	}
	if resp := plain(t, h, "/"); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
		t.Errorf("GET / = %d to %q, want a redirect to /ui/", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Everything else is exactly as protected as before.
	for _, path := range []string{"/v1/apps", "/v1/info", "/v1/releases", "/v1/apps/web/health", "/nope", "/uiapp.js"} {
		if resp := plain(t, h, path); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestLoadingThePanelNeverLocksAClientOut(t *testing.T) {
	h := servertest.New(t, func(s *server.Server) { s.Panel = panel.Handler() })

	// Far more requests than the failed-authentication limit allows.
	for range 30 {
		if resp := plain(t, h, "/ui/"); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /ui/ = %d, want 200", resp.StatusCode)
		}
	}
	if _, err := h.Client.Apps(ctx()); err != nil {
		t.Errorf("Apps() after loading the panel many times error = %v", err)
	}
}

func TestWithoutAPanelItsPathIsNothingSpecial(t *testing.T) {
	h := servertest.New(t)

	if resp := plain(t, h, "/ui/"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /ui/ on a server without a panel = %d, want 401 like any unknown path", resp.StatusCode)
	}
}

func TestInfo(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	h := servertest.New(t, func(s *server.Server) {
		s.Started = started
		s.Monitoring = &api.MonitorInfo{IntervalSeconds: 10, FailureThreshold: 3, RetentionSeconds: 604800}
	})

	info, err := h.Client.Info(ctx())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if !info.StartedAt.Equal(started) || info.Now.Before(started) {
		t.Errorf("Info() times = started %s, now %s", info.StartedAt, info.Now)
	}
	if info.Proxy || !info.Secrets || info.Monitor == nil || info.Monitor.IntervalSeconds != 10 {
		t.Errorf("Info() = %+v (monitor %+v), want no proxy, secrets and the monitor's settings", info, info.Monitor)
	}
	if info.Version == "" {
		t.Error("Info() reports no version")
	}

	// Unlike /v1/health this says how the server is set up, so it is not
	// for everybody.
	if resp := plain(t, h, "/v1/info"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /v1/info without a token = %d, want 401", resp.StatusCode)
	}
}

func TestReleasesListing(t *testing.T) {
	h := servertest.New(t)

	if got, err := h.Client.Releases(ctx(), "", 10); err != nil || got == nil || len(got) != 0 {
		t.Errorf("Releases() with no deploys = %v, %v; want an empty, non-nil list", got, err)
	}

	for range 2 {
		if _, err := h.Client.Deploy(ctx(), []byte(definition), api.DeployOptions{}, io.Discard); err != nil {
			t.Fatalf("Deploy() error = %v", err)
		}
	}
	other := strings.Replace(definition, "orders-api", "web", 2)
	h.HealthErr = errors.New("503")
	// A failed deploy comes back as a result and as an error.
	if res, err := h.Client.Deploy(ctx(), []byte(other), api.DeployOptions{}, io.Discard); err == nil || res == nil || res.OK {
		t.Fatalf("Deploy() of the failing app = %+v, %v; want a failed deploy", res, err)
	}

	all, err := h.Client.Releases(ctx(), "", 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("Releases() = %+v, %v; want three", all, err)
	}
	// Newest first: the failed deploy of the other app.
	if all[0].App != "web" || all[0].Status != store.StatusFailed || all[0].Error == "" || all[0].FinishedAt.Before(all[0].StartedAt) {
		t.Errorf("newest release = %+v, want the failed deploy of web", all[0])
	}

	mine, err := h.Client.Releases(ctx(), "orders-api", 1)
	if err != nil || len(mine) != 1 || mine[0].Release != "r2" || mine[0].Status != store.StatusSucceeded {
		t.Errorf("Releases(orders-api, 1) = %+v, %v; want only r2", mine, err)
	}

	for _, query := range []string{"?limit=0", "?limit=abc", "?limit=100000", "?app=../etc"} {
		resp := request(t, h, http.MethodGet, "/v1/releases"+query, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /v1/releases%s = %d, want 400", query, resp.StatusCode)
		}
	}
}

func TestHealthHistory(t *testing.T) {
	h := servertest.New(t)
	now := time.Now()

	samples := []store.HealthSample{
		{Time: now, App: "web", OK: true, Probed: true, Duration: 4 * time.Millisecond},
		{Time: now, App: "web", OK: false, Probed: true, Duration: 8 * time.Millisecond},
		// Older than the six hours asked for below, and another app.
		{Time: now.Add(-7 * time.Hour), App: "web", OK: false},
		{Time: now, App: "api", OK: false},
	}
	for _, sample := range samples {
		if err := h.Store.AppendHealthSample(ctx(), sample); err != nil {
			t.Fatal(err)
		}
	}

	got, err := h.Client.HealthHistory(ctx(), "web", "6h")
	if err != nil {
		t.Fatalf("HealthHistory() error = %v", err)
	}
	if got.App != "web" || got.BucketSeconds != 450 {
		t.Errorf("HealthHistory() = app %q, %ds buckets; want web and 450s", got.App, got.BucketSeconds)
	}
	// 48 buckets, the last of which contains this moment.
	if span := got.Until.Sub(got.Since); span != 6*time.Hour || now.Before(got.Since) || !now.Before(got.Until) {
		t.Errorf("HealthHistory() covers %s to %s, want six hours around now", got.Since, got.Until)
	}
	if len(got.Buckets) != 1 {
		t.Fatalf("HealthHistory() buckets = %+v, want one", got.Buckets)
	}
	b := got.Buckets[0]
	if b.OK != 1 || b.Failed != 1 || b.Probed != 2 || b.AvgMS != 6 || b.MaxMS != 8 {
		t.Errorf("bucket = %+v, want 1 ok, 1 failed, average 6 ms, slowest 8 ms", b)
	}
	if b.Start.Before(got.Since) || !b.Start.Before(got.Until) || b.Start.Sub(got.Since)%(450*time.Second) != 0 {
		t.Errorf("bucket starts at %s, want a multiple of the width after %s", b.Start, got.Since)
	}

	// An app nobody has checked: an empty list, not an error.
	none, err := h.Client.HealthHistory(ctx(), "unknown", "7d")
	if err != nil || none.Buckets == nil || len(none.Buckets) != 0 || none.BucketSeconds != 12600 {
		t.Errorf("HealthHistory() of an unknown app = %+v, %v; want no buckets", none, err)
	}

	for path, want := range map[string]int{
		"/v1/apps/web/health":            http.StatusOK, // the range has a default
		"/v1/apps/web/health?range=1s":   http.StatusBadRequest,
		"/v1/apps/web/health?range=900d": http.StatusBadRequest,
		"/v1/apps/WEB/health?range=6h":   http.StatusBadRequest,
	} {
		resp := request(t, h, http.MethodGet, path, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}

// --- backups and the panel's write access ---------------------------------------

func TestBackupsOverTheAPI(t *testing.T) {
	h := servertest.New(t)

	list, err := h.Client.Backups(ctx())
	if err != nil || list.Backups == nil || len(list.Backups) != 0 || list.Dir != h.BackupDir {
		t.Fatalf("Backups() before any = %+v, %v; want an empty, non-nil list in %s", list, err, h.BackupDir)
	}

	if err := h.Client.SetSecret(ctx(), "web", "API_KEY", []byte("s3cr3t-value")); err != nil {
		t.Fatal(err)
	}

	res, err := h.Client.CreateBackup(ctx())
	if err != nil {
		t.Fatalf("CreateBackup() error = %v", err)
	}
	if !res.Report.OK || res.Report.Signature != "valid" || res.Report.Secrets != 1 || !res.Report.KeyChecked || res.Report.Rows["tokens"] != 1 {
		t.Errorf("CreateBackup() report = %+v, want ok with one secret and one token", res.Report)
	}
	if res.Backup.SizeBytes == 0 || time.Since(res.Backup.CreatedAt) > time.Minute {
		t.Errorf("CreateBackup() backup = %+v, want a size and a recent time", res.Backup)
	}
	// The file is where the listing says, private, and holds no plain text.
	data, err := os.ReadFile(filepath.Join(h.BackupDir, res.Backup.Name))
	if err != nil || bytes.Contains(data, []byte("s3cr3t-value")) {
		t.Errorf("backup file: %v; it must exist and not contain the secret's value", err)
	}

	list, err = h.Client.Backups(ctx())
	if err != nil || len(list.Backups) != 1 || list.Backups[0].Name != res.Backup.Name {
		t.Errorf("Backups() = %+v, %v; want the new backup", list, err)
	}

	rep, err := h.Client.VerifyBackup(ctx(), res.Backup.Name)
	if err != nil || !rep.OK {
		t.Errorf("VerifyBackup() = %+v, %v; want ok", rep, err)
	}

	// With another master key the backup is intact but not usable.
	if err := os.Remove(h.KeyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.CreateKey(h.KeyPath); err != nil {
		t.Fatal(err)
	}
	rep, err = h.Client.VerifyBackup(ctx(), res.Backup.Name)
	if err != nil || rep.OK || rep.Signature != "invalid" {
		t.Errorf("VerifyBackup() with another key = %+v, %v; want not ok with an invalid signature", rep, err)
	}
}

func TestBackupNamesFromRequestsCannotLeaveTheDirectory(t *testing.T) {
	h := servertest.New(t)

	// A real database next to the backup directory, which a traversal
	// would reach.
	if err := h.Store.Snapshot(ctx(), filepath.Join(filepath.Dir(h.BackupDir), "outside.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.BackupDir, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"..%2Foutside.db", "outside.db", "liman-20260101-000000.db", "liman-20260101-000000.db%00", "%2e%2e%2f%2e%2e%2fetc%2fpasswd"} {
		resp := request(t, h, http.MethodPost, "/v1/backups/"+name+"/verify", nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("verify of %q = %d, want 404 or 400", name, resp.StatusCode)
		}
	}
}

func TestBackupEndpointsNeedATokenAndTheRightMethod(t *testing.T) {
	h := servertest.New(t)

	if resp := plain(t, h, "/v1/backups"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /v1/backups without a token = %d, want 401", resp.StatusCode)
	}
	resp, err := http.Post(h.URL+"/v1/backups", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /v1/backups without a token = %d, want 401", resp.StatusCode)
	}
	if entries, _ := os.ReadDir(h.BackupDir); len(entries) != 0 {
		t.Errorf("a backup was made without a token: %v", entries)
	}

	// There is no way to remove or fetch a backup through the API.
	for _, method := range []string{http.MethodDelete, http.MethodPut} {
		resp := request(t, h, method, "/v1/backups", nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/backups = %d, want 405", method, resp.StatusCode)
		}
	}
}

func TestOnlyThePanelsOwnPagesMaySendRequestsFromABrowser(t *testing.T) {
	h := servertest.New(t, func(s *server.Server) { s.Panel = panel.Handler() })
	host := strings.TrimPrefix(h.URL, "http://")

	post := func(origin, hostHeader string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.URL+"/v1/backups", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+h.Client.Token)
		req.Header.Set("Origin", origin)
		if hostHeader != "" {
			// What the server sees as the request's Host header.
			req.Host = hostHeader
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// The panel: its Origin is the daemon's own address.
	if got := post("http://"+host, ""); got != http.StatusCreated {
		t.Errorf("POST from the panel's own origin = %d, want 201", got)
	}
	// Another site, and the same address with another scheme or port.
	_, port, _ := net.SplitHostPort(host)
	for _, origin := range []string{"https://evil.example", "https://" + host, "http://127.0.0.1:1", "null"} {
		if got := post(origin, ""); got != http.StatusForbidden {
			t.Errorf("POST with Origin %q = %d, want 403", origin, got)
		}
	}
	// DNS rebinding: a hostile name that resolves to the daemon. Origin and
	// Host agree, but the daemon is never addressed by such a name.
	if got := post("http://evil.example:"+port, "evil.example:"+port); got != http.StatusForbidden {
		t.Errorf("POST from a rebound host name = %d, want 403", got)
	}
	// localhost is a name the daemon is addressed by.
	if got := post("http://localhost:"+port, "localhost:"+port); got != http.StatusCreated && got != http.StatusConflict {
		t.Errorf("POST addressed as localhost = %d, want it accepted", got)
	}

	// A daemon without a panel has no browser client at all.
	bare := servertest.New(t)
	req, _ := http.NewRequest(http.MethodPost, bare.URL+"/v1/backups", nil)
	req.Header.Set("Authorization", "Bearer "+bare.Client.Token)
	req.Header.Set("Origin", bare.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("same-origin POST to a daemon without a panel = %d, want 403", resp.StatusCode)
	}
}

// --- token scopes and the audit trail ---------------------------------------------

func TestReadOnlyTokenCanLookAndChangeNothing(t *testing.T) {
	h := servertest.New(t)
	h.Engine.Add(engine.Container{Name: "web-r1", App: "web", Image: "web:1.0"})
	if err := h.Client.SetSecret(ctx(), "web", "KEY", []byte("value")); err != nil {
		t.Fatal(err)
	}

	token, err := h.Store.CreateScopedToken(ctx(), "wallboard", store.ScopeRead, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string) int {
		t.Helper()
		req, err := http.NewRequest(method, h.URL+path, strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	for _, path := range []string{"/v1/apps", "/v1/history", "/v1/releases", "/v1/info", "/v1/backups", "/v1/apps/web/secrets", "/v1/apps/web/health", "/v1/apps/web/logs"} {
		if got := do(http.MethodGet, path); got != http.StatusOK {
			t.Errorf("GET %s with a read-only token = %d, want 200", path, got)
		}
	}

	// Everything that changes something, including a path that does not
	// exist: the rule is about the method.
	changes := []struct{ method, path string }{
		{http.MethodPost, "/v1/deploys"},
		{http.MethodPost, "/v1/deploys?dry_run=true"},
		{http.MethodPost, "/v1/apps/web/rollback"},
		{http.MethodDelete, "/v1/apps/web"},
		{http.MethodPut, "/v1/apps/web/secrets/KEY"},
		{http.MethodDelete, "/v1/apps/web/secrets/KEY"},
		{http.MethodPost, "/v1/backups"},
		{http.MethodPost, "/v1/confirmations/abc"},
		{http.MethodPost, "/v1/something-added-later"},
	}
	for _, c := range changes {
		if got := do(c.method, c.path); got != http.StatusForbidden {
			t.Errorf("%s %s with a read-only token = %d, want 403", c.method, c.path, got)
		}
	}

	// And nothing did change.
	if h.Engine.State("web-r1") != "running" {
		t.Error("the app was touched by a read-only token")
	}
	if list, err := h.Client.Secrets(ctx(), "web"); err != nil || len(list) != 1 {
		t.Errorf("secrets after the attempts = %+v, %v; want the one that was there", list, err)
	}
	if entries, _ := os.ReadDir(h.BackupDir); len(entries) != 0 {
		t.Errorf("a backup was made by a read-only token: %v", entries)
	}

	// The token is told what it is.
	info, err := (&api.Client{BaseURL: h.URL, Token: token}).Info(ctx())
	if err != nil || info.Scope != store.ScopeRead || info.Token != "wallboard" {
		t.Errorf("Info() with the read-only token = %+v, %v", info, err)
	}
	if info, err := h.Client.Info(ctx()); err != nil || info.Scope != store.ScopeAdmin {
		t.Errorf("Info() with the admin token = %+v, %v", info, err)
	}
}

func TestRequestLogNamesTheTokenAndNeverContainsIt(t *testing.T) {
	var log lockedBuffer
	h := servertest.New(t, func(s *server.Server) {
		s.Log = slog.New(slog.NewTextHandler(&log, nil))
	})

	if _, err := h.Client.Apps(ctx()); err != nil {
		t.Fatal(err)
	}
	if err := h.Client.SetSecret(ctx(), "web", "KEY", []byte("s3cr3t-value")); err != nil {
		t.Fatal(err)
	}
	if resp := plain(t, h, "/v1/apps"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("expected a 401")
	}

	out := log.String()
	if !strings.Contains(out, "path=/v1/apps status=200") || !strings.Contains(out, "token=test") {
		t.Errorf("the log does not say which token made the request:\n%s", out)
	}
	if !strings.Contains(out, `status=401`) || !strings.Contains(out, `token=""`) {
		t.Errorf("the rejected request is not logged with an empty token name:\n%s", out)
	}
	if strings.Contains(out, h.Client.Token) || strings.Contains(out, "s3cr3t-value") {
		t.Error("the log contains a token or a secret's value")
	}
}

// lockedBuffer is a buffer that handlers of concurrent requests can write
// log lines to while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// --- findings of the security review ----------------------------------------------

func TestRequestsWithoutATokenCannotLockTheOperatorOut(t *testing.T) {
	h := servertest.New(t, func(s *server.Server) { s.Panel = panel.Handler() })

	// What ten <img> tags on a hostile page would send, and then some.
	for range 40 {
		_ = plain(t, h, "/v1/apps")
	}
	if resp := plain(t, h, "/v1/apps"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after the flood a request without a token = %d, want 429", resp.StatusCode)
	}

	// The operator's CLI and panel, from the same address, carry on.
	if _, err := h.Client.Apps(ctx()); err != nil {
		t.Errorf("Apps() with a valid token during the lockout error = %v", err)
	}
	if _, err := h.Client.CreateBackup(ctx()); err != nil {
		t.Errorf("CreateBackup() with a valid token during the lockout error = %v", err)
	}
	if resp := plain(t, h, "/ui/"); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /ui/ during the lockout = %d, want 200", resp.StatusCode)
	}
}

func TestRequestsCausedByOtherSitesAreRefusedEvenWithoutAnOriginHeader(t *testing.T) {
	h := servertest.New(t, func(s *server.Server) { s.Panel = panel.Handler() })

	// An <img>, <script> or <link> tag on another site: a GET without an
	// Origin header, which the browser labels with Sec-Fetch-Site.
	for site, want := range map[string]int{
		"cross-site":  http.StatusForbidden,
		"same-site":   http.StatusForbidden, // another port on the same machine
		"same-origin": http.StatusOK,        // the panel
		"none":        http.StatusOK,        // typed into the address bar
	} {
		resp := request(t, h, http.MethodGet, "/v1/apps", map[string]string{"Sec-Fetch-Site": site})
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET with Sec-Fetch-Site: %s = %d, want %d", site, resp.StatusCode, want)
		}
	}

	// Refused before authentication: such requests do not count as failed
	// attempts, so they cannot be used to lock anybody out either.
	for range 30 {
		req, _ := http.NewRequest(http.MethodGet, h.URL+"/v1/apps", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if resp := plain(t, h, "/v1/apps"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after 30 refused cross-site requests, a plain one = %d, want 401 (not locked out)", resp.StatusCode)
	}
}

func TestRefusedBrowserRequestsAreNotLoggedOneByOne(t *testing.T) {
	var log lockedBuffer
	h := servertest.New(t, func(s *server.Server) {
		s.Log = slog.New(slog.NewTextHandler(&log, nil))
	})

	long := "/v1/" + strings.Repeat("a", 3000)
	for range 50 {
		req, _ := http.NewRequest(http.MethodGet, h.URL+long, nil)
		req.Header.Set("Origin", "https://evil.example")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	out := log.String()
	// A summary line now and then, never a line per request, and never the
	// path in full.
	if n := strings.Count(out, "\n"); n > 3 {
		t.Errorf("50 refused requests wrote %d log lines, want a summary", n)
	}
	if strings.Contains(out, strings.Repeat("a", 300)) {
		t.Error("the log contains a path of thousands of characters")
	}
}

// --- token expiry ----------------------------------------------------------------

func TestExpiredTokenIsRefusedAndSaysSo(t *testing.T) {
	var log lockedBuffer
	h := servertest.New(t, func(s *server.Server) {
		s.Log = slog.New(slog.NewTextHandler(&log, nil))
	})

	expired, err := h.Store.CreateScopedToken(ctx(), "old-ci", store.ScopeAdmin, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&api.Client{BaseURL: h.URL, Token: expired}).Apps(ctx())
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || !strings.HasPrefix(apiErr.Message, "token expired;") {
		t.Errorf("a request with an expired token: %v; want 401 token expired", err)
	}

	// An unknown token still gets the plain answer.
	_, err = (&api.Client{BaseURL: h.URL, Token: store.TokenPrefix + strings.Repeat("0", 64)}).Apps(ctx())
	if !errors.As(err, &apiErr) || !strings.HasPrefix(apiErr.Message, "invalid token;") {
		t.Errorf("a request with an unknown token: %v; want 401 invalid token", err)
	}

	out := log.String()
	if !strings.Contains(out, `msg="expired token used" token=old-ci`) {
		t.Errorf("the log does not name the expired token:\n%s", out)
	}
	if strings.Contains(out, expired) {
		t.Error("the log contains the token itself")
	}
}

func TestInfoSaysWhenTheTokenExpires(t *testing.T) {
	h := servertest.New(t)

	at := time.Now().Add(72 * time.Hour)
	token, err := h.Store.CreateScopedToken(ctx(), "ci", store.ScopeRead, at)
	if err != nil {
		t.Fatal(err)
	}
	info, err := (&api.Client{BaseURL: h.URL, Token: token}).Info(ctx())
	if err != nil || info.TokenExpiresAt == nil || !info.TokenExpiresAt.Equal(at) {
		t.Errorf("Info() with an expiring token = %+v, %v; want it to expire at %v", info, err, at)
	}

	// A token without an expiry leaves the field out altogether.
	req, err := http.NewRequest(http.MethodGet, h.URL+"/v1/info", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.Client.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(body), "token_expires_at") {
		t.Errorf("/v1/info for a token that never expires mentions an expiry: %s", body)
	}
}

func TestRepeatedReadsAreLoggedOnce(t *testing.T) {
	var log lockedBuffer
	h := servertest.New(t, func(s *server.Server) {
		s.Log = slog.New(slog.NewTextHandler(&log, nil))
	})

	for i := 0; i < 3; i++ {
		if _, err := h.Client.Apps(ctx()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Client.History(ctx(), "", 10); err != nil {
			t.Fatal(err)
		}
		if err := h.Client.SetSecret(ctx(), "web", "KEY", []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	// A failed read is always logged.
	if _, err := h.Client.HealthHistory(ctx(), "web", "1s"); err == nil {
		t.Fatal("expected an error for range=1s")
	}
	if _, err := h.Client.HealthHistory(ctx(), "web", "1s"); err == nil {
		t.Fatal("expected an error for range=1s")
	}

	out := log.String()
	for line, want := range map[string]int{
		"method=GET path=/v1/apps status=200":              1,
		"method=GET path=/v1/history status=200":           1,
		"method=PUT path=/v1/apps/web/secrets/KEY status=": 3,
		"method=GET path=/v1/apps/web/health status=400":   2,
	} {
		if got := strings.Count(out, line); got != want {
			t.Errorf("%q appears %d times in the log, want %d:\n%s", line, got, want, out)
		}
	}
}
