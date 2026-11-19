package plan

import (
	"testing"

	"github.com/onrcanogul/liman/internal/app"
)

func TestBuildFullDefinition(t *testing.T) {
	a := &app.App{
		Name:        "orders-api",
		Image:       "ghcr.io/user/orders-api:1.4",
		Domain:      "api.example.com",
		Port:        3000,
		Env:         map[string]string{"NODE_ENV": "production"},
		Resources:   app.Resources{Memory: "512m", CPUs: 0.5},
		Healthcheck: &app.Healthcheck{Path: "/health"},
	}

	want := []string{
		"Pull image ghcr.io/user/orders-api:1.4",
		"Allocate a free host port",
		"Start container orders-api on port 3000 (memory 512m, cpus 0.5, 1 env vars)",
		"Health check: GET /health on port 3000",
		"Write Nginx config for api.example.com",
		"Reload Nginx",
	}

	assertSteps(t, Build(a), want)
}

func TestBuildMinimalDefinition(t *testing.T) {
	a := &app.App{
		Name:   "web",
		Image:  "ghcr.io/user/web:2.0",
		Domain: "example.com",
		Port:   8080,
	}

	want := []string{
		"Pull image ghcr.io/user/web:2.0",
		"Allocate a free host port",
		"Start container web on port 8080 (no resource limits, 0 env vars)",
		"Health check: wait until the container is running",
		"Write Nginx config for example.com",
		"Reload Nginx",
	}

	assertSteps(t, Build(a), want)
}

// assertSteps is a test helper. t.Helper makes failures point at the calling
// test instead of at this function.
func assertSteps(t *testing.T, got []Step, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("got %d steps, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Description != want[i] {
			t.Errorf("step %d = %q, want %q", i+1, got[i].Description, want[i])
		}
	}
}
