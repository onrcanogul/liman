package deploy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/app"
)

// slowStart is a probe that fails until the app has been up for d, like a
// JVM that takes half a minute to start.
func slowStart(d time.Duration) func(context.Context, string) error {
	started := time.Now()
	return func(context.Context, string) error {
		if time.Since(started) < d {
			return errors.New("connection refused")
		}
		return nil
	}
}

func TestAnAppGetsTheHealthTimeoutItAsksFor(t *testing.T) {
	// The daemon-wide timeout is too short for this app ...
	d := newTestDeployer(withPrevious(), io.Discard)
	d.HealthTimeout = 20 * time.Millisecond
	d.Probe = slowStart(100 * time.Millisecond)
	if _, err := d.Deploy(context.Background(), testApp(), Options{}); err == nil {
		t.Fatal("Deploy() without a timeout of its own succeeded; the test does not test anything")
	}

	// ... and its own is long enough.
	var out bytes.Buffer
	d = newTestDeployer(withPrevious(), &out)
	d.HealthTimeout = 20 * time.Millisecond
	d.Probe = slowStart(100 * time.Millisecond)
	a := testApp()
	a.Healthcheck.Timeout = app.Duration(5 * time.Second)
	if _, err := d.Deploy(context.Background(), a, Options{}); err != nil {
		t.Fatalf("Deploy() with healthcheck.timeout 5s: %v", err)
	}
	if !strings.Contains(out.String(), "(up to 5s)") {
		t.Errorf("output does not name the timeout:\n%s", out.String())
	}
}

func TestTheTimeoutIsKeptForARollback(t *testing.T) {
	fake, proxy := afterTwoDeploys(t)
	for name, c := range fake.Containers {
		if c.Release == "r1" {
			c.HealthTimeout = 5 * time.Second
			fake.Containers[name] = c
		}
	}
	d := newTestDeployer(fake, io.Discard)
	d.Proxy = proxy
	d.HealthTimeout = 20 * time.Millisecond
	d.Probe = slowStart(100 * time.Millisecond)
	if _, err := d.Rollback(context.Background(), "orders-api", Options{}); err != nil {
		t.Fatalf("Rollback() to a release deployed with a 5s timeout: %v", err)
	}
}

func TestTheTimeoutGoesOnTheContainer(t *testing.T) {
	a := testApp()
	a.Healthcheck.Timeout = app.Duration(90 * time.Second)
	fake := withPrevious()
	d := newTestDeployer(fake, io.Discard)
	if _, err := d.Deploy(context.Background(), a, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := fake.Specs[0].HealthTimeout; got != 90*time.Second {
		t.Errorf("Spec.HealthTimeout = %s, want 1m30s", got)
	}
}
