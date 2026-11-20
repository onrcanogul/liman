package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// AuditEntry is one row of the audit trail: who changed what, when, and
// how it ended. Package audit decides what goes in and signs it; the store
// keeps the rows in order and never changes one.
type AuditEntry struct {
	// Seq numbers the rows from 1, without gaps.
	Seq   int64
	At    time.Time
	Actor string
	// Action is what was done ("deploy", "secret.set", "token.revoke").
	Action string
	// App is the app it concerned; "" for the daemon as a whole.
	App string
	// Target is what within the app or the daemon: a secret's name, a
	// release, a token's name, a backup file.
	Target string
	OK     bool
	Error  string
	// KeyID names the master key the row was signed with; "" when there
	// was none.
	KeyID string
	MAC   []byte
}

// AuditSigner computes the signature of e, which follows the row whose
// mac is prev (nil for the first row).
type AuditSigner func(prev []byte, e AuditEntry) (keyID string, mac []byte, err error)

// AppendAudit adds e to the end of the trail, with the next sequence
// number and the signature sign gives it, and returns it as stored.
//
// Reading the last row and adding the next must happen as one step: two
// appends that both read the same last row would sign the same
// predecessor, and the trail would be broken by nobody. The daemon is not
// the only writer either (limand commands on the server open the database
// too), so a lock in this process would not do. BEGIN IMMEDIATE takes
// SQLite's write lock before the read; another writer, in any process,
// waits for it (busy_timeout) instead of reading the same row.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry, sign AuditSigner) (AuditEntry, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("append audit entry: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return AuditEntry{}, fmt.Errorf("append audit entry: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	var last int64
	var prevHex string
	var prev []byte
	err = conn.QueryRowContext(ctx, `SELECT seq, mac FROM audit ORDER BY seq DESC LIMIT 1`).Scan(&last, &prevHex)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return AuditEntry{}, fmt.Errorf("append audit entry: %w", err)
	default:
		if prev, err = hex.DecodeString(prevHex); err != nil {
			return AuditEntry{}, fmt.Errorf("append audit entry: stored mac of %d: %w", last, err)
		}
	}

	e.Seq = last + 1
	// The time is signed as stored: in UTC, to the nanosecond, so that
	// reading it back gives the same bytes to check.
	if e.At, err = parseTime(formatTime(e.At)); err != nil {
		return AuditEntry{}, err
	}
	if e.KeyID, e.MAC, err = sign(prev, e); err != nil {
		return AuditEntry{}, fmt.Errorf("sign audit entry: %w", err)
	}
	_, err = conn.ExecContext(ctx,
		`INSERT INTO audit (seq, at, actor, action, app, target, ok, error, key_id, mac) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Seq, formatTime(e.At), e.Actor, e.Action, e.App, e.Target, e.OK, e.Error, e.KeyID, hex.EncodeToString(e.MAC))
	if err != nil {
		return AuditEntry{}, fmt.Errorf("append audit entry: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return AuditEntry{}, fmt.Errorf("append audit entry: %w", err)
	}
	done = true
	return e, nil
}

const auditColumns = `seq, at, actor, action, app, target, ok, error, key_id, mac`

// AuditEntries returns the newest limit rows of the apps in apps, oldest
// first. nil means everything, rows about the daemon as a whole included;
// a set means those apps' rows only.
func (s *Store) AuditEntries(ctx context.Context, apps []string, limit int) ([]AuditEntry, error) {
	filter, err := appFilter(apps)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+auditColumns+` FROM (
			SELECT * FROM audit WHERE (? IS NULL OR app IN (SELECT value FROM json_each(?))) ORDER BY seq DESC LIMIT ?
		 ) ORDER BY seq ASC`,
		filter, filter, limit)
	if err != nil {
		return nil, fmt.Errorf("read audit trail: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AuditEntry
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read audit trail: %w", err)
	}
	return out, nil
}

// EachAudit calls fn for every row, in order, until fn returns an error.
// The rows are read one at a time, so a long trail is never held in
// memory.
func (s *Store) EachAudit(ctx context.Context, fn func(AuditEntry) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT `+auditColumns+` FROM audit ORDER BY seq ASC`)
	if err != nil {
		return fmt.Errorf("read audit trail: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read audit trail: %w", err)
	}
	return nil
}

func scanAudit(rows *sql.Rows) (AuditEntry, error) {
	var e AuditEntry
	var at, mac string
	if err := rows.Scan(&e.Seq, &at, &e.Actor, &e.Action, &e.App, &e.Target, &e.OK, &e.Error, &e.KeyID, &mac); err != nil {
		return AuditEntry{}, fmt.Errorf("read audit entry: %w", err)
	}
	var err error
	if e.At, err = parseTime(at); err != nil {
		return AuditEntry{}, err
	}
	// A mac that does not decode is kept as it is not: empty. Verifying
	// then reports the row as broken, which it is.
	e.MAC, _ = hex.DecodeString(mac)
	return e, nil
}

// AuditKeyCounts returns how many rows each master key signed, by key ID;
// unsigned rows are counted under "".
func (s *Store) AuditKeyCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key_id, COUNT(*) FROM audit GROUP BY key_id`)
	if err != nil {
		return nil, fmt.Errorf("read audit trail: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("read audit trail: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}
