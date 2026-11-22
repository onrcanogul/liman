package docker

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestTheLogDriverIsAskedForEachContainer: the daemon keeps one client for
// its whole life, across restarts of the engine. An administrator who
// changes daemon.json's log driver and restarts dockerd, not limand, must
// not get containers on the old driver.
func TestTheLogDriverIsAskedForEachContainer(t *testing.T) {
	driver := "json-file"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.47")
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/info"):
			_ = json.NewEncoder(w).Encode(map[string]string{"LoggingDriver": driver})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := Connect(context.Background(), Options{Name: "Docker", Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if got := c.logDriverNow(context.Background()); got != "json-file" {
		t.Fatalf("driver = %q, want json-file", got)
	}
	driver = "journald"
	if got := c.logDriverNow(context.Background()); got != "journald" {
		t.Errorf("driver after the engine changed = %q, want journald", got)
	}

	// A preset's driver is the preset's, whatever the engine says.
	c.opts.LogDriver = "k8s-file"
	if got := c.logDriverNow(context.Background()); got != "k8s-file" {
		t.Errorf("driver with a preset = %q, want k8s-file", got)
	}
}
