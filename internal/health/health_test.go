package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbe(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr string // empty means healthy
	}{
		{"ok", http.StatusOK, ""},
		{"no content", http.StatusNoContent, ""},
		{"redirect counts as answering", http.StatusFound, ""},
		{"not found", http.StatusNotFound, "unexpected status 404"},
		{"server error", http.StatusInternalServerError, "unexpected status 500"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.status == http.StatusFound {
					// If the probe followed this, it would fail with 404.
					http.Redirect(w, r, "/elsewhere-that-does-not-exist", http.StatusFound)
					return
				}
				if r.URL.Path == "/elsewhere-that-does-not-exist" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			err := Probe(context.Background(), srv.URL+"/health")

			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Probe() error = %v, want healthy", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Probe() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestProbeUnreachable(t *testing.T) {
	// Nothing listens on port 1.
	if err := Probe(context.Background(), URL(1, "/health")); err == nil {
		t.Error("Probe() of a closed port succeeded")
	}
}

func TestURL(t *testing.T) {
	if got, want := URL(40001, "/health?deep=1"), "http://127.0.0.1:40001/health?deep=1"; got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
}
