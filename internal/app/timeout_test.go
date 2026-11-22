package app

import (
	"strings"
	"testing"
	"time"
)

func TestHealthcheckTimeout(t *testing.T) {
	a, err := Parse([]byte("name: api\nimage: a:1\ndomain: api.example.com\nport: 8080\nhealthcheck:\n  path: /actuator/health\n  timeout: 90s\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got := time.Duration(a.Healthcheck.Timeout); got != 90*time.Second {
		t.Errorf("Healthcheck.Timeout = %s, want 1m30s", got)
	}

	// A timeout alone is enough: it applies to the image's HEALTHCHECK too.
	if _, err := Parse([]byte("name: api\nimage: a:1\ndomain: api.example.com\nport: 8080\nhealthcheck:\n  timeout: 2m\n")); err != nil {
		t.Errorf("timeout only: %v", err)
	}

	for _, tt := range []struct{ value, want string }{
		{"500ms", "healthcheck.timeout: must be between 1s and 10m0s"},
		{"11m", "healthcheck.timeout: must be between 1s and 10m0s"},
		{"-5s", "healthcheck.timeout: must be between 1s and 10m0s"},
		{"soon", "is not a duration"},
	} {
		_, err := Parse([]byte("name: api\nimage: a:1\ndomain: api.example.com\nport: 8080\nhealthcheck:\n  path: /h\n  timeout: " + tt.value + "\n"))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("timeout %s: error = %v, want %q", tt.value, err, tt.want)
		}
	}
}

func TestValidHealthTimeout(t *testing.T) {
	for d, want := range map[time.Duration]bool{
		0: false, time.Second: true, 90 * time.Second: true, 10 * time.Minute: true,
		10*time.Minute + time.Second: false, -time.Second: false,
	} {
		if got := ValidHealthTimeout(d); got != want {
			t.Errorf("ValidHealthTimeout(%s) = %v, want %v", d, got, want)
		}
	}
}

func TestFieldsNameTheTimeoutOnlyWhenSet(t *testing.T) {
	a := &App{Name: "api", Image: "a:1", Domain: "api.example.com", Port: 8080, Healthcheck: &Healthcheck{Path: "/h"}}
	if _, ok := a.Fields()["healthcheck.timeout"]; ok {
		t.Error("Fields() names healthcheck.timeout although it is not set; old checksums would change")
	}
	a.Healthcheck.Timeout = Duration(90 * time.Second)
	if got := a.Fields()["healthcheck.timeout"]; got != "1m30s" {
		t.Errorf(`Fields()["healthcheck.timeout"] = %q, want "1m30s"`, got)
	}
}
