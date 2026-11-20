package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()

	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state", "liman.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenIsRepeatable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liman.db")
	ctx := context.Background()

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.AppendEvent(ctx, Event{Time: time.Now(), App: "web", Step: "Pull", Action: "run", OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening again must not re-run the migration (which would fail with
	// "table already exists") and must find the data.
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer func() { _ = second.Close() }()

	events, err := second.Events(ctx, "", 10)
	if err != nil || len(events) != 1 {
		t.Errorf("Events() after reopen = %v, %v; want the one stored event", events, err)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liman.db")
	ctx := context.Background()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	// Pretend a newer liman has migrated this database.
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	_, err = Open(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "newer than this liman") {
		t.Errorf("Open() error = %v, want it to refuse the newer schema", err)
	}
}

func TestEvents(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 20, 10, 0, 0, 123456789, time.UTC)

	stored := []Event{
		{Time: base, App: "web", Release: "r1", Step: "Pull image", Action: "run", OK: true, DurationMS: 1200},
		{Time: base.Add(time.Second), App: "api", Release: "r7", Step: "Pull image", Action: "run", OK: true},
		{Time: base.Add(2 * time.Second), App: "web", Release: "r1", Step: "Health check", Action: "run", Error: "not healthy after 30s"},
		{Time: base.Add(3 * time.Second), App: "web", Release: "r1", Step: "Start container", Action: "undo", OK: true, DurationMS: 15},
	}
	for _, ev := range stored {
		if err := s.AppendEvent(ctx, ev); err != nil {
			t.Fatalf("AppendEvent() error = %v", err)
		}
	}

	t.Run("all apps, chronological", func(t *testing.T) {
		got, err := s.Events(ctx, "", 10)
		if err != nil {
			t.Fatalf("Events() error = %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("got %d events, want 4", len(got))
		}
		// Every field must survive the round trip, including the
		// nanoseconds of the timestamp.
		for i := range stored {
			if !got[i].Time.Equal(stored[i].Time) {
				t.Errorf("event %d time = %v, want %v", i, got[i].Time, stored[i].Time)
			}
			got[i].Time = stored[i].Time
			if got[i] != stored[i] {
				t.Errorf("event %d = %+v, want %+v", i, got[i], stored[i])
			}
		}
	})

	t.Run("filtered by app", func(t *testing.T) {
		got, err := s.Events(ctx, "web", 10)
		if err != nil {
			t.Fatalf("Events() error = %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d events, want 3", len(got))
		}
		for _, ev := range got {
			if ev.App != "web" {
				t.Errorf("got an event of app %q", ev.App)
			}
		}
	})

	t.Run("limit keeps the newest", func(t *testing.T) {
		got, err := s.Events(ctx, "", 2)
		if err != nil {
			t.Fatalf("Events() error = %v", err)
		}
		if len(got) != 2 || got[0].Step != "Health check" || got[1].Action != "undo" {
			t.Errorf("got %+v, want the two newest events in chronological order", got)
		}
	})

	t.Run("unknown app is empty", func(t *testing.T) {
		got, err := s.Events(ctx, "nope", 10)
		if err != nil || len(got) != 0 {
			t.Errorf("Events() = %v, %v; want none", got, err)
		}
	})
}

func TestEventTextIsNotInterpretedAsSQL(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	hostile := `x'); DROP TABLE events; --`
	if err := s.AppendEvent(ctx, Event{Time: time.Now(), App: hostile, Step: hostile, Action: "run"}); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}

	got, err := s.Events(ctx, hostile, 10)
	if err != nil || len(got) != 1 || got[0].Step != hostile {
		t.Errorf("Events() = %v, %v; want the hostile text stored verbatim", got, err)
	}
}

func TestReleases(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	first := Release{App: "web", Release: "r1", Image: "web:1.0", Status: StatusSucceeded, StartedAt: start, FinishedAt: start.Add(5 * time.Second)}
	second := Release{App: "web", Release: "r2", Image: "web:1.1", Status: StatusFailed, Error: "Health check: not healthy", StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour + 30*time.Second)}
	other := Release{App: "api", Release: "r1", Image: "api:3", Status: StatusSucceeded, StartedAt: start, FinishedAt: start}

	for _, r := range []Release{first, second, other} {
		if err := s.RecordRelease(ctx, r); err != nil {
			t.Fatalf("RecordRelease() error = %v", err)
		}
	}

	got, err := s.Releases(ctx, "web", 10)
	if err != nil {
		t.Fatalf("Releases() error = %v", err)
	}
	if len(got) != 2 || got[0].Release != "r2" || got[1].Release != "r1" {
		t.Fatalf("Releases() = %+v, want r2 then r1", got)
	}

	// Without a filter every app is included, and the limit keeps the newest.
	all, err := s.Releases(ctx, "", 2)
	if err != nil {
		t.Fatalf("Releases() error = %v", err)
	}
	if len(all) != 2 || all[0].App != "api" || all[1].Release != "r2" {
		t.Errorf("Releases(all, 2) = %+v, want api/r1 then web/r2", all)
	}
	if got[0].Status != StatusFailed || got[0].Error != second.Error || !got[0].FinishedAt.Equal(second.FinishedAt) {
		t.Errorf("newest release = %+v, want %+v", got[0], second)
	}

	// The same release of the same app cannot be recorded twice.
	if err := s.RecordRelease(ctx, first); err == nil {
		t.Error("RecordRelease() of a duplicate succeeded, want a constraint error")
	}
}

func TestTokens(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	token, err := s.CreateToken(ctx, "laptop")
	if err != nil {
		t.Fatalf("CreateToken() error = %v", err)
	}
	if !strings.HasPrefix(token, TokenPrefix) || len(token) != len(TokenPrefix)+64 {
		t.Errorf("token has an unexpected shape (length %d)", len(token))
	}

	other, err := s.CreateToken(ctx, "ci")
	if err != nil {
		t.Fatalf("CreateToken() error = %v", err)
	}
	if other == token {
		t.Error("two tokens are identical")
	}

	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{"valid", token, true},
		{"other valid", other, true},
		{"empty", "", false},
		{"wrong prefix", strings.TrimPrefix(token, TokenPrefix), false},
		{"one character changed", token[:len(token)-1] + "x", false},
		{"well-formed but unknown", TokenPrefix + strings.Repeat("0", 64), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.VerifyToken(ctx, tt.token)
			if err != nil || got != tt.want {
				t.Errorf("VerifyToken() = %v, %v; want %v", got, err, tt.want)
			}
		})
	}

	if _, err := s.CreateToken(ctx, "laptop"); !errors.Is(err, ErrExists) {
		t.Errorf("CreateToken() with a used name error = %v, want ErrExists", err)
	}
	// The failed attempt must not have replaced the existing token.
	if ok, _ := s.VerifyToken(ctx, token); !ok {
		t.Error("the original token stopped working after a rejected duplicate")
	}

	infos, err := s.Tokens(ctx)
	if err != nil || len(infos) != 2 || infos[0].Name != "ci" || infos[1].Name != "laptop" {
		t.Errorf("Tokens() = %+v, %v; want ci and laptop", infos, err)
	}

	if err := s.RevokeToken(ctx, "laptop"); err != nil {
		t.Fatalf("RevokeToken() error = %v", err)
	}
	if ok, _ := s.VerifyToken(ctx, token); ok {
		t.Error("a revoked token is still accepted")
	}
	if ok, _ := s.VerifyToken(ctx, other); !ok {
		t.Error("revoking one token broke another")
	}
	if err := s.RevokeToken(ctx, "laptop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RevokeToken() twice error = %v, want ErrNotFound", err)
	}
}

func TestTokenIsNotStoredInPlainText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liman.db")
	ctx := context.Background()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateToken(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// Read the stored value back through a separate connection.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var hash string
	if err := db.QueryRowContext(ctx, "SELECT hash FROM tokens").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimPrefix(token, TokenPrefix)
	if strings.Contains(hash, secret) || hash == token {
		t.Error("the database contains the token itself")
	}

	// And the raw file must not contain it either.
	for _, file := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(file)
		if err != nil {
			continue // the WAL file may not exist
		}
		if strings.Contains(string(data), secret) {
			t.Errorf("%s contains the token in plain text", filepath.Base(file))
		}
	}
}

func TestDatabaseFilesArePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liman.db")
	ctx := context.Background()

	// An existing, too permissive file must be tightened as well.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	// A write makes SQLite create its companion files.
	if _, err := s.CreateToken(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}

	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(file)
		if err != nil {
			continue // the companion files exist only in WAL mode, while open
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %o, want no access for group and others", filepath.Base(file), perm)
		}
	}
}

func TestSecretsStorage(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, found, err := s.GetSecret(ctx, "web", "KEY"); err != nil || found {
		t.Errorf("GetSecret() of a missing secret = found %v, %v", found, err)
	}

	sealed := []byte{1, 2, 3, 0, 255} // opaque bytes, including a zero
	if err := s.PutSecret(ctx, "web", "KEY", sealed); err != nil {
		t.Fatalf("PutSecret() error = %v", err)
	}
	if err := s.PutSecret(ctx, "web", "ANOTHER", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "api", "KEY", []byte("y")); err != nil {
		t.Fatal(err)
	}

	got, found, err := s.GetSecret(ctx, "web", "KEY")
	if err != nil || !found || string(got) != string(sealed) {
		t.Errorf("GetSecret() = %v, %v, %v; want the stored bytes", got, found, err)
	}

	// Putting again replaces, it does not add a second row.
	if err := s.PutSecret(ctx, "web", "KEY", []byte("replaced")); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetSecret(ctx, "web", "KEY"); string(got) != "replaced" {
		t.Errorf("GetSecret() after replacing = %q", got)
	}

	infos, err := s.ListSecrets(ctx, "web")
	if err != nil || len(infos) != 2 || infos[0].Name != "ANOTHER" || infos[1].Name != "KEY" || infos[1].UpdatedAt.IsZero() {
		t.Errorf("ListSecrets() = %+v, %v; want ANOTHER and KEY with timestamps", infos, err)
	}
	if n, err := s.CountSecrets(ctx); err != nil || n != 3 {
		t.Errorf("CountSecrets() = %d, %v; want 3", n, err)
	}

	if found, err := s.DeleteSecret(ctx, "web", "KEY"); err != nil || !found {
		t.Errorf("DeleteSecret() = %v, %v; want found", found, err)
	}
	if found, _ := s.DeleteSecret(ctx, "web", "KEY"); found {
		t.Error("DeleteSecret() twice reported the secret as found")
	}
	// The other app's secret of the same name is untouched.
	if _, found, _ := s.GetSecret(ctx, "api", "KEY"); !found {
		t.Error("deleting a secret of one app removed another app's")
	}
}

func TestExistingDatabaseIsMigratedForward(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liman.db")
	ctx := context.Background()

	// Build a database as the first schema version left it: run only the
	// first migration by hand.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO tokens (name, hash, created_at) VALUES ('old', 'h', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// Opening it with the current code must add what is missing and keep
	// what is there.
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() of a version 1 database error = %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.PutSecret(ctx, "web", "KEY", []byte("sealed")); err != nil {
		t.Errorf("the secrets table is missing after migration: %v", err)
	}
	if err := s.AppendHealthSample(ctx, HealthSample{Time: time.Now(), App: "web", OK: true}); err != nil {
		t.Errorf("the health_samples table is missing after migration: %v", err)
	}
	if tokens, err := s.Tokens(ctx); err != nil || len(tokens) != 1 || tokens[0].Name != "old" {
		t.Errorf("existing data after migration = %+v, %v", tokens, err)
	}

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Errorf("schema version = %d, %v; want %d", version, err, len(migrations))
	}
}

func TestHealthSamples(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	samples := []HealthSample{
		// First minute: two good checks with a response time.
		{Time: start, App: "web", OK: true, Probed: true, Duration: 10 * time.Millisecond},
		{Time: start.Add(30 * time.Second), App: "web", OK: true, Probed: true, Duration: 30 * time.Millisecond},
		// Second minute: nothing at all.
		// Third minute: one good, one failed, and a check without a probe.
		{Time: start.Add(2 * time.Minute), App: "web", OK: true, Probed: true, Duration: 50 * time.Millisecond},
		{Time: start.Add(2*time.Minute + 10*time.Second), App: "web", OK: false, Probed: true, Duration: 2 * time.Second},
		{Time: start.Add(2*time.Minute + 20*time.Second), App: "web", OK: false},
		// Outside the requested range, and another app.
		{Time: start.Add(-time.Second), App: "web", OK: false},
		{Time: start.Add(3 * time.Minute), App: "web", OK: false},
		{Time: start, App: "api", OK: false},
	}
	for _, h := range samples {
		if err := s.AppendHealthSample(ctx, h); err != nil {
			t.Fatalf("AppendHealthSample() error = %v", err)
		}
	}

	got, err := s.HealthBuckets(ctx, "web", start, start.Add(3*time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("HealthBuckets() error = %v", err)
	}
	want := []HealthBucket{
		{Start: start, OK: 2, Probed: 2, AvgMS: 20, MaxMS: 30},
		{Start: start.Add(2 * time.Minute), OK: 1, Failed: 2, Probed: 2, AvgMS: 1025, MaxMS: 2000},
	}
	if len(got) != len(want) {
		t.Fatalf("HealthBuckets() = %+v, want %+v", got, want)
	}
	for i := range want {
		// Start is compared with Equal: two time.Time values for the same
		// instant are not necessarily == (they can differ in location).
		if !got[i].Start.Equal(want[i].Start) {
			t.Errorf("bucket %d starts at %s, want %s", i, got[i].Start, want[i].Start)
		}
		got[i].Start = want[i].Start
		if got[i] != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if _, err := s.HealthBuckets(ctx, "web", start, start.Add(time.Minute), time.Millisecond); err == nil {
		t.Error("HealthBuckets() with a width below a second succeeded, want an error")
	}

	// Pruning removes what is older than the cut-off and nothing else.
	removed, err := s.PruneHealthSamples(ctx, start.Add(2*time.Minute))
	if err != nil || removed != 4 {
		t.Errorf("PruneHealthSamples() = %d, %v; want 4 removed", removed, err)
	}
	got, err = s.HealthBuckets(ctx, "web", start, start.Add(3*time.Minute), time.Minute)
	if err != nil || len(got) != 1 || got[0].Failed != 2 {
		t.Errorf("after pruning HealthBuckets() = %+v, %v; want only the third minute", got, err)
	}
}
