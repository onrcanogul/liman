// The tests live in package server_test (not api) for two reasons: they use
// the package the way an outside caller would, and apitest imports api, so
// tests inside package api could not import apitest without a cycle.
package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/engine"
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
	for _, want := range []string{"==> [1/5] Pull image ghcr.io/user/orders-api:1.4", "==> [5/5] Remove previous containers of orders-api"} {
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

	releases, err := h.Store.Releases(ctx(), "orders-api")
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

	releases, _ := h.Store.Releases(ctx(), "orders-api")
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
	releases, _ := h.Store.Releases(ctx(), "orders-api")
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
		if strings.HasPrefix(step, "Remove previous") {
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

	h.Engine.Add(engine.Container{Name: "web-r1", App: "web", Image: "web:1.0", Status: "Up 2 minutes", HostPort: 40123})

	got, err = h.Client.Apps(ctx())
	want := api.Container{App: "web", Container: "web-r1", Image: "web:1.0", State: "running", Status: "Up 2 minutes", HostPort: 40123}
	if err != nil || len(got) != 1 || got[0] != want {
		t.Errorf("Apps() = %+v, %v; want %+v", got, err, want)
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
