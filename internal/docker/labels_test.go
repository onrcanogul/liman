package docker

import (
	"testing"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/engine"
)

func TestEveryLabelIsSetSoAnImageCannotSupplyOne(t *testing.T) {
	// An app without a health check and, as in a test, without a domain.
	got := labels(engine.Spec{App: "web", Release: "r1"})

	for _, key := range []string{engine.LabelApp, engine.LabelRelease, engine.LabelHealthPath, engine.LabelDomain} {
		if _, ok := got[key]; !ok {
			t.Errorf("label %s is not set; an image declaring it would decide its value", key)
		}
	}
	if got[engine.LabelHealthPath] != "" || got[engine.LabelDomain] != "" {
		t.Errorf("labels = %v, want the unused ones set to the empty string", got)
	}
}

func TestLabelsReadBackAreCheckedLikeInput(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid func(string) bool
		want  string
	}{
		{"health path", "/health", app.ValidHealthPath, "/health"},
		{"health path that redirects the probe", "@169.254.169.254/latest/meta-data", app.ValidHealthPath, ""},
		{"health path with a line break", "/ok\nHost: evil", app.ValidHealthPath, ""},
		{"domain", "api.example.com", app.ValidDomain, "api.example.com"},
		{"domain that ends the server_name line", "a.example.com; include /etc/shadow", app.ValidDomain, ""},
		{"release", "20261120-081731-895", app.ValidRelease, "20261120-081731-895"},
		{"release with Nginx syntax", `x"; return 200 "`, app.ValidRelease, ""},
		{"empty", "", app.ValidDomain, ""},
	}
	for _, tt := range tests {
		if got := trusted(tt.value, tt.valid); got != tt.want {
			t.Errorf("%s: trusted(%q) = %q, want %q", tt.name, tt.value, got, tt.want)
		}
	}
}
