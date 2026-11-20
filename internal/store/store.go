// Package store keeps liman's durable state in a SQLite database: the
// history of executed steps, the releases of every app and the API tokens.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	// The blank import registers the "sqlite" driver with database/sql.
	// Nothing from the package is called directly: database/sql is the
	// API, the driver is the implementation behind it. This driver is
	// written in pure Go, so the binary still cross-compiles to Linux
	// without a C toolchain.
	_ "modernc.org/sqlite"
)

// Store is a handle to the database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// migrations are applied in order, each exactly once. A released migration
// is never edited; schema changes are made by appending a new one. The
// database remembers how many have been applied (PRAGMA user_version).
var migrations = []string{
	// 1: initial schema.
	`
	CREATE TABLE events (
		id          INTEGER PRIMARY KEY,
		time        TEXT    NOT NULL,
		app         TEXT    NOT NULL,
		release     TEXT    NOT NULL,
		step        TEXT    NOT NULL,
		action      TEXT    NOT NULL,
		ok          INTEGER NOT NULL,
		error       TEXT    NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL
	);
	CREATE INDEX events_app ON events (app, id);

	CREATE TABLE releases (
		id          INTEGER PRIMARY KEY,
		app         TEXT NOT NULL,
		release     TEXT NOT NULL,
		image       TEXT NOT NULL,
		status      TEXT NOT NULL,
		error       TEXT NOT NULL DEFAULT '',
		started_at  TEXT NOT NULL,
		finished_at TEXT NOT NULL,
		UNIQUE (app, release)
	);

	CREATE TABLE tokens (
		name       TEXT PRIMARY KEY,
		hash       TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	`,

	// 2: secrets. The value column holds ciphertext only; the store never
	// sees a plaintext secret and has no key to make one.
	`
	CREATE TABLE secrets (
		app        TEXT NOT NULL,
		name       TEXT NOT NULL,
		value      BLOB NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (app, name)
	);
	`,

	// 3: health samples, one row per health check of an app. Unlike the
	// other tables the time is a Unix timestamp: this table is only ever
	// read as "everything since T, grouped into buckets", and that is
	// integer arithmetic instead of string parsing. duration_ms is NULL for
	// a check that made no HTTP request.
	`
	CREATE TABLE health_samples (
		id          INTEGER PRIMARY KEY,
		at          INTEGER NOT NULL,
		app         TEXT    NOT NULL,
		ok          INTEGER NOT NULL,
		duration_ms INTEGER
	);
	CREATE INDEX health_samples_app ON health_samples (app, at);
	CREATE INDEX health_samples_at ON health_samples (at);
	`,

	// 4: token scopes. Every token that already exists keeps the power it
	// had: before this migration there was only one kind.
	`
	ALTER TABLE tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'admin';
	`,

	// 5: token expiry. NULL means the token never expires, which is what
	// every token that already exists keeps: an upgrade must not lock
	// anybody out.
	`
	ALTER TABLE tokens ADD COLUMN expires_at TEXT;
	`,

	// 6: when a token was last used, to within a minute (see TouchToken).
	// NULL means not since this column exists: a token that is older than
	// the upgrade may well have been used before it.
	`
	ALTER TABLE tokens ADD COLUMN last_used_at TEXT;
	`,

	// 7: the apps a token is limited to. A token without rows here may use
	// every app, which is what every token that already exists keeps. The
	// rows go with their token when it is revoked.
	`
	CREATE TABLE token_apps (
		token TEXT NOT NULL REFERENCES tokens (name) ON DELETE CASCADE,
		app   TEXT NOT NULL,
		PRIMARY KEY (token, app)
	);
	`,

	// 8: the audit trail: who changed what, when, and how it ended. Rows
	// are only ever added. Each one's mac covers the previous one's (see
	// package audit), so a row changed or removed later shows. app is ""
	// for what concerns the daemon as a whole (tokens, backups, keys).
	`
	CREATE TABLE audit (
		seq    INTEGER PRIMARY KEY,
		at     TEXT    NOT NULL,
		actor  TEXT    NOT NULL,
		action TEXT    NOT NULL,
		app    TEXT    NOT NULL DEFAULT '',
		target TEXT    NOT NULL DEFAULT '',
		ok     INTEGER NOT NULL,
		error  TEXT    NOT NULL DEFAULT '',
		key_id TEXT    NOT NULL DEFAULT '',
		mac    TEXT    NOT NULL
	);
	CREATE INDEX audit_app ON audit (app, seq);
	`,

	// 9: the app definition of the last successful deploy made by hand. A
	// deploy triggered from a pipeline runs it again with another image;
	// stopping the app removes it, so a stopped app is not brought back by
	// the next push.
	`
	CREATE TABLE app_definitions (
		app        TEXT PRIMARY KEY,
		definition BLOB NOT NULL,
		updated_at TEXT NOT NULL
	);
	`,

	// 10: apps are registered, and a pipeline sends the whole definition
	// with every deploy; nothing is deployed by hand first. A registration
	// fixes what a deploy key cannot change: the image's repository and
	// the domain. app_state remembers what the last triggered deploy ran,
	// as checksums only: the values (secrets among them) are not kept.
	`
	DROP TABLE app_definitions;
	CREATE TABLE apps (
		name       TEXT PRIMARY KEY,
		repo       TEXT NOT NULL,
		domain     TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL
	);
	CREATE TABLE app_state (
		app        TEXT PRIMARY KEY,
		release    TEXT NOT NULL,
		image      TEXT NOT NULL,
		checksum   TEXT NOT NULL,
		fields     TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	`,
}

// SchemaVersion is the schema version this build reads and writes.
func SchemaVersion() int { return len(migrations) }

// OldSchemaError is returned by OpenCurrent for a database an older
// version of liman wrote.
type OldSchemaError struct {
	Path       string
	Have, Want int
}

func (e *OldSchemaError) Error() string {
	return fmt.Sprintf("%s has schema version %d and this limand uses %d; start the daemon once (it backs the database up first, then upgrades it), then run this again", e.Path, e.Have, e.Want)
}

// SchemaVersionOf returns the schema version of the database at path
// without changing anything: 0 for a file that does not exist yet.
func SchemaVersionOf(ctx context.Context, path string) (int, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	st, err := OpenReadOnly(ctx, path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = st.Close() }()
	var version int
	if err := st.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

// Open opens the database at path, creating it and its directory if
// needed, and brings the schema up to date, whatever version wrote it.
//
// Upgrading a database is a step back versions cannot undo: an older liman
// refuses a newer schema. Only the daemon calls Open on a database that may
// be old, and it backs it up first. Everything else uses OpenCurrent.
func Open(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, true)
}

// OpenCurrent is Open for everything but the daemon's start: a new, empty
// database is set up, but one an older version wrote is not upgraded; that
// returns an *OldSchemaError.
func OpenCurrent(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, false)
}

func open(ctx context.Context, path string, upgrade bool) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	// Create the file ourselves before SQLite does, so that it is private
	// from its first moment. SQLite gives its companion files (the -wal and
	// -shm files) the same permissions as the database.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("create database %s: %w", path, err)
	}
	// O_CREATE only applies the mode to a new file; tighten an existing one.
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("protect database %s: %w", path, err)
	}

	// The part after "?" configures every connection:
	//   busy_timeout  wait up to 5s for another process's lock instead of
	//                 failing at once (the daemon and "limand token" may
	//                 touch the file at the same time)
	//   journal_mode  WAL lets readers proceed while a write is in progress
	//   foreign_keys  off by default in SQLite, for historical reasons
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

	// sql.Open only validates its arguments. sql.DB is a pool of
	// connections that are opened lazily, which is why the Ping follows.
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// SQLite allows one writer at a time. A single connection makes
	// database/sql queue our own goroutines instead of having them collide
	// on SQLite's lock.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx, path, upgrade); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context, path string, upgrade bool) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this liman understands (%d)", version, len(migrations))
	}
	// Version 0 is a database that was just created: nothing in it to
	// lose, so it is set up whoever opens it.
	if version > 0 && version < len(migrations) && !upgrade {
		return &OldSchemaError{Path: path, Have: version, Want: len(migrations)}
	}

	for i := version; i < len(migrations); i++ {
		// A transaction makes each migration all-or-nothing: if any
		// statement fails, the database is left exactly as it was.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", i+1, err)
		}

		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
		// PRAGMA does not accept placeholders; the value is our own loop
		// counter, never user input.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", i+1, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}

// Event is one executed step.
type Event struct {
	Time    time.Time `json:"time"`
	App     string    `json:"app"`
	Release string    `json:"release"`
	Step    string    `json:"step"`
	// Action is "run" or "undo".
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	// DurationMS is how long the step took, in milliseconds.
	DurationMS int64 `json:"duration_ms"`
}

// AppendEvent records an executed step.
func (s *Store) AppendEvent(ctx context.Context, ev Event) error {
	// The "?" placeholders keep values out of the SQL text. The driver
	// sends them separately, so no value can ever be parsed as SQL: this
	// is what prevents SQL injection.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (time, app, release, step, action, ok, error, duration_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		formatTime(ev.Time), ev.App, ev.Release, ev.Step, ev.Action, ev.OK, ev.Error, ev.DurationMS)
	if err != nil {
		return fmt.Errorf("record event: %w", err)
	}
	return nil
}

// Events returns the newest limit events, oldest first. An empty app
// returns events of all apps.
func (s *Store) Events(ctx context.Context, app string, limit int) ([]Event, error) {
	return s.EventsOf(ctx, oneApp(app), limit)
}

// EventsOf is Events for a set of apps; nil means every app, and an empty
// non-nil set no app at all.
func (s *Store) EventsOf(ctx context.Context, apps []string, limit int) ([]Event, error) {
	filter, err := appFilter(apps)
	if err != nil {
		return nil, err
	}
	// The inner query picks the newest rows; the outer one puts them back
	// in chronological order. The filter is one parameter, a JSON array
	// (or NULL for every app), so the SQL text never changes with it.
	rows, err := s.db.QueryContext(ctx,
		`SELECT time, app, release, step, action, ok, error, duration_ms FROM (
			SELECT * FROM events WHERE (? IS NULL OR app IN (SELECT value FROM json_each(?))) ORDER BY id DESC LIMIT ?
		 ) ORDER BY id ASC`,
		filter, filter, limit)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	// Rows hold a database connection until closed. With a pool of one,
	// forgetting this would block every later query.
	defer func() { _ = rows.Close() }()

	var events []Event
	for rows.Next() {
		var ev Event
		var at string
		// Scan copies the columns of the current row into the pointers, in
		// SELECT order.
		if err := rows.Scan(&at, &ev.App, &ev.Release, &ev.Step, &ev.Action, &ev.OK, &ev.Error, &ev.DurationMS); err != nil {
			return nil, fmt.Errorf("read event: %w", err)
		}
		if ev.Time, err = parseTime(at); err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	// The loop also ends when reading fails midway; Err tells the two
	// cases apart.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	return events, nil
}

// Release statuses.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Release is one deploy of an app and how it ended.
type Release struct {
	App        string    `json:"app"`
	Release    string    `json:"release"`
	Image      string    `json:"image"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// RecordRelease stores the outcome of a deploy.
func (s *Store) RecordRelease(ctx context.Context, r Release) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO releases (app, release, image, status, error, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.App, r.Release, r.Image, r.Status, r.Error, formatTime(r.StartedAt), formatTime(r.FinishedAt))
	if err != nil {
		return fmt.Errorf("record release: %w", err)
	}
	return nil
}

// Releases returns the newest limit releases, newest first. An empty app
// returns releases of all apps.
func (s *Store) Releases(ctx context.Context, app string, limit int) ([]Release, error) {
	return s.ReleasesOf(ctx, oneApp(app), limit)
}

// ReleasesOf is Releases for a set of apps, as in EventsOf.
func (s *Store) ReleasesOf(ctx context.Context, apps []string, limit int) ([]Release, error) {
	filter, err := appFilter(apps)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT app, release, image, status, error, started_at, finished_at
		 FROM releases WHERE (? IS NULL OR app IN (SELECT value FROM json_each(?))) ORDER BY id DESC LIMIT ?`,
		filter, filter, limit)
	if err != nil {
		return nil, fmt.Errorf("read releases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var releases []Release
	for rows.Next() {
		var r Release
		var started, finished string
		if err := rows.Scan(&r.App, &r.Release, &r.Image, &r.Status, &r.Error, &started, &finished); err != nil {
			return nil, fmt.Errorf("read release: %w", err)
		}
		if r.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if r.FinishedAt, err = parseTime(finished); err != nil {
			return nil, err
		}
		releases = append(releases, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read releases: %w", err)
	}
	return releases, nil
}

// ErrExists is returned when a record with the same key already exists.
var ErrExists = errors.New("already exists")

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// Times are stored as text in a fixed UTC format. Fixed-width UTC strings
// sort in chronological order, and remain readable in the sqlite3 shell.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// oneApp turns the single-app filter of Events and Releases into a set:
// "" is every app.
func oneApp(app string) []string {
	if app == "" {
		return nil
	}
	return []string{app}
}

// appFilter encodes a set of apps for "app IN (SELECT value FROM
// json_each(?))": nil (every app) becomes NULL, anything else a JSON array,
// so an empty set matches nothing rather than everything.
func appFilter(apps []string) (any, error) {
	if apps == nil {
		return nil, nil
	}
	b, err := json.Marshal(apps)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("stored time %q is invalid: %w", s, err)
	}
	return t, nil
}
