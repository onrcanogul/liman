package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These tests are in package server (not server_test) because they replace
// the limiter's clock, which is not exported.

func TestAuthLimiterLocksOutAfterTooManyFailures(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	l := newAuthLimiter()
	l.now = func() time.Time { return now }

	// One failure short of the limit: still allowed.
	for range maxAuthFailures - 1 {
		l.recordFailure("10.0.0.5")
	}
	if _, locked := l.blocked("10.0.0.5"); locked {
		t.Fatalf("locked out after %d failures, want the limit to be %d", maxAuthFailures-1, maxAuthFailures)
	}

	l.recordFailure("10.0.0.5")
	wait, locked := l.blocked("10.0.0.5")
	if !locked || wait != authFailureWindow {
		t.Fatalf("blocked() = %v, %v; want locked for the whole window", wait, locked)
	}

	// Another client is not affected.
	if _, locked := l.blocked("10.0.0.6"); locked {
		t.Error("a different address is locked out too")
	}

	// Time passing shortens the wait...
	now = now.Add(40 * time.Second)
	if wait, locked := l.blocked("10.0.0.5"); !locked || wait != 20*time.Second {
		t.Errorf("after 40s blocked() = %v, %v; want 20s left", wait, locked)
	}

	// ...and at the end of the window the client gets a clean slate.
	now = now.Add(20 * time.Second)
	if _, locked := l.blocked("10.0.0.5"); locked {
		t.Error("still locked out after the window ended")
	}
	l.recordFailure("10.0.0.5")
	if _, locked := l.blocked("10.0.0.5"); locked {
		t.Error("a single failure in a new window locked the client out")
	}
}

func TestAuthLimiterFailuresInDifferentWindowsDoNotAddUp(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	l := newAuthLimiter()
	l.now = func() time.Time { return now }

	// Someone who mistypes a token now and then must never be locked out.
	for range maxAuthFailures * 3 {
		l.recordFailure("10.0.0.5")
		now = now.Add(authFailureWindow) // each failure in its own window
		if _, locked := l.blocked("10.0.0.5"); locked {
			t.Fatal("occasional failures led to a lockout")
		}
	}
}

func TestAuthLimiterDoesNotGrowWithoutBound(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	l := newAuthLimiter()
	l.now = func() time.Time { return now }

	// A flood of failures from made-up addresses...
	for i := range 20_000 {
		l.recordFailure(fmt.Sprintf("addr-%d", i))
		// ...spread over time, so earlier entries expire.
		if i%1000 == 999 {
			now = now.Add(authFailureWindow)
		}
	}

	if got := len(l.failures); got > 11_000 {
		t.Errorf("the limiter holds %d entries after the flood, want expired ones to be dropped", got)
	}
}

func TestClientAddressIgnoresForwardedHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
	r.RemoteAddr = "203.0.113.9:51234"
	// What an attacker would send to appear as a new client on every try.
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	r.Header.Set("X-Real-IP", "10.0.0.2")

	if got := clientAddress(r); got != "203.0.113.9" {
		t.Errorf("clientAddress() = %q, want the address of the connection", got)
	}
}

func TestRetryAfterRoundsUp(t *testing.T) {
	tests := []struct {
		wait time.Duration
		want string
	}{
		{500 * time.Millisecond, "1"},
		{time.Second, "1"},
		{1500 * time.Millisecond, "2"},
		{59 * time.Second, "59"},
		{0, "1"},
	}
	for _, tt := range tests {
		if got := retryAfter(tt.wait); got != tt.want {
			t.Errorf("retryAfter(%s) = %q, want %q", tt.wait, got, tt.want)
		}
	}
}
