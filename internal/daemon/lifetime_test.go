package daemon

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/store"
)

func TestParseLifetime(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"90d", 90 * 24 * time.Hour, true},
		{"1d", 24 * time.Hour, true},
		{"12h", 12 * time.Hour, true},
		{"3650d", 3650 * 24 * time.Hour, true},
		{"3651d", 0, false},
		{"999999999999h", 0, false},
		{"0d", 0, false},
		{"-5d", 0, false},
		{"+5d", 0, false},
		{"5", 0, false},
		{"d", 0, false},
		{"", 0, false},
		{"5m", 0, false},
		{"1d12h", 0, false},
		{"2160h", 2160 * time.Hour, true},
		{" 5d", 0, false},
	}
	for _, tt := range tests {
		got, err := parseLifetime(tt.in)
		if tt.ok && (err != nil || got != tt.want) {
			t.Errorf("parseLifetime(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
		if !tt.ok && err == nil {
			t.Errorf("parseLifetime(%q) = %v; want an error", tt.in, got)
		}
	}
}

func TestDescribeExpiry(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.Local)
	tests := []struct {
		at   time.Time
		want string
	}{
		{time.Time{}, "never"},
		{now, "expired 2026-11-20 12:00"},
		{now.Add(-48 * time.Hour), "expired 2026-11-18 12:00"},
		{now.Add(90 * 24 * time.Hour), "2027-02-18 12:00 (in 90 days)"},
		{now.Add(5*24*time.Hour - time.Second), "2026-11-25 11:59 (in 5 days)"},
		{now.Add(47 * time.Hour), "2026-11-22 11:00 (in 47 hours)"},
		{now.Add(5*time.Hour + 59*time.Minute), "2026-11-20 17:59 (in 6 hours)"},
		{now.Add(119 * time.Minute), "2026-11-20 13:59 (in 119 minutes)"},
		{now.Add(time.Minute), "2026-11-20 12:01 (in 1 minute)"},
		{now.Add(30 * time.Second), "2026-11-20 12:00 (in less than a minute)"},
	}
	for _, tt := range tests {
		if got := describeExpiry(store.TokenInfo{ExpiresAt: tt.at}, now); got != tt.want {
			t.Errorf("describeExpiry(%v) = %q, want %q", tt.at, got, tt.want)
		}
	}
}

func TestTokenCreateExpires(t *testing.T) {
	td := newTestDaemon(t)
	ctx := context.Background()

	before := time.Now()
	if code := td.Run(ctx, []string{"token", "create", "ci", "--expires", "30d"}); code != exitOK {
		t.Fatalf("token create --expires: code %d, stderr %q", code, td.stderr.String())
	}
	if !strings.Contains(td.stdout.String(), `Created token "ci" (expires `) {
		t.Errorf("token create does not say when the token expires:\n%s", td.stdout.String())
	}
	token := tokenRe.FindString(td.stdout.String())

	st, err := store.Open(ctx, filepath.Join(td.Home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	info, ok, err := st.LookupToken(ctx, token)
	_ = st.Close()
	want := before.Add(30 * 24 * time.Hour)
	if err != nil || !ok || info.ExpiresAt.Before(want) || info.ExpiresAt.After(want.Add(time.Minute)) {
		t.Errorf("the created token = %+v, %v, %v; want it to expire about %v", info, ok, err, want)
	}

	if code := td.Run(ctx, []string{"token", "create", "laptop"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	if code := td.Run(ctx, []string{"token", "list"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	out := td.stdout.String()
	// Neither has been used: the daemon has not seen them yet.
	if !regexp.MustCompile(`(?m)^NAME\s+SCOPE\s+CREATED\s+EXPIRES\s+LAST USED$`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^ci\s.*\(in 30 days\)\s+-$`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^laptop\s.*\snever\s+-$`).MatchString(out) {
		t.Errorf("token list does not show the expiry:\n%s", out)
	}
}

func TestTokenCreateRejectsABadLifetime(t *testing.T) {
	td := newTestDaemon(t)
	ctx := context.Background()

	if code := td.Run(ctx, []string{"token", "create", "ci", "--expires", "3 months"}); code != exitUsage {
		t.Errorf("token create --expires '3 months': code %d, want %d", code, exitUsage)
	}
	if tokenRe.MatchString(td.stdout.String()) {
		t.Error("a token was printed although the command failed")
	}
	if code := td.Run(ctx, []string{"token", "list"}); code != exitOK || !strings.Contains(td.stdout.String(), "No tokens.") {
		t.Errorf("a token was created although --expires was invalid:\n%s", td.stdout.String())
	}
}

func TestDescribeLastUse(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		used time.Time
		want string
	}{
		{time.Time{}, "-"},
		{now.Add(-20 * time.Second), "in the last minute"},
		{now.Add(-5 * time.Minute), "5 minutes ago"},
		{now.Add(-3 * time.Hour), "3 hours ago"},
		{now.Add(-10 * 24 * time.Hour), "10 days ago"},
	}
	for _, tt := range tests {
		if got := describeLastUse(store.TokenInfo{LastUsedAt: tt.used}, now); got != tt.want {
			t.Errorf("describeLastUse(%v) = %q, want %q", tt.used, got, tt.want)
		}
	}
}
