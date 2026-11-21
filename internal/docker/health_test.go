package docker

import (
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/onrcanogul/liman/internal/engine"
)

func TestHealthWindow(t *testing.T) {
	tests := []struct {
		name string
		hc   *container.HealthConfig
		want time.Duration
	}{
		// What the trial showed: an image that leaves the interval out is
		// first checked after 30 seconds. Three retries 30 s apart, the last
		// of which may run for 30 s.
		{"Docker's defaults", &container.HealthConfig{Test: []string{"CMD-SHELL", "true"}}, 120 * time.Second},
		{"no config at all", nil, 120 * time.Second},
		{"the image's own", &container.HealthConfig{Interval: time.Second, Timeout: 2 * time.Second, Retries: 2, StartPeriod: 5 * time.Second}, 9 * time.Second},
	}
	for _, tt := range tests {
		if got := healthWindow(tt.hc); got != tt.want {
			t.Errorf("%s: healthWindow() = %s, want %s", tt.name, got, tt.want)
		}
	}
}

// The kind and strategy labels are always written: an image declaring them
// itself must not turn a web app into a worker, or the other way round.
func TestKindAndStrategyLabelsAreAlwaysSet(t *testing.T) {
	web := labels(engine.Spec{App: "web"})
	if web[engine.LabelKind] != "web" || web[engine.LabelStrategy] != "overlap" {
		t.Errorf("labels of a web app = %v", web)
	}
	worker := labels(engine.Spec{App: "w", Kind: "worker", Strategy: "replace"})
	if worker[engine.LabelKind] != "worker" || worker[engine.LabelStrategy] != "replace" {
		t.Errorf("labels of a worker = %v", worker)
	}
	// Read back, only the values that change behaviour count.
	if trusted("web", isWorker) != "" || trusted("worker", isWorker) != "worker" || trusted("overlap", isReplace) != "" {
		t.Error("labels read back wrong")
	}
}
