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

	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/engine/enginetest"
	"github.com/onrcanogul/liman/internal/pipeline"
)

// afterTwoDeploys returns the state two successful deploys leave behind:
// r1 stopped and kept, r2 in service, and the proxy pointing at r2.
func afterTwoDeploys(t *testing.T) (*enginetest.Fake, *fakeProxy) {
	t.Helper()

	fake, proxy := enginetest.New(), newFakeProxy()
	for i, release := range []string{"r1", "r2"} {
		d := newTestDeployer(fake, io.Discard)
		d.Proxy = proxy
		d.NewRelease = func() string { return release }
		d.AllocatePort = func() (int, error) { return 40001 + i, nil }
		if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
			t.Fatalf("deploy %s: %v", release, err)
		}
	}
	// Start from a clean record of calls.
	fake.Calls, proxy.calls = nil, nil
	return fake, proxy
}

func TestRollbackPutsPreviousReleaseBackInService(t *testing.T) {
	fake, proxy := afterTwoDeploys(t)
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	d.Proxy = proxy

	var events []string
	opts := Options{OnEvent: func(release string, ev pipeline.Event) {
		events = append(events, release+": "+ev.Step)
	}}

	res, err := d.Rollback(context.Background(), "orders-api", opts)
	if err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}

	// The result describes the release that is now in service.
	if want := (Result{Release: "r1", Container: "orders-api-r1", HostPort: 40001}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}

	// No image is pulled and no container is created: the kept one is
	// started, traffic is moved, the newer one is removed.
	wantCalls := []string{"restart orders-api-r1", "remove orders-api-r2"}
	if !slices.Equal(fake.Calls, wantCalls) {
		t.Errorf("engine calls = %v, want %v", fake.Calls, wantCalls)
	}
	if got := fake.Running(); !slices.Equal(got, []string{"orders-api-r1"}) {
		t.Errorf("running = %v, want only orders-api-r1", got)
	}
	if len(fake.Containers) != 1 {
		t.Errorf("%d containers left, want only the one rolled back to", len(fake.Containers))
	}

	// The proxy points at the old release's port and reports its release.
	if live := proxy.live["orders-api"]; !strings.Contains(live, "proxy_pass 127.0.0.1:40001;") || !strings.Contains(live, "release r1;") {
		t.Errorf("live proxy config = %q, want it to point at r1 on port 40001", live)
	}

	wantEvents := []string{
		"r1: Start previous container orders-api-r1",
		"r1: Health check",
		"r1: Write Nginx config for api.example.com",
		"r1: Reload Nginx",
		"r1: Remove container orders-api-r2",
	}
	if !slices.Equal(events, wantEvents) {
		t.Errorf("events = %v, want %v", events, wantEvents)
	}
}

func TestRollbackSwitchesTrafficBeforeRemovingTheCurrentRelease(t *testing.T) {
	fake, proxy := afterTwoDeploys(t)
	d := newTestDeployer(fake, io.Discard)
	d.Proxy = proxy

	// When the health probe of the old release runs, the current one must
	// still be running and still be receiving the traffic.
	d.Probe = func(context.Context, string) error {
		if got := fake.Running(); len(got) != 2 {
			t.Errorf("during the health check %v are running, want both", got)
		}
		if !strings.Contains(proxy.live["orders-api"], "release r2;") {
			t.Error("traffic was moved before the old release proved healthy")
		}
		return nil
	}

	if _, err := d.Rollback(context.Background(), "orders-api", Options{}); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
}

func TestRollbackFailureLeavesEverythingAsItWas(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *enginetest.Fake, p *fakeProxy, d *Deployer)
		opts  Options
	}{
		{
			name: "old release cannot be started",
			setup: func(f *enginetest.Fake, _ *fakeProxy, _ *Deployer) {
				f.RestartErr = errors.New("port is already allocated")
			},
		},
		{
			name:  "old release crashes on start",
			setup: func(f *enginetest.Fake, _ *fakeProxy, _ *Deployer) { f.AfterRestart = "exited" },
		},
		{
			name: "old release never becomes healthy",
			setup: func(_ *enginetest.Fake, _ *fakeProxy, d *Deployer) {
				d.HealthTimeout = 20 * time.Millisecond
				d.Probe = func(context.Context, string) error { return errors.New("connection refused") }
			},
		},
		{
			name: "proxy rejects the config",
			setup: func(_ *enginetest.Fake, p *fakeProxy, _ *Deployer) {
				p.testErr = errors.New("nginx config test failed")
			},
		},
		{
			name: "user declines the last step",
			opts: Options{Confirm: func(step pipeline.Step) (bool, error) {
				return !strings.HasPrefix(step.Name, "Remove container"), nil
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake, proxy := afterTwoDeploys(t)
			d := newTestDeployer(fake, io.Discard)
			d.Proxy = proxy
			if tt.setup != nil {
				tt.setup(fake, proxy, d)
			}

			if _, err := d.Rollback(context.Background(), "orders-api", tt.opts); err == nil {
				t.Fatal("Rollback() error = nil, want a failure")
			}

			// The invariant: r2 still serves, r1 is stopped but still kept
			// (so the rollback can be tried again), and the proxy still
			// points at r2.
			if got := fake.Running(); !slices.Equal(got, []string{"orders-api-r2"}) {
				t.Errorf("running = %v, want only orders-api-r2", got)
			}
			if got := fake.State("orders-api-r1"); got != "exited" {
				t.Errorf("state of the old release = %q, want it stopped and kept", got)
			}
			if live := proxy.live["orders-api"]; !strings.Contains(live, "release r2;") {
				t.Errorf("live proxy config = %q, want it to still point at r2", live)
			}
			if file := proxy.files["orders-api"]; !strings.Contains(file, "release r2;") {
				t.Errorf("proxy config on disk = %q, want it to still be r2's", file)
			}
		})
	}
}

func TestRollbackNeedsAPreviousRelease(t *testing.T) {
	t.Run("unknown app", func(t *testing.T) {
		d := newTestDeployer(enginetest.New(), io.Discard)

		_, err := d.Rollback(context.Background(), "orders-api", Options{})

		if !errors.Is(err, engine.ErrNotFound) {
			t.Errorf("Rollback() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("only one release", func(t *testing.T) {
		fake := withPrevious() // a single running container
		d := newTestDeployer(fake, io.Discard)

		_, err := d.Rollback(context.Background(), "orders-api", Options{})

		if !errors.Is(err, ErrNoPrevious) {
			t.Fatalf("Rollback() error = %v, want ErrNoPrevious", err)
		}
		if len(fake.Calls) != 0 {
			t.Errorf("engine calls = %v, want nothing to be touched", fake.Calls)
		}
	})

	t.Run("no second rollback", func(t *testing.T) {
		fake, proxy := afterTwoDeploys(t)
		d := newTestDeployer(fake, io.Discard)
		d.Proxy = proxy

		if _, err := d.Rollback(context.Background(), "orders-api", Options{}); err != nil {
			t.Fatalf("first Rollback() error = %v", err)
		}
		// The release rolled back from is gone, so there is nothing left
		// to go back to.
		if _, err := d.Rollback(context.Background(), "orders-api", Options{}); !errors.Is(err, ErrNoPrevious) {
			t.Errorf("second Rollback() error = %v, want ErrNoPrevious", err)
		}
	})
}

func TestRollbackWithoutDomainLabelIsRefusedWhenProxyIsUsed(t *testing.T) {
	fake := enginetest.New()
	// Containers as an older liman would have left them: no domain label.
	fake.Add(engine.Container{Name: "orders-api-r1", App: "orders-api", State: "exited", Release: "r1"})
	fake.Add(engine.Container{Name: "orders-api-r2", App: "orders-api", Release: "r2"})
	d := newTestDeployer(fake, io.Discard)
	proxy := newFakeProxy()
	// The deploys of that older liman routed the app.
	proxy.files["orders-api"] = "server { }"
	d.Proxy = proxy

	_, err := d.Rollback(context.Background(), "orders-api", Options{})

	if err == nil || !strings.Contains(err.Error(), "did not record its domain") {
		t.Fatalf("Rollback() error = %v, want it to explain the missing domain", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("engine calls = %v, want nothing to be touched", fake.Calls)
	}
}

func TestRollbackDryRunChangesNothing(t *testing.T) {
	fake, proxy := afterTwoDeploys(t)
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	d.Proxy = proxy

	if _, err := d.Rollback(context.Background(), "orders-api", Options{DryRun: true}); err != nil {
		t.Fatalf("Rollback(dry run) error = %v", err)
	}

	if len(fake.Calls) != 0 || len(proxy.calls) != 0 {
		t.Errorf("dry run made calls: engine %v, proxy %v", fake.Calls, proxy.calls)
	}
	// Unlike a deploy, a rollback dry run knows every name and port.
	for _, want := range []string{
		"==> [1/5] Start previous container orders-api-r1",
		"$ docker start orders-api-r1",
		"$ curl --fail http://127.0.0.1:40001/health",
		"- proxy_pass 127.0.0.1:40002;",
		"+ proxy_pass 127.0.0.1:40001;",
		"==> [5/5] Remove container orders-api-r2",
		"$ docker rm -f orders-api-r2",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestRedeployAfterRollbackWorks(t *testing.T) {
	fake, proxy := afterTwoDeploys(t)
	d := newTestDeployer(fake, io.Discard)
	d.Proxy = proxy
	if _, err := d.Rollback(context.Background(), "orders-api", Options{}); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}

	// The next deploy sees r1 as the release in service and keeps it.
	d.NewRelease = func() string { return "r3" }
	d.AllocatePort = func() (int, error) { return 40003, nil }
	if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
		t.Fatalf("Deploy() after rollback error = %v", err)
	}

	if got := fake.Running(); !slices.Equal(got, []string{"orders-api-r3"}) {
		t.Errorf("running = %v, want only orders-api-r3", got)
	}
	if got := fake.State("orders-api-r1"); got != "exited" {
		t.Errorf("state of r1 = %q, want it kept for the next rollback", got)
	}
}

func TestDrain(t *testing.T) {
	t.Run("waits between the switch and retiring the previous release", func(t *testing.T) {
		fake, proxy := withPreviousRelease()
		var out bytes.Buffer
		d := newTestDeployer(fake, &out)
		d.Proxy = proxy
		d.Drain = 7 * time.Second

		var slept time.Duration
		d.Sleep = func(_ context.Context, wait time.Duration) error {
			slept = wait
			// While draining, traffic already goes to the new release and
			// the previous one is still running to finish its requests.
			if !strings.Contains(proxy.live["orders-api"], "release r2;") {
				t.Error("draining started before the proxy was switched")
			}
			if got := fake.Running(); len(got) != 2 {
				t.Errorf("during the drain %v are running, want both releases", got)
			}
			return nil
		}

		if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
			t.Fatalf("Deploy() error = %v", err)
		}

		if slept != 7*time.Second {
			t.Errorf("slept %s, want the configured 7s", slept)
		}
		if !strings.Contains(out.String(), "==> [7/8] Drain connections to the previous release\n    waiting 7s\n") {
			t.Errorf("output does not show the drain step:\n%s", out.String())
		}
	})

	t.Run("is skipped on a first deploy", func(t *testing.T) {
		d := newTestDeployer(enginetest.New(), io.Discard)
		d.Proxy = newFakeProxy()
		d.Drain = 7 * time.Second
		d.Sleep = func(context.Context, time.Duration) error {
			t.Error("slept although there is no previous release to drain")
			return nil
		}

		if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
			t.Fatalf("Deploy() error = %v", err)
		}
	})

	t.Run("is not part of a deploy without a proxy", func(t *testing.T) {
		var out bytes.Buffer
		d := newTestDeployer(withPrevious(), &out)
		d.Drain = 7 * time.Second

		if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
			t.Fatalf("Deploy() error = %v", err)
		}
		// Without a proxy nothing routes traffic, so there is nothing to
		// drain and the pipeline keeps its five steps.
		if strings.Contains(out.String(), "Drain") || !strings.Contains(out.String(), "==> [5/5]") {
			t.Errorf("unexpected steps:\n%s", out.String())
		}
	})

	t.Run("interrupted drain rolls the deploy back", func(t *testing.T) {
		fake, proxy := withPreviousRelease()
		d := newTestDeployer(fake, io.Discard)
		d.Proxy = proxy
		d.Drain = 7 * time.Second
		d.Sleep = func(context.Context, time.Duration) error { return context.Canceled }

		if _, err := d.Deploy(context.Background(), testApp(), Options{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Deploy() error = %v, want context.Canceled", err)
		}

		if got := fake.Running(); !slices.Equal(got, []string{prevContainer}) {
			t.Errorf("running = %v, want only the previous release", got)
		}
		if proxy.live["orders-api"] != oldConf {
			t.Errorf("live proxy config = %q, want the previous one", proxy.live["orders-api"])
		}
	})
}

func TestSleepStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)

	started := time.Now()
	err := sleep(ctx, time.Minute)

	if !errors.Is(err, context.Canceled) || time.Since(started) > 5*time.Second {
		t.Errorf("sleep() = %v after %s, want it to return early with context.Canceled", err, time.Since(started))
	}
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleep() without cancellation = %v, want nil", err)
	}
}
