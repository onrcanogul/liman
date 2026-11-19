package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/onrcanogul/liman/internal/secrets"
)

// The methods below implement secrets.Storage. They store and return sealed
// values as opaque bytes; encryption happens in package secrets, before the
// data gets here.
var _ secrets.Storage = (*Store)(nil)

// PutSecret stores a sealed value, replacing an earlier one.
func (s *Store) PutSecret(ctx context.Context, app, name string, sealed []byte) error {
	// "Upsert": insert, or update the existing row if the primary key is
	// already taken. One statement, so there is no window between a check
	// and a write in which another request could slip in.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets (app, name, value, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (app, name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		app, name, sealed, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("store secret: %w", err)
	}
	return nil
}

// GetSecret returns a sealed value. found is false if it does not exist.
func (s *Store) GetSecret(ctx context.Context, app, name string) (sealed []byte, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT value FROM secrets WHERE app = ? AND name = ?`, app, name).Scan(&sealed)
	// QueryRow reports "no such row" as an error value of its own, so that
	// it can be told apart from a real failure.
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read secret: %w", err)
	}
	return sealed, true, nil
}

// ListSecrets returns the names of the secrets of app, sorted.
func (s *Store) ListSecrets(ctx context.Context, app string) ([]secrets.Info, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, updated_at FROM secrets WHERE app = ? ORDER BY name`, app)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []secrets.Info
	for rows.Next() {
		var info secrets.Info
		var updated string
		if err := rows.Scan(&info.Name, &updated); err != nil {
			return nil, fmt.Errorf("list secrets: %w", err)
		}
		if info.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	return out, nil
}

// DeleteSecret removes a secret. found is false if it did not exist.
func (s *Store) DeleteSecret(ctx context.Context, app, name string) (found bool, err error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE app = ? AND name = ?`, app, name)
	if err != nil {
		return false, fmt.Errorf("delete secret: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountSecrets returns how many secrets are stored, for all apps.
func (s *Store) CountSecrets(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count secrets: %w", err)
	}
	return n, nil
}
