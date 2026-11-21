package deploy

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/engine/enginetest"
)

func published(endpoints ...string) *app.App {
	a := testApp()
	a.Domain = ""
	for _, e := range endpoints {
		ep, err := app.ParseEndpoint(e)
		if err != nil {
			panic(err)
		}
		a.Publish = append(a.Publish, ep)
	}
	return a
}

func TestOneFixedPortIsTriedFirstThenReplaces(t *testing.T) {
	fake := withPrevious()
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)

	res, err := d.Deploy(context.Background(), published("10.8.0.1:8080"), Options{})
	if err != nil {
		t.Fatalf("Deploy() error = %v\n%s", err, out.String())
	}

	// The trial on a free port, gone again, and only then is the running
	// release stopped: one port cannot hold both.
	trial := newContainer + "-try"
	want := []string{"pull " + testImage, "start " + trial, "remove " + trial, "stop " + prevContainer, "start " + newContainer}
	if !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %v, want %v", fake.Calls, want)
	}
	if s := fake.Specs[0]; s.HostPort != 40001 || s.HostIP != "" {
		t.Errorf("trial on %s:%d, want a free port on 127.0.0.1", s.HostIP, s.HostPort)
	}
	if s := fake.Specs[1]; s.HostPort != 8080 || s.HostIP != "10.8.0.1" || s.Strategy != app.StrategyReplace {
		t.Errorf("release on %s:%d (strategy %s), want 10.8.0.1:8080 and replace for a rollback", s.HostIP, s.HostPort, s.Strategy)
	}
	if res.HostPort != 8080 {
		t.Errorf("HostPort = %d", res.HostPort)
	}
	if !strings.Contains(out.String(), "the release itself goes on 10.8.0.1:8080") {
		t.Errorf("output:\n%s", out.String())
	}
}

// The point of the trial: a broken release fails on the free port and the
// running one is never stopped.
func TestABrokenReleaseNeverTouchesTheFixedPort(t *testing.T) {
	fake := withPrevious()
	fake.StartState, fake.StartExitCode = "exited", 1
	d := newTestDeployer(fake, &bytes.Buffer{})

	if _, err := d.Deploy(context.Background(), published("8080"), Options{}); err == nil {
		t.Fatal("Deploy() succeeded with a release that exits")
	}
	if slices.Contains(fake.Calls, "stop "+prevContainer) || !fake.Containers[prevContainer].Running() {
		t.Errorf("calls = %v; the running release was touched", fake.Calls)
	}
	if _, left := fake.Containers[newContainer+"-try"]; left {
		t.Error("the trial container was left behind")
	}
}

func TestTwoFixedPortsTakeTurns(t *testing.T) {
	tests := []struct {
		name     string
		current  engine.Container
		wantPort int
	}{
		{"running on the first", engine.Container{Name: prevContainer, App: "orders-api", HostPort: 8080, HostIP: "127.0.0.1"}, 8081},
		{"running on the second", engine.Container{Name: prevContainer, App: "orders-api", HostPort: 8081, HostIP: "127.0.0.1"}, 8080},
		{"stopped on the first", engine.Container{Name: prevContainer, App: "orders-api", HostPort: 8080, State: "exited"}, 8080},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := enginetest.New()
			fake.Add(tt.current)
			d := newTestDeployer(fake, &bytes.Buffer{})
			d.AllocatePort = func() (int, error) { t.Fatal("a fixed-port app asked for a free port"); return 0, nil }

			if _, err := d.Deploy(context.Background(), published("8080", "8081"), Options{}); err != nil {
				t.Fatal(err)
			}
			spec := fake.Specs[len(fake.Specs)-1]
			if spec.HostPort != tt.wantPort {
				t.Errorf("new release on %d, want %d", spec.HostPort, tt.wantPort)
			}
			// Side by side: started before the old one is stopped.
			if i, j := slices.Index(fake.Calls, "start "+newContainer), slices.Index(fake.Calls, "stop "+prevContainer); tt.current.Running() && (i < 0 || j < i) {
				t.Errorf("calls = %v, want the new one started before the old one stops", fake.Calls)
			}
		})
	}
}

func TestDryRunOfAFixedPort(t *testing.T) {
	var out bytes.Buffer
	d := newTestDeployer(enginetest.New(), &out)
	if _, err := d.Deploy(context.Background(), published("8080"), Options{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Try the new release on a free port", "Stop the running release of orders-api", "Start container"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run lacks %q:\n%s", want, out.String())
		}
	}
}
