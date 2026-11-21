package deploy

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/engine/enginetest"
)

func testWorker() *app.App {
	return &app.App{
		Name:        "orders-api",
		Image:       testImage,
		Kind:        app.KindWorker,
		Command:     []string{"node", "worker.js"},
		Env:         map[string]string{"QUEUE_URL": "amqp://queue"},
		Healthcheck: &app.Healthcheck{Settle: app.Duration(20 * time.Millisecond)},
	}
}

func TestWorkerHasNoPortAndNoProxy(t *testing.T) {
	fake := enginetest.New()
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	d.Proxy = newFakeProxy()
	d.AllocatePort = func() (int, error) { t.Fatal("a worker asked for a port"); return 0, nil }

	res, err := d.Deploy(context.Background(), testWorker(), Options{})
	if err != nil {
		t.Fatalf("Deploy() error = %v\n%s", err, out.String())
	}

	if want := []string{"pull " + testImage, "start " + newContainer}; !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %v, want %v", fake.Calls, want)
	}
	spec := fake.Specs[0]
	if spec.ContainerPort != 0 || spec.HostPort != 0 || spec.Domain != "" || spec.Kind != app.KindWorker || !slices.Equal(spec.Command, []string{"node", "worker.js"}) {
		t.Errorf("spec = %+v, want a worker without ports, with its command", spec)
	}
	if res.HostPort != 0 {
		t.Errorf("HostPort = %d, want none", res.HostPort)
	}
	for _, unwanted := range []string{"Allocate a free host port", "config for", "Reload", "Drain"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("output has %q:\n%s", unwanted, out.String())
		}
	}
	if !strings.Contains(out.String(), "the container must stay up for 20ms") {
		t.Errorf("output does not say how it is checked:\n%s", out.String())
	}
}

// crashingFake is a fake engine in which the new container exits after a
// few inspections: a worker that cannot reach its queue.
type crashingFake struct {
	*enginetest.Fake
	after int
	seen  int
}

func (f *crashingFake) Inspect(ctx context.Context, ref string) (engine.Container, error) {
	ctr, err := f.Fake.Inspect(ctx, ref)
	if err == nil && ref == newContainer {
		f.seen++
		if f.seen > f.after {
			ctr.State, ctr.ExitCode = "exited", 1
		}
	}
	return ctr, err
}

// What the trial showed: with "up after one interval" a worker that dies
// three seconds in was called healthy, and the release that worked was
// stopped. Within its settle time the crash fails the deploy instead.
func TestWorkerThatDiesWhileSettlingKeepsThePrevious(t *testing.T) {
	fake := &crashingFake{Fake: withPrevious(), after: 3}
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	w := testWorker()
	w.Healthcheck.Settle = app.Duration(time.Second)

	_, err := d.Deploy(context.Background(), w, Options{})

	if err == nil || !strings.Contains(err.Error(), "exit code 1") {
		t.Fatalf("Deploy() error = %v, want the crash", err)
	}
	if prev := fake.Containers[prevContainer]; !prev.Running() {
		t.Errorf("previous release is %s, want it still running", prev.State)
	}
	if _, ok := fake.Containers[newContainer]; ok {
		t.Error("the crashed release was left behind")
	}
}

func TestImageHealthcheckIsWaitedFor(t *testing.T) {
	fake := enginetest.New()
	// Docker's verdict comes after its own interval, which can be longer
	// than liman's health timeout.
	fake.Health = func(_ string, n int) string {
		if n < 30 {
			return engine.HealthStarting
		}
		return engine.HealthHealthy
	}
	fake.HealthWindow = 5 * time.Second
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	d.HealthTimeout = 10 * time.Millisecond

	if _, err := d.Deploy(context.Background(), testWorker(), Options{}); err != nil {
		t.Fatalf("Deploy() error = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "waiting for the image's HEALTHCHECK") {
		t.Errorf("output does not say it waits for the image's check:\n%s", out.String())
	}
}

func TestImageHealthcheckUnhealthyFails(t *testing.T) {
	fake := withPrevious()
	fake.Health = func(name string, n int) string {
		if name == newContainer && n >= 2 {
			return engine.HealthUnhealthy
		}
		return engine.HealthStarting
	}
	d := newTestDeployer(fake, &bytes.Buffer{})

	_, err := d.Deploy(context.Background(), testWorker(), Options{})

	if err == nil || !strings.Contains(err.Error(), "HEALTHCHECK reports it unhealthy") {
		t.Fatalf("Deploy() error = %v", err)
	}
	if !fake.Containers[prevContainer].Running() {
		t.Error("previous release was stopped")
	}
}

func TestReplaceStopsTheRunningReleaseFirst(t *testing.T) {
	fake := withPrevious()
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	w := testWorker()
	w.Deploy.Strategy = app.StrategyReplace

	if _, err := d.Deploy(context.Background(), w, Options{}); err != nil {
		t.Fatalf("Deploy() error = %v\n%s", err, out.String())
	}

	// Never two at once: stopped before the new one starts, and not
	// stopped again by the retire step.
	want := []string{"pull " + testImage, "stop " + prevContainer, "start " + newContainer}
	if !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %v, want %v", fake.Calls, want)
	}
	if fake.Containers[newContainer].Strategy != app.StrategyReplace {
		t.Error("the container does not record its strategy for a rollback")
	}
}

func TestReplaceStartsThePreviousAgainWhenTheNewIsNotHealthy(t *testing.T) {
	fake := withPrevious()
	fake.StartState = "exited"
	fake.StartExitCode = 1
	d := newTestDeployer(fake, &bytes.Buffer{})
	w := testWorker()
	w.Deploy.Strategy = app.StrategyReplace

	if _, err := d.Deploy(context.Background(), w, Options{}); err == nil {
		t.Fatal("Deploy() succeeded with a release that exits")
	}
	if !fake.Containers[prevContainer].Running() {
		t.Errorf("previous release is %s, want it running again", fake.Containers[prevContainer].State)
	}
	if !slices.Contains(fake.Calls, "restart "+prevContainer) {
		t.Errorf("calls = %v, want the previous restarted", fake.Calls)
	}
}

func TestRollbackOfAWorkerThatMustNotRunTwice(t *testing.T) {
	fake := enginetest.New()
	fake.Add(engine.Container{Name: prevContainer, App: "orders-api", State: "exited", Kind: app.KindWorker, Strategy: app.StrategyReplace, Release: "r1"})
	fake.Add(engine.Container{Name: newContainer, App: "orders-api", Kind: app.KindWorker, Strategy: app.StrategyReplace, Release: "r2"})
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	d.Proxy = newFakeProxy()

	if _, err := d.Rollback(context.Background(), "orders-api", Options{}); err != nil {
		t.Fatalf("Rollback() error = %v\n%s", err, out.String())
	}

	want := []string{"stop " + newContainer, "restart " + prevContainer, "remove " + newContainer}
	if !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %v, want %v", fake.Calls, want)
	}
	if strings.Contains(out.String(), "config for") {
		t.Errorf("a worker's rollback touched the proxy:\n%s", out.String())
	}
	// The settle time is not on the container; a worker gets the default.
	if !strings.Contains(out.String(), "must stay up for 5ms") {
		t.Errorf("rollback did not wait for the worker to settle:\n%s", out.String())
	}
}

// The trial: the release running was deployed with replace, the one a
// rollback brings back with overlap. The running one must still be
// stopped first.
func TestRollbackGoesByTheRunningReleasesStrategyToo(t *testing.T) {
	fake := enginetest.New()
	fake.Add(engine.Container{Name: prevContainer, App: "orders-api", State: "exited", Kind: app.KindWorker, Release: "r1"})
	fake.Add(engine.Container{Name: newContainer, App: "orders-api", Kind: app.KindWorker, Strategy: app.StrategyReplace, Release: "r2"})
	d := newTestDeployer(fake, &bytes.Buffer{})

	if _, err := d.Rollback(context.Background(), "orders-api", Options{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"stop " + newContainer, "restart " + prevContainer, "remove " + newContainer}; !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %v, want %v", fake.Calls, want)
	}
}
