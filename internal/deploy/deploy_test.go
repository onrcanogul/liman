package deploy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/engine/enginetest"
	"github.com/onrcanogul/liman/internal/pipeline"
)

const (
	testImage     = "ghcr.io/user/orders-api:1.4"
	newContainer  = "orders-api-r2"
	prevContainer = "orders-api-r1"
)

func testApp() *app.App {
	return &app.App{
		Name:        "orders-api",
		Image:       testImage,
		Domain:      "api.example.com",
		Port:        3000,
		Env:         map[string]string{"NODE_ENV": "production", "API_KEY": "s3cret"},
		Resources:   app.Resources{Memory: "512m", CPUs: 0.5},
		Healthcheck: &app.Healthcheck{Path: "/health"},
	}
}

// newTestDeployer returns a deployer whose outside-world dependencies are
// replaced, so tests need no Docker, no network and no real waiting.
func newTestDeployer(e engine.Engine, out io.Writer) *Deployer {
	d := New(e, out)
	d.AllocatePort = func() (int, error) { return 40001, nil }
	d.Probe = func(context.Context, string) error { return nil }
	d.NewRelease = func() string { return "r2" }
	d.HealthTimeout = 200 * time.Millisecond
	d.HealthInterval = time.Millisecond
	// No draining unless a test asks for it; it adds a step.
	d.Drain = 0
	d.Sleep = func(context.Context, time.Duration) error { return nil }
	return d
}

// withPrevious returns a fake engine in which release r1 is running.
func withPrevious() *enginetest.Fake {
	fake := enginetest.New()
	fake.Add(engine.Container{Name: prevContainer, App: "orders-api", HostPort: 40000})
	return fake
}

func TestDeployFirstTime(t *testing.T) {
	fake := enginetest.New()
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)

	var probed string
	d.Probe = func(_ context.Context, url string) error {
		probed = url
		return nil
	}

	res, err := d.Deploy(context.Background(), testApp(), Options{})
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	wantCalls := []string{"pull " + testImage, "start " + newContainer}
	if !slices.Equal(fake.Calls, wantCalls) {
		t.Errorf("calls = %v, want %v", fake.Calls, wantCalls)
	}

	want := Result{Release: "r2", Container: newContainer, HostPort: 40001}
	if res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if wantURL := "http://127.0.0.1:40001/health"; probed != wantURL {
		t.Errorf("probed %q, want %q", probed, wantURL)
	}

	// The app definition must be translated into engine terms correctly.
	spec := fake.Specs[0]
	if spec.App != "orders-api" || spec.Name != newContainer {
		t.Errorf("spec app/name = %q/%q, want orders-api/%s", spec.App, spec.Name, newContainer)
	}
	if spec.ContainerPort != 3000 || spec.HostPort != 40001 {
		t.Errorf("ports = %d -> %d, want 40001 -> 3000", spec.HostPort, spec.ContainerPort)
	}
	if wantMem := int64(512 * 1024 * 1024); spec.MemoryBytes != wantMem {
		t.Errorf("MemoryBytes = %d, want %d", spec.MemoryBytes, wantMem)
	}
	if spec.CPUs != 0.5 {
		t.Errorf("CPUs = %g, want 0.5", spec.CPUs)
	}
	if spec.Env["NODE_ENV"] != "production" {
		t.Errorf("Env = %v, want NODE_ENV=production", spec.Env)
	}

	for _, want := range []string{"==> [1/5] Pull image " + testImage, "none (first deploy)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestDeployRetiresPreviousOnlyAfterNewIsHealthy(t *testing.T) {
	fake := withPrevious()
	d := newTestDeployer(fake, io.Discard)

	// At the moment of the health probe both releases must be running.
	d.Probe = func(context.Context, string) error {
		if got := fake.Running(); len(got) != 2 {
			t.Errorf("during the health check %v are running, want old and new", got)
		}
		return nil
	}

	if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	wantCalls := []string{"pull " + testImage, "start " + newContainer, "stop " + prevContainer}
	if !slices.Equal(fake.Calls, wantCalls) {
		t.Errorf("calls = %v, want %v", fake.Calls, wantCalls)
	}
	// Only the new release runs; the previous one is stopped but kept.
	if got := fake.Running(); !slices.Equal(got, []string{newContainer}) {
		t.Errorf("running = %v, want only %s", got, newContainer)
	}
	if got := fake.State(prevContainer); got != "exited" {
		t.Errorf("previous container state = %q, want it stopped and kept for rollback", got)
	}
}

func TestDeployKeepsOnlyOnePreviousRelease(t *testing.T) {
	fake := enginetest.New()
	// An older stopped release (kept by the deploy before last) and the
	// release currently in service.
	fake.Add(engine.Container{Name: "orders-api-r0", App: "orders-api", State: "exited"})
	fake.Add(engine.Container{Name: prevContainer, App: "orders-api", HostPort: 40000})
	// A container of another app must not be touched.
	fake.Add(engine.Container{Name: "other-r9", App: "other"})
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)

	if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if got := fake.State("orders-api-r0"); got != "" {
		t.Errorf("the release before last still exists (state %q), want it removed", got)
	}
	if got := fake.State(prevContainer); got != "exited" {
		t.Errorf("the last release has state %q, want it stopped and kept", got)
	}
	if got := fake.State("other-r9"); got != "running" {
		t.Errorf("a container of another app has state %q, want it untouched", got)
	}
	for _, want := range []string{"removed orders-api-r0", "stopped " + prevContainer + " (kept for rollback)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestDeployFailureKeepsPreviousRunning(t *testing.T) {
	tests := []struct {
		name string
		// setup makes the deploy fail in a particular way.
		setup     func(f *enginetest.Fake, d *Deployer)
		wantErr   string
		wantCalls []string
	}{
		{
			name:      "pull fails",
			setup:     func(f *enginetest.Fake, _ *Deployer) { f.PullErr = errors.New("manifest unknown") },
			wantErr:   "Pull image " + testImage + ": manifest unknown",
			wantCalls: []string{"pull " + testImage},
		},
		{
			name: "no free port",
			setup: func(_ *enginetest.Fake, d *Deployer) {
				d.AllocatePort = func() (int, error) { return 0, errors.New("no ports left") }
			},
			wantErr:   "Allocate a free host port: no ports left",
			wantCalls: []string{"pull " + testImage},
		},
		{
			name:    "start fails",
			setup:   func(f *enginetest.Fake, _ *Deployer) { f.StartErr = errors.New("port is already allocated") },
			wantErr: "port is already allocated",
			// The start step failed, so there is nothing of it to undo.
			wantCalls: []string{"pull " + testImage, "start " + newContainer},
		},
		{
			name:    "container crashes",
			setup:   func(f *enginetest.Fake, _ *Deployer) { f.StartState = "exited"; f.StartExitCode = 1 },
			wantErr: "Health check: container is exited (exit code 1)",
			// Logs are shown, then the start step is undone.
			wantCalls: []string{"pull " + testImage, "start " + newContainer, "logs " + newContainer, "remove " + newContainer},
		},
		{
			name: "out of memory",
			setup: func(f *enginetest.Fake, _ *Deployer) {
				f.StartState = "exited"
				f.StartExitCode = 137
				f.StartOOMKilled = true
			},
			wantErr:   "exceeding its memory limit (512m)",
			wantCalls: []string{"pull " + testImage, "start " + newContainer, "logs " + newContainer, "remove " + newContainer},
		},
		{
			name: "never healthy",
			setup: func(_ *enginetest.Fake, d *Deployer) {
				d.HealthTimeout = 20 * time.Millisecond
				d.Probe = func(context.Context, string) error { return errors.New("connection refused") }
			},
			wantErr:   "not healthy after 20ms: connection refused",
			wantCalls: []string{"pull " + testImage, "start " + newContainer, "logs " + newContainer, "remove " + newContainer},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := withPrevious()
			d := newTestDeployer(fake, io.Discard)
			tt.setup(fake, d)

			_, err := d.Deploy(context.Background(), testApp(), Options{})

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Deploy() error = %v, want it to contain %q", err, tt.wantErr)
			}
			if !slices.Equal(fake.Calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", fake.Calls, tt.wantCalls)
			}

			// The property that matters: whatever went wrong, the previous
			// release is still there and nothing else is left behind.
			if _, ok := fake.Containers[prevContainer]; !ok {
				t.Error("previous container was removed although the deploy failed")
			}
			if len(fake.Containers) != 1 {
				t.Errorf("%d containers left, want only the previous one", len(fake.Containers))
			}
		})
	}
}

func TestDeployShowsLogsOfFailedContainer(t *testing.T) {
	fake := enginetest.New()
	fake.StartState = "exited"
	fake.LogOutput = "Error: DATABASE_URL is not set\n"
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)

	_, _ = d.Deploy(context.Background(), testApp(), Options{})

	if !strings.Contains(out.String(), "        Error: DATABASE_URL is not set") {
		t.Errorf("output does not show the container logs:\n%s", out.String())
	}
}

func TestDeployRetriesUntilHealthy(t *testing.T) {
	fake := enginetest.New()
	d := newTestDeployer(fake, io.Discard)

	attempts := 0
	d.Probe = func(context.Context, string) error {
		attempts++
		if attempts < 3 {
			return errors.New("connection refused")
		}
		return nil
	}

	if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if attempts != 3 {
		t.Errorf("probe attempts = %d, want 3", attempts)
	}
}

func TestDeployWithoutHealthcheckDoesNotProbe(t *testing.T) {
	fake := enginetest.New()
	d := newTestDeployer(fake, io.Discard)
	d.Probe = func(context.Context, string) error {
		t.Error("Probe was called although no healthcheck is defined")
		return nil
	}

	a := testApp()
	a.Healthcheck = nil

	if _, err := d.Deploy(context.Background(), a, Options{}); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
}

func TestDeployCancelledRollsBack(t *testing.T) {
	fake := withPrevious()
	d := newTestDeployer(fake, io.Discard)
	d.HealthTimeout = time.Minute
	d.Probe = func(context.Context, string) error { return errors.New("connection refused") }

	// Cancel shortly after the deploy starts, as Ctrl+C would.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	// Run the deploy in a goroutine so the test can give up waiting if the
	// cancellation is ignored, instead of hanging for a minute.
	done := make(chan error, 1)
	go func() {
		_, err := d.Deploy(ctx, testApp(), Options{})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Deploy() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Deploy() did not return after the context was cancelled")
	}

	// Even though the context was cancelled, the half-finished release
	// must have been cleaned up.
	if _, ok := fake.Containers[newContainer]; ok {
		t.Error("new container was left behind after cancellation")
	}
	if _, ok := fake.Containers[prevContainer]; !ok {
		t.Error("previous container was removed although the deploy was cancelled")
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	var out bytes.Buffer
	// A nil engine proves the point: any use of it would panic.
	d := newTestDeployer(nil, &out)
	d.AllocatePort = func() (int, error) {
		t.Error("AllocatePort was called during a dry run")
		return 0, nil
	}

	res, err := d.Deploy(context.Background(), testApp(), Options{DryRun: true})
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if res.HostPort != 0 {
		t.Errorf("HostPort = %d, want 0 in a dry run", res.HostPort)
	}

	want := `==> [1/5] Pull image ghcr.io/user/orders-api:1.4
    $ docker pull ghcr.io/user/orders-api:1.4
==> [2/5] Allocate a free host port
==> [3/5] Start container orders-api-r2
    $ docker run -d --name orders-api-r2 --label liman.app=orders-api -p 127.0.0.1:<free port>:3000 --memory 512m --cpus 0.5 -e API_KEY=*** -e NODE_ENV=*** ghcr.io/user/orders-api:1.4
==> [4/5] Health check
    $ curl --fail http://127.0.0.1:<free port>/health
==> [5/5] Retire previous containers of orders-api
    $ docker stop <current container of orders-api>   # kept for rollback; older ones are removed
`
	if out.String() != want {
		t.Errorf("output =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestOutputNeverContainsEnvValues(t *testing.T) {
	fake := enginetest.New()
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)

	// Confirm mode prints the manual commands during a real run.
	opts := Options{Confirm: func(pipeline.Step) (bool, error) { return true, nil }}
	if _, err := d.Deploy(context.Background(), testApp(), opts); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if strings.Contains(out.String(), "s3cret") {
		t.Errorf("output leaks an env value:\n%s", out.String())
	}
	// With real values known, the manual command shows the real port.
	if !strings.Contains(out.String(), "-p 127.0.0.1:40001:3000") {
		t.Errorf("manual command does not show the allocated port:\n%s", out.String())
	}
}

func TestDeclinedStepRollsBack(t *testing.T) {
	fake := withPrevious()
	d := newTestDeployer(fake, io.Discard)

	// Approve everything up to and including the health check, then decline
	// the removal of the previous release.
	opts := Options{Confirm: func(step pipeline.Step) (bool, error) {
		return !strings.HasPrefix(step.Name, "Retire previous"), nil
	}}

	_, err := d.Deploy(context.Background(), testApp(), opts)

	if !errors.Is(err, pipeline.ErrAborted) {
		t.Fatalf("Deploy() error = %v, want ErrAborted", err)
	}
	if _, ok := fake.Containers[newContainer]; ok {
		t.Error("new container was left behind after the abort")
	}
	if _, ok := fake.Containers[prevContainer]; !ok {
		t.Error("previous container is gone after the abort")
	}
}

func TestOnEventReceivesRelease(t *testing.T) {
	fake := enginetest.New()
	d := newTestDeployer(fake, io.Discard)

	var got []string
	opts := Options{OnEvent: func(release string, ev pipeline.Event) {
		got = append(got, release+" "+string(ev.Action)+" "+ev.Step)
	}}

	if _, err := d.Deploy(context.Background(), testApp(), opts); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if len(got) != 5 || got[0] != "r2 run Pull image "+testImage {
		t.Errorf("events = %v, want 5 run events for release r2", got)
	}
}

func TestIndentWriter(t *testing.T) {
	var out bytes.Buffer
	w := indent(&out)

	// Lines may arrive split across several writes.
	for _, chunk := range []string{"first\nsec", "ond\n"} {
		if _, err := io.WriteString(w, chunk); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}

	if want := "    first\n    second\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}
