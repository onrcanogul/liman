package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// Across apps: every secret, with its app, whether or not that app
	// has ever been deployed. ListSecrets leaves App empty.
	all, err := s.ListAllSecrets(ctx)
	if err != nil || len(all) != 3 ||
		all[0].App != "api" || all[0].Name != "KEY" ||
		all[1].App != "web" || all[1].Name != "ANOTHER" ||
		all[2].App != "web" || all[2].Name != "KEY" || all[2].UpdatedAt.IsZero() {
		t.Errorf("ListAllSecrets() = %+v, %v; want api/KEY, web/ANOTHER, web/KEY", all, err)
	}
	if infos[0].App != "" {
		t.Errorf("ListSecrets() fills in App = %q; it is only for listings across apps", infos[0].App)
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

func TestSnapshotIsACompleteCopyTakenWhileTheDatabaseIsInUse(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := s.CreateToken(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "web", "KEY", []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		if err := s.AppendEvent(ctx, Event{Time: time.Now(), App: "web", Release: "r1", Step: fmt.Sprintf("step %d", i), Action: "run", OK: true}); err != nil {
			t.Fatal(err)
		}
	}

	dest := filepath.Join(dir, "backup.db")
	if err := s.Snapshot(ctx, dest); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	// Written after the snapshot: must not be in it.
	if err := s.AppendEvent(ctx, Event{Time: time.Now(), App: "web", Release: "r1", Step: "later", Action: "run", OK: true}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %o, want 600", perm)
	}
	// One file, and nothing left behind next to it.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("backup directory holds %d entries, want only the backup", len(entries))
	}

	b, err := OpenReadOnly(ctx, dest)
	if err != nil {
		t.Fatalf("OpenReadOnly() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	sum, err := b.Inspect(ctx)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if sum.SchemaVersion != len(migrations) || sum.Rows["events"] != 20 || sum.Rows["tokens"] != 1 || sum.Rows["secrets"] != 1 {
		t.Errorf("Inspect() = %+v, want the current schema, 20 events, 1 token, 1 secret", sum)
	}
	refs, err := b.SecretRefs(ctx)
	if err != nil || len(refs) != 1 || refs[0] != (SecretRef{App: "web", Name: "KEY"}) {
		t.Errorf("SecretRefs() = %+v, %v", refs, err)
	}
	if sealed, found, err := b.GetSecret(ctx, "web", "KEY"); err != nil || !found || string(sealed) != "sealed" {
		t.Errorf("GetSecret() from the backup = %q, %v, %v", sealed, found, err)
	}

	// A backup opened for inspection cannot be changed.
	if err := b.PutSecret(ctx, "web", "OTHER", []byte("x")); err == nil {
		t.Error("writing to a database opened read-only succeeded")
	}

	// A second backup under the same name is refused, and the first stays.
	if err := s.Snapshot(ctx, dest); err == nil {
		t.Error("Snapshot() over an existing file succeeded, want an error")
	}
	if after, err := os.Stat(dest); err != nil || after.Size() != info.Size() {
		t.Errorf("the existing backup changed: %v, %v", after, err)
	}
}

func TestOpenReadOnlyRejectsWhatIsNotALimanDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := OpenReadOnly(ctx, filepath.Join(dir, "missing.db")); err == nil {
		t.Error("OpenReadOnly() of a missing file succeeded")
	}

	text := filepath.Join(dir, "notes.db")
	if err := os.WriteFile(text, []byte("this is not a database, just some text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(ctx, text); err == nil {
		t.Error("OpenReadOnly() of a text file succeeded")
	}

	// An empty SQLite database is a database, but not one of ours.
	empty := filepath.Join(dir, "empty.db")
	db, err := sql.Open("sqlite", "file:"+empty)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE x (y)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := OpenReadOnly(ctx, empty); err == nil {
		t.Error("OpenReadOnly() of a foreign database succeeded")
	}
}

func TestInspectFindsDamage(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := range 400 {
		if err := s.AppendEvent(ctx, Event{Time: time.Now(), App: "web", Release: "r1", Step: fmt.Sprintf("a long step description number %d", i), Action: "run", OK: true}); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Snapshot(ctx, dest); err != nil {
		t.Fatal(err)
	}

	// Overwrite a stretch in the middle of the file, as a failing disk or a
	// cut-off transfer would.
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(data) / 2; i < len(data)/2+4096 && i < len(data); i++ {
		data[i] = 0xff
	}
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		t.Fatal(err)
	}

	b, err := OpenReadOnly(ctx, dest)
	if err != nil {
		return // refused at the door: also fine
	}
	defer func() { _ = b.Close() }()
	if _, err := b.Inspect(ctx); err == nil {
		t.Error("Inspect() of a damaged file reported no problem")
	}
}

func TestTokenScopesAndNames(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	admin, err := s.CreateToken(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := s.CreateScopedToken(ctx, "wall.board-1", ScopeRead, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if info, ok, err := s.LookupToken(ctx, admin); err != nil || !ok || info.Name != "laptop" || info.Scope != ScopeAdmin {
		t.Errorf("LookupToken(admin) = %+v, %v, %v", info, ok, err)
	}
	if info, ok, err := s.LookupToken(ctx, reader); err != nil || !ok || info.Name != "wall.board-1" || info.Scope != ScopeRead {
		t.Errorf("LookupToken(reader) = %+v, %v, %v", info, ok, err)
	}
	if info, ok, _ := s.LookupToken(ctx, TokenPrefix+strings.Repeat("0", 64)); ok || info.Name != "" {
		t.Errorf("LookupToken(unknown) = %+v, %v; want nothing", info, ok)
	}

	tokens, err := s.Tokens(ctx)
	if err != nil || len(tokens) != 2 || tokens[0].Scope != ScopeAdmin || tokens[1].Scope != ScopeRead {
		t.Errorf("Tokens() = %+v, %v; want laptop (admin) and wall.board-1 (read)", tokens, err)
	}

	if _, err := s.CreateScopedToken(ctx, "x", "superuser", time.Time{}); err == nil {
		t.Error("a token with an unknown scope was created")
	}
	// Names end up in log lines and in terminal listings.
	for _, name := range []string{"two words", "line\nbreak", "esc\x1b[31m", "token=admin", "-dash", strings.Repeat("a", 65), "tür"} {
		if _, err := s.CreateToken(ctx, name); err == nil {
			t.Errorf("a token named %q was created", name)
		}
	}
}

func TestTokensFromBeforeScopesKeepTheirPower(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liman.db")
	ctx := context.Background()

	// A database as schema version 3 left it, with a token in it.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	token := TokenPrefix + strings.Repeat("ab", 32)
	if _, err := db.ExecContext(ctx, `INSERT INTO tokens (name, hash, created_at) VALUES ('old', ?, '2026-01-01T00:00:00.000000000Z')`, hashToken(token)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if info, ok, err := s.LookupToken(ctx, token); err != nil || !ok || info.Scope != ScopeAdmin {
		t.Errorf("the old token after migration = %+v, %v, %v; want it valid with scope admin", info, ok, err)
	}
}

func TestTokenExpiry(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	soon := time.Now().Add(time.Hour)
	live, err := s.CreateScopedToken(ctx, "ci", ScopeAdmin, soon)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	dead, err := s.CreateScopedToken(ctx, "old-laptop", ScopeRead, past)
	if err != nil {
		t.Fatal(err)
	}
	forever, err := s.CreateToken(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}

	info, ok, err := s.LookupToken(ctx, live)
	if err != nil || !ok || !info.ExpiresAt.Equal(soon) {
		t.Errorf("LookupToken(live) = %+v, %v, %v; want valid, expiring at %v", info, ok, err, soon)
	}

	// Refused, but still named, so the refusal can say why.
	info, ok, err = s.LookupToken(ctx, dead)
	if err != nil || ok || info.Name != "old-laptop" || !info.Expired(time.Now()) {
		t.Errorf("LookupToken(expired) = %+v, %v, %v; want refused, named old-laptop", info, ok, err)
	}
	if ok, _ := s.VerifyToken(ctx, dead); ok {
		t.Error("VerifyToken accepts an expired token")
	}

	info, ok, err = s.LookupToken(ctx, forever)
	if err != nil || !ok || !info.ExpiresAt.IsZero() {
		t.Errorf("LookupToken(no expiry) = %+v, %v, %v; want valid with a zero ExpiresAt", info, ok, err)
	}

	// The listing shows all three, expired ones included: they are removed
	// by revoking them, not by time.
	tokens, err := s.Tokens(ctx)
	if err != nil || len(tokens) != 3 {
		t.Fatalf("Tokens() = %+v, %v; want 3", tokens, err)
	}
	byName := map[string]TokenInfo{}
	for _, tok := range tokens {
		byName[tok.Name] = tok
	}
	if !byName["ci"].ExpiresAt.Equal(soon) || !byName["old-laptop"].ExpiresAt.Equal(past) || !byName["laptop"].ExpiresAt.IsZero() {
		t.Errorf("Tokens() expiry times = %+v", tokens)
	}
}

func TestTokenInfoExpired(t *testing.T) {
	at := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		info TokenInfo
		now  time.Time
		want bool
	}{
		{"never expires", TokenInfo{}, at, false},
		{"before", TokenInfo{ExpiresAt: at}, at.Add(-time.Nanosecond), false},
		// The moment itself counts as expired: "valid until T" means
		// not at T.
		{"at the moment", TokenInfo{ExpiresAt: at}, at, true},
		{"after", TokenInfo{ExpiresAt: at}, at.Add(time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.Expired(tt.now); got != tt.want {
				t.Errorf("Expired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTokensFromBeforeExpiryNeverExpire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liman.db")
	ctx := context.Background()

	// A database as schema version 4 left it, with a token in it.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := db.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 4"); err != nil {
		t.Fatal(err)
	}
	token := TokenPrefix + strings.Repeat("cd", 32)
	if _, err := db.ExecContext(ctx, `INSERT INTO tokens (name, hash, scope, created_at) VALUES ('old', ?, 'read', '2026-01-01T00:00:00.000000000Z')`, hashToken(token)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	info, ok, err := s.LookupToken(ctx, token)
	if err != nil || !ok || info.Scope != ScopeRead || !info.ExpiresAt.IsZero() {
		t.Errorf("the old token after migration = %+v, %v, %v; want it valid, read, never expiring", info, ok, err)
	}
}

func TestTouchToken(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateToken(ctx, "ci"); err != nil {
		t.Fatal(err)
	}
	lastUsed := func() time.Time {
		t.Helper()
		tokens, err := s.Tokens(ctx)
		if err != nil || len(tokens) != 1 {
			t.Fatalf("Tokens() = %+v, %v", tokens, err)
		}
		return tokens[0].LastUsedAt
	}

	if got := lastUsed(); !got.IsZero() {
		t.Errorf("LastUsedAt of a new token = %v, want zero", got)
	}
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if err := s.TouchToken(ctx, "ci", at); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Equal(at) {
		t.Errorf("LastUsedAt = %v, want %v", got, at)
	}
	// Requests finish in any order; an earlier one must not move the time
	// back.
	if err := s.TouchToken(ctx, "ci", at.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Equal(at) {
		t.Errorf("LastUsedAt after an earlier touch = %v, want it still %v", got, at)
	}
	if err := s.TouchToken(ctx, "ci", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Equal(at.Add(time.Hour)) {
		t.Errorf("LastUsedAt after a later touch = %v, want %v", got, at.Add(time.Hour))
	}
	// A token revoked meanwhile is not an error: the request that used it
	// was valid when it arrived.
	if err := s.TouchToken(ctx, "gone", at); err != nil {
		t.Errorf("TouchToken() of a missing token = %v, want nil", err)
	}
}

func TestTokensLimitedToApps(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	ci, err := s.CreateScopedToken(ctx, "ci", ScopeAdmin, time.Time{}, "orders-api", "billing", "orders-api")
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.CreateToken(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}

	info, ok, err := s.LookupToken(ctx, ci)
	if err != nil || !ok || !slices.Equal(info.Apps, []string{"billing", "orders-api"}) || !info.Limited() {
		t.Fatalf("LookupToken(ci) = %+v, %v, %v; want limited to billing and orders-api, sorted, once each", info, ok, err)
	}
	if !info.MayUse("orders-api") || info.MayUse("shop") {
		t.Errorf("MayUse: orders-api %v, shop %v; want true, false", info.MayUse("orders-api"), info.MayUse("shop"))
	}
	info, _, _ = s.LookupToken(ctx, all)
	if info.Limited() || !info.MayUse("anything") {
		t.Errorf("a token without apps = %+v; want it to use every app", info)
	}

	tokens, err := s.Tokens(ctx)
	if err != nil || len(tokens) != 2 || !slices.Equal(tokens[0].Apps, []string{"billing", "orders-api"}) || tokens[1].Apps != nil {
		t.Errorf("Tokens() = %+v, %v; want ci limited and laptop not", tokens, err)
	}

	if _, err := s.CreateScopedToken(ctx, "bad", ScopeAdmin, time.Time{}, ""); err == nil {
		t.Error("CreateScopedToken accepts an empty app name")
	}

	// Revoking takes the limits along. A token created again under the
	// same name must not inherit them, in either direction.
	if err := s.RevokeToken(ctx, "ci"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_apps`).Scan(&left); err != nil || left != 0 {
		t.Errorf("token_apps rows after revoking = %d, %v; want 0", left, err)
	}
	again, err := s.CreateToken(ctx, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if info, _, _ := s.LookupToken(ctx, again); info.Limited() {
		t.Errorf("a new token under a revoked name = %+v; want no limits", info)
	}
}

func TestEventsAndReleasesOfASetOfApps(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	for _, app := range []string{"a", "b", "c"} {
		if err := s.AppendEvent(ctx, Event{Time: now, App: app, Release: "r1", Step: "Pull", Action: "run", OK: true}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordRelease(ctx, Release{App: app, Release: "r1", Image: app + ":1", Status: "succeeded", StartedAt: now, FinishedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	apps := func(evs []Event) []string {
		var out []string
		for _, e := range evs {
			out = append(out, e.App)
		}
		return out
	}

	if got, err := s.EventsOf(ctx, nil, 10); err != nil || len(got) != 3 {
		t.Errorf("EventsOf(nil) = %v, %v; want every app", apps(got), err)
	}
	if got, err := s.EventsOf(ctx, []string{"a", "c"}, 10); err != nil || !slices.Equal(apps(got), []string{"a", "c"}) {
		t.Errorf("EventsOf(a, c) = %v, %v", apps(got), err)
	}
	// The limit counts matching rows only: a limited caller is not left
	// with less than it asked for because other apps were busier.
	if got, _ := s.EventsOf(ctx, []string{"a"}, 1); len(got) != 1 || got[0].App != "a" {
		t.Errorf("EventsOf(a, limit 1) = %v", apps(got))
	}
	// An empty set is no app, not every app.
	if got, err := s.EventsOf(ctx, []string{}, 10); err != nil || len(got) != 0 {
		t.Errorf("EventsOf(empty) = %v, %v; want nothing", apps(got), err)
	}
	if got, err := s.ReleasesOf(ctx, []string{"b"}, 10); err != nil || len(got) != 1 || got[0].App != "b" {
		t.Errorf("ReleasesOf(b) = %+v, %v", got, err)
	}
	if got, err := s.ReleasesOf(ctx, []string{}, 10); err != nil || len(got) != 0 {
		t.Errorf("ReleasesOf(empty) = %+v, %v; want nothing", got, err)
	}
}

func TestRegistrations(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := Registration{Name: "web", Repo: "ghcr.io/team/web", Domain: "web.example.com"}
	if err := s.Register(ctx, web); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ctx, web); !errors.Is(err, ErrExists) {
		t.Errorf("Register() twice = %v; want ErrExists", err)
	}
	if err := s.Register(ctx, Registration{Name: "api", Repo: "ghcr.io/team/api", Domain: "web.example.com"}); !errors.Is(err, ErrDomainTaken) {
		t.Errorf("Register() with a taken domain = %v; want ErrDomainTaken", err)
	}
	if r, found, err := s.Registration(ctx, "web"); err != nil || !found || r.Repo != web.Repo || r.Domain != web.Domain || r.CreatedAt.IsZero() {
		t.Errorf("Registration() = %+v, %v, %v", r, found, err)
	}
	if owner, err := s.DomainOwner(ctx, "web.example.com"); err != nil || owner != "web" {
		t.Errorf("DomainOwner() = %q, %v", owner, err)
	}
	if list, err := s.Registrations(ctx); err != nil || len(list) != 1 {
		t.Errorf("Registrations() = %+v, %v", list, err)
	}

	st := DeployState{App: "web", Release: "r1", Image: "ghcr.io/team/web@sha256:x", Checksum: "c1", Fields: map[string]string{"port": "f1"}, Templates: map[string]string{"port": "t1"}}
	if err := s.SaveDeployState(ctx, st); err != nil {
		t.Fatal(err)
	}
	st.Release, st.Checksum = "r2", "c2"
	if err := s.SaveDeployState(ctx, st); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.DeployStateOf(ctx, "web"); err != nil || !found || got.Release != "r2" || got.Checksum != "c2" || got.Fields["port"] != "f1" || got.Templates["port"] != "t1" {
		t.Errorf("DeployStateOf() = %+v, %v, %v", got, found, err)
	}
	if err := s.ForgetDeployState(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.DeployStateOf(ctx, "web"); found {
		t.Error("the state survived ForgetDeployState")
	}

	_ = s.SaveDeployState(ctx, st)
	if _, err := s.Unregister(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.DeployStateOf(ctx, "web"); found {
		t.Error("the state survived Unregister")
	}
	if _, err := s.Unregister(ctx, "web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Unregister() twice = %v; want ErrNotFound", err)
	}
}

func TestUnregisterTakesTheKeysWithIt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, app := range []string{"web", "api"} {
		if err := s.Register(ctx, Registration{Name: app, Repo: "r/" + app, Domain: app + ".example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	web, _ := s.CreateScopedToken(ctx, "web-ci", ScopeDeploy, time.Time{}, "web")
	pr, _ := s.CreateScopedToken(ctx, "web-pr", ScopePreview, time.Time{}, "web")
	both, _ := s.CreateScopedToken(ctx, "both", ScopeDeploy, time.Time{}, "web", "api")
	admin, _ := s.CreateScopedToken(ctx, "web-admin", ScopeAdmin, time.Time{}, "web")

	changes, err := s.Unregister(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changes.Revoked, []string{"web-ci", "web-pr"}) || !slices.Equal(changes.Detached, []string{"both"}) {
		t.Errorf("Unregister() = %+v", changes)
	}
	for _, tok := range []string{web, pr} {
		if _, ok, _ := s.LookupToken(ctx, tok); ok {
			t.Error("a key of the unregistered app still works")
		}
	}
	if info, ok, _ := s.LookupToken(ctx, both); !ok || !slices.Equal(info.Apps, []string{"api"}) {
		t.Errorf("the shared key = %+v, %v; want it for api only", info, ok)
	}
	if info, ok, _ := s.LookupToken(ctx, admin); !ok || !info.MayUse("web") {
		t.Error("an admin token limited to the app was revoked")
	}

	// Registered again, the name brings back none of the old keys.
	if err := s.Register(ctx, Registration{Name: "web", Repo: "r/web", Domain: "web.example.com"}); err != nil {
		t.Fatal(err)
	}
	if info, ok, _ := s.LookupToken(ctx, both); !ok || info.MayUse("web") {
		t.Error("the shared key came back for the app registered again")
	}
}

func TestRotateKey(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, app := range []string{"web", "api"} {
		_ = s.Register(ctx, Registration{Name: app, Repo: "r/" + app, Domain: app + ".example.com"})
	}
	old, _ := s.CreateScopedToken(ctx, "web-ci", ScopeDeploy, time.Time{}, "web")
	soon := time.Now().Add(10 * time.Minute)
	expiring, _ := s.CreateScopedToken(ctx, "web-ci-short", ScopeDeploy, soon, "web")
	shared, _ := s.CreateScopedToken(ctx, "shared", ScopeDeploy, time.Time{}, "web", "api")
	pr, _ := s.CreateScopedToken(ctx, "web-pr", ScopePreview, time.Time{}, "web")

	// Keeping the old ones for an hour: they get an end, the one that
	// ends sooner keeps its own, the shared one loses the app at once,
	// and keys of another scope are not touched.
	fresh, c, err := s.RotateKey(ctx, "web", ScopeDeploy, "web-ci-2", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Expiring, []string{"web-ci", "web-ci-short"}) || !slices.Equal(c.Detached, []string{"shared"}) || len(c.Revoked) != 0 {
		t.Errorf("RotateKey() = %+v", c)
	}
	if info, ok, _ := s.LookupToken(ctx, old); !ok || info.ExpiresAt.Before(time.Now().Add(59*time.Minute)) || info.ExpiresAt.After(time.Now().Add(61*time.Minute)) {
		t.Errorf("the old key = %+v, %v; want it to work for another hour", info, ok)
	}
	if info, ok, _ := s.LookupToken(ctx, expiring); !ok || info.ExpiresAt.After(soon.Add(time.Second)) {
		t.Errorf("the key that ended sooner = %+v; want its own end kept", info)
	}
	if info, _, _ := s.LookupToken(ctx, shared); info.MayUse("web") || !info.MayUse("api") {
		t.Errorf("the shared key = %+v; want it for api only", info)
	}
	if info, ok, _ := s.LookupToken(ctx, fresh); !ok || info.Scope != ScopeDeploy || !slices.Equal(info.Apps, []string{"web"}) || !info.ExpiresAt.IsZero() {
		t.Errorf("the new key = %+v, %v", info, ok)
	}
	if _, ok, _ := s.LookupToken(ctx, pr); !ok {
		t.Error("rotating the deploy key touched the preview key")
	}

	// Without keeping: the old ones (the new one of before among them)
	// are gone at once.
	if _, c, err = s.RotateKey(ctx, "web", ScopeDeploy, "web-ci-3", 0); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Revoked, []string{"web-ci", "web-ci-2", "web-ci-short"}) {
		t.Errorf("RotateKey() = %+v", c)
	}
	if _, ok, _ := s.LookupToken(ctx, fresh); ok {
		t.Error("the replaced key still works")
	}

	// Kept for a while, the old key keeps its name: the new one needs
	// another. Refused as a whole, nothing changes.
	if _, _, err := s.RotateKey(ctx, "web", ScopeDeploy, "web-ci-3", time.Hour); !errors.Is(err, ErrExists) {
		t.Errorf("RotateKey() with a taken name = %v", err)
	}
	if list, _ := s.Tokens(ctx); !slices.ContainsFunc(list, func(i TokenInfo) bool { return i.Name == "web-ci-3" && i.ExpiresAt.IsZero() }) {
		t.Error("a refused rotation changed the key it would have replaced")
	}
	if _, _, err := s.RotateKey(ctx, "nope", ScopeDeploy, "nope-ci", 0); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("RotateKey() for an app that is not registered = %v", err)
	}
}

func TestDeployKeysNameTheirApps(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateScopedToken(ctx, "ci", ScopeDeploy, time.Time{}); err == nil {
		t.Error("a deploy key for every app was created")
	}
	if _, err := s.CreateScopedToken(ctx, "ci", ScopeDeploy, time.Time{}, "web"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("a deploy key for an app that is not registered: %v", err)
	}
	if err := s.Register(ctx, Registration{Name: "web", Repo: "web", Domain: "web.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateScopedToken(ctx, "pr", ScopePreview, time.Time{}); err == nil {
		t.Error("a preview key for every app was created")
	}
	if _, err := s.CreateScopedToken(ctx, "pr", ScopePreview, time.Time{}, "api"); err == nil {
		t.Error("a preview key for an app that is not registered was created")
	}
	if _, err := s.CreateScopedToken(ctx, "pr", ScopePreview, time.Time{}, "web"); err != nil {
		t.Errorf("preview key: %v", err)
	}
	tok, err := s.CreateScopedToken(ctx, "ci", ScopeDeploy, time.Time{}, "web")
	if err != nil {
		t.Fatal(err)
	}
	if info, ok, err := s.LookupToken(ctx, tok); err != nil || !ok || info.Scope != ScopeDeploy || !info.MayUse("web") || info.MayUse("api") {
		t.Errorf("LookupToken() = %+v, %v, %v", info, ok, err)
	}
}

func TestResealSecretsIsAllOrNothing(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, n := range []string{"A", "B", "C"} {
		if err := s.PutSecret(ctx, "web", n, []byte("old-"+n)); err != nil {
			t.Fatal(err)
		}
	}

	// Fails at the second value: the first one must not stay changed.
	_, err := s.ResealSecrets(ctx, func(_, name string, _ []byte) ([]byte, bool, error) {
		if name == "B" {
			return nil, false, errors.New("cannot open B")
		}
		return []byte("new-" + name), true, nil
	})
	if err == nil {
		t.Fatal("ResealSecrets() error = nil, want the failure")
	}
	for _, n := range []string{"A", "B", "C"} {
		if got, _, _ := s.GetSecret(ctx, "web", n); string(got) != "old-"+n {
			t.Errorf("%s = %q after a failed reseal, want it unchanged", n, got)
		}
	}

	// Succeeds, leaving C alone.
	n, err := s.ResealSecrets(ctx, func(_, name string, sealed []byte) ([]byte, bool, error) {
		if name == "C" {
			return sealed, false, nil
		}
		return []byte("new-" + name), true, nil
	})
	if err != nil || n != 2 {
		t.Fatalf("ResealSecrets() = %d, %v; want 2", n, err)
	}
	for n, want := range map[string]string{"A": "new-A", "B": "new-B", "C": "old-C"} {
		if got, _, _ := s.GetSecret(ctx, "web", n); string(got) != want {
			t.Errorf("%s = %q, want %q", n, got, want)
		}
	}
}

func TestOpenCurrentDoesNotUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "liman.db")

	// Nothing there yet: 0, and a new database is set up by either.
	if v, err := SchemaVersionOf(ctx, path); err != nil || v != 0 {
		t.Fatalf("SchemaVersionOf(missing) = %d, %v", v, err)
	}
	s, err := OpenCurrent(ctx, path)
	if err != nil {
		t.Fatalf("OpenCurrent() of a new file = %v", err)
	}
	_ = s.Close()
	if v, _ := SchemaVersionOf(ctx, path); v != SchemaVersion() {
		t.Fatalf("schema after OpenCurrent of a new file = %d, want %d", v, SchemaVersion())
	}

	// A database an older version left behind.
	old := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+old)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	_, err = OpenCurrent(ctx, old)
	var oldErr *OldSchemaError
	if !errors.As(err, &oldErr) || oldErr.Have != 3 || oldErr.Want != SchemaVersion() {
		t.Fatalf("OpenCurrent() of an old database = %v, want an OldSchemaError 3 -> %d", err, SchemaVersion())
	}
	if v, _ := SchemaVersionOf(ctx, old); v != 3 {
		t.Errorf("OpenCurrent changed the schema to %d", v)
	}
	s, err = Open(ctx, old)
	if err != nil {
		t.Fatalf("Open() of an old database = %v", err)
	}
	_ = s.Close()
	if v, _ := SchemaVersionOf(ctx, old); v != SchemaVersion() {
		t.Errorf("schema after Open = %d, want %d", v, SchemaVersion())
	}
}
