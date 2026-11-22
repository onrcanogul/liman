package docker

import (
	"maps"
	"testing"
)

func TestLogsAreCappedWhereLimanCanTellTheDriver(t *testing.T) {
	tests := []struct {
		driver string
		want   map[string]string
	}{
		// Docker's default: one file that grows until the disk is full.
		{"json-file", map[string]string{"max-size": "20m", "max-file": "5"}},
		// Podman's file: one file, truncated when it reaches the size.
		{"k8s-file", map[string]string{"max-size": "20m"}},
		// A driver the administrator chose keeps its own settings; an
		// option it does not know would fail the container's creation.
		{"journald", nil},
		{"local", nil},
		{"", nil},
	}
	for _, tt := range tests {
		if got := logOptions(tt.driver); !maps.Equal(got, tt.want) {
			t.Errorf("logOptions(%q) = %v, want %v", tt.driver, got, tt.want)
		}
	}
}
