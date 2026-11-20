package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Snapshot writes a complete, consistent copy of the database to dest. It
// can be taken while the daemon is running and writing.
//
// Copying liman.db with cp is not a backup. The database is three files
// (liman.db, liman.db-wal, liman.db-shm) that change while they are being
// read: recent writes live only in the -wal file, and a copy made between
// two of them can be a file that never existed, or one that is corrupt.
// VACUUM INTO asks SQLite itself for the copy: it reads the database inside
// one transaction, so the result is the state at a single moment, in a
// single file.
//
// dest must not exist; nothing is ever overwritten.
func (s *Store) Snapshot(ctx context.Context, dest string) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists; a backup never overwrites a file", dest)
	}

	// SQLite creates its output with the default permissions, readable by
	// everyone. The copy is therefore written into a private directory
	// first and only moved to its place once it is private itself: the
	// tokens' hashes and the sealed secrets are never exposed, not even
	// briefly. MkdirTemp creates the directory with mode 0700.
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".liman-snapshot-*")
	if err != nil {
		return fmt.Errorf("prepare backup: %w", err)
	}
	// Whatever happens below, the temporary directory does not stay.
	defer func() { _ = os.RemoveAll(dir) }()

	tmp := filepath.Join(dir, "snapshot.db")
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("copy database: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("protect backup: %w", err)
	}
	// Ask the operating system to put the bytes on the disk before the file
	// gets its final name. Without this a power cut shortly afterwards
	// could leave a correctly named, empty file.
	if err := syncFile(tmp); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}

	// Link gives the file its second, final name and fails if that name is
	// taken; Rename would silently replace whatever is there. The first
	// name disappears with the temporary directory.
	if err := os.Link(tmp, dest); err != nil {
		return fmt.Errorf("place backup: %w", err)
	}
	return nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// OpenReadOnly opens a database file without changing it: no migration, no
// permission fix, no write of any kind. It is how a backup is inspected.
//
// The schema of the file may be older than this version's, so only the
// methods documented as safe for it should be used on the result: Inspect,
// SecretRefs, GetSecret and Tokens.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		// Checked here because SQLite would otherwise report a missing file
		// only at the first query, in its own words.
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// mode=ro makes SQLite refuse every write on this connection.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%s is not a liman database: %w", path, err)
	}
	if version < 1 {
		_ = db.Close()
		return nil, fmt.Errorf("%s is not a liman database", path)
	}
	if version > len(migrations) {
		_ = db.Close()
		return nil, fmt.Errorf("%s has schema version %d, newer than this liman understands (%d); use the liman version that wrote it", path, version, len(migrations))
	}
	return s, nil
}

// Summary describes the content of a database.
type Summary struct {
	// SchemaVersion is how many migrations the file has seen.
	SchemaVersion int
	// Rows counts the rows of each table that exists in the file.
	Rows map[string]int
}

// Inspect checks the database for corruption and counts what is in it.
func (s *Store) Inspect(ctx context.Context) (Summary, error) {
	// integrity_check reads every page and verifies that the tables, their
	// indexes and the links between pages agree. One row saying "ok" means
	// they do; anything else is a list of problems.
	rows, err := s.db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return Summary{}, fmt.Errorf("check database: %w", err)
	}
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			_ = rows.Close()
			return Summary{}, fmt.Errorf("check database: %w", err)
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return Summary{}, fmt.Errorf("check database: %w", err)
	}
	if len(problems) > 0 {
		return Summary{}, fmt.Errorf("the database is damaged: %s", problems[0])
	}

	sum := Summary{Rows: map[string]int{}}
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&sum.SchemaVersion); err != nil {
		return Summary{}, fmt.Errorf("read schema version: %w", err)
	}

	// A file written by an older version has fewer tables. Only the ones
	// it has are counted; the list is ours, so the name can go into the
	// query text (a table name cannot be a "?" parameter).
	for _, table := range []string{"events", "releases", "tokens", "secrets", "health_samples"} {
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&exists)
		if err != nil {
			return Summary{}, fmt.Errorf("read schema: %w", err)
		}
		if exists == 0 {
			continue
		}
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			return Summary{}, fmt.Errorf("count %s: %w", table, err)
		}
		sum.Rows[table] = n
	}
	return sum, nil
}

// SecretRef names one stored secret.
type SecretRef struct {
	App  string
	Name string
}

// SecretRefs lists every stored secret of every app, by name only. It
// returns nothing for a file that predates secrets.
func (s *Store) SecretRefs(ctx context.Context) ([]SecretRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app, name FROM secrets ORDER BY app, name`)
	if err != nil {
		var exists int
		if s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'secrets'`).Scan(&exists) == nil && exists == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var refs []SecretRef
	for rows.Next() {
		var ref SecretRef
		if err := rows.Scan(&ref.App, &ref.Name); err != nil {
			return nil, fmt.Errorf("list secrets: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	return refs, nil
}

// Checkpoint moves everything from the write-ahead log into the main
// database file, so that liman.db on its own is complete. Close does not
// promise that; a restore that is about to move the file aside needs it.
func (s *Store) Checkpoint(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("checkpoint database: %w", err)
	}
	return nil
}
