// Package store keeps liman's durable state in a SQLite database: the
// history of executed steps, the releases of every app and the API tokens.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
}

// Open opens the database at path, creating it and its directory if
// needed, and brings the schema up to date.
func Open(ctx context.Context, path string) (*Store, error) {
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
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this liman understands (%d)", version, len(migrations))
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
	// The inner query picks the newest rows; the outer one puts them back
	// in chronological order. "? = ''" makes the filter optional without
	// building the SQL text dynamically.
	rows, err := s.db.QueryContext(ctx,
		`SELECT time, app, release, step, action, ok, error, duration_ms FROM (
			SELECT * FROM events WHERE (? = '' OR app = ?) ORDER BY id DESC LIMIT ?
		 ) ORDER BY id ASC`,
		app, app, limit)
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

// Releases returns the releases of app, newest first.
func (s *Store) Releases(ctx context.Context, app string) ([]Release, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT app, release, image, status, error, started_at, finished_at
		 FROM releases WHERE app = ? ORDER BY id DESC`, app)
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
