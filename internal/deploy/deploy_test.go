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
)

func testApp() *app.App {
	return &app.App{
		Name:        "orders-api",
		Image:       "ghcr.io/user/orders-api:1.4",
		Domain:      "api.example.com",
		Port:        3000,
		Env:         map[string]string{"NODE_ENV": "production"},
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
	d.HealthTimeout = 200 * time.Millisecond
	d.HealthInterval = time.Millisecond
	return d
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

	res, err := d.Deploy(context.Background(), testApp())
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	wantCalls := []string{"pull ghcr.io/user/orders-api:1.4", "start orders-api"}
	if !slices.Equal(fake.Calls, wantCalls) {
		t.Errorf("calls = %v, want %v", fake.Calls, wantCalls)
	}

	if res.HostPort != 40001 {
		t.Errorf("HostPort = %d, want 40001", res.HostPort)
	}
	if want := "http://127.0.0.1:40001/health"; probed != want {
		t.Errorf("probed %q, want %q", probed, want)
	}

	// The app definition must be translated into engine terms correctly.
	spec := fake.Specs[0]
	if spec.ContainerPort != 3000 || spec.HostPort != 40001 {
		t.Errorf("ports = %d -> %d, want 40001 -> 3000", spec.HostPort, spec.ContainerPort)
	}
	if want := int64(512 * 1024 * 1024); spec.MemoryBytes != want {
		t.Errorf("MemoryBytes = %d, want %d", spec.MemoryBytes, want)
	}
	if spec.CPUs != 0.5 {
		t.Errorf("CPUs = %g, want 0.5", spec.CPUs)
	}
	if spec.Env["NODE_ENV"] != "production" {
		t.Errorf("Env = %v, want NODE_ENV=production", spec.Env)
	}

	if !strings.Contains(out.String(), "==> Pull image ghcr.io/user/orders-api:1.4") {
		t.Errorf("output does not show the pull step:\n%s", out.String())
	}
}

func TestDeployReplacesPreviousContainer(t *testing.T) {
	fake := enginetest.New()
	fake.Containers["orders-api"] = engine.Container{Name: "orders-api", State: "running"}
	d := newTestDeployer(fake, io.Discard)

	if _, err := d.Deploy(context.Background(), testApp()); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	// Order matters: the old container is only removed after the new image
	// has been pulled successfully.
	wantCalls := []string{"pull ghcr.io/user/orders-api:1.4", "remove orders-api", "start orders-api"}
	if !slices.Equal(fake.Calls, wantCalls) {
		t.Errorf("calls = %v, want %v", fake.Calls, wantCalls)
	}
}

func TestDeployKeepsPreviousContainerWhenPullFails(t *testing.T) {
	fake := enginetest.New()
	fake.Containers["orders-api"] = engine.Container{Name: "orders-api", State: "running"}
	fake.PullErr = errors.New("manifest unknown")
	d := newTestDeployer(fake, io.Discard)

	_, err := d.Deploy(context.Background(), testApp())

	if !errors.Is(err, fake.PullErr) {
		t.Fatalf("Deploy() error = %v, want the pull error", err)
	}
	if _, stillThere := fake.Containers["orders-api"]; !stillThere {
		t.Error("previous container was removed although the pull failed")
	}
}

func TestDeployFailsFastWhenContainerExits(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(f *enginetest.Fake)
		wantErr string
	}{
		{
			name:    "crash",
			setup:   func(f *enginetest.Fake) { f.StartState = "exited"; f.StartExitCode = 1 },
			wantErr: "container is exited (exit code 1)",
		},
		{
			name:    "out of memory",
			setup:   func(f *enginetest.Fake) { f.StartState = "exited"; f.StartExitCode = 137; f.StartOOMKilled = true },
			wantErr: "exceeding its memory limit (512m)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := enginetest.New()
			tt.setup(fake)
			d := newTestDeployer(fake, io.Discard)
			// A long timeout proves the failure is immediate: if the code
			// waited for the deadline, the test would hang for a minute.
			d.HealthTimeout = time.Minute

			_, err := d.Deploy(context.Background(), testApp())

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Deploy() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDeployTimesOutWhenNeverHealthy(t *testing.T) {
	fake := enginetest.New()
	d := newTestDeployer(fake, io.Discard)
	d.HealthTimeout = 20 * time.Millisecond
	d.Probe = func(context.Context, string) error { return errors.New("connection refused") }

	_, err := d.Deploy(context.Background(), testApp())

	if err == nil || !strings.Contains(err.Error(), "not healthy after 20ms") {
		t.Fatalf("Deploy() error = %v, want a timeout", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q does not include the last probe failure", err)
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

	if _, err := d.Deploy(context.Background(), testApp()); err != nil {
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

	if _, err := d.Deploy(context.Background(), a); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
}

func TestDeployStopsWhenContextIsCancelled(t *testing.T) {
	fake := enginetest.New()
	d := newTestDeployer(fake, io.Discard)
	d.HealthTimeout = time.Minute
	d.Probe = func(context.Context, string) error { return errors.New("connection refused") }

	// Cancel shortly after the deploy starts, as Ctrl+C would.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	done := make(chan error, 1)
	go func() {
		_, err := d.Deploy(ctx, testApp())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Deploy() error = nil, want an error after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Deploy() did not return after the context was cancelled")
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
