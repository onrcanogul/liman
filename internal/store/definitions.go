package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SaveDefinition keeps definition (app.yaml as deployed) as the one app's
// triggered deploys start from, replacing the one before.
func (s *Store) SaveDefinition(ctx context.Context, app string, definition []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO app_definitions (app, definition, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (app) DO UPDATE SET definition = excluded.definition, updated_at = excluded.updated_at`,
		app, definition, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("save definition of %s: %w", app, err)
	}
	return nil
}

// Definition returns the definition saved for app; found is false if there
// is none.
func (s *Store) Definition(ctx context.Context, app string) (definition []byte, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT definition FROM app_definitions WHERE app = ?`, app).Scan(&definition)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read definition of %s: %w", app, err)
	}
	return definition, true, nil
}

// DeleteDefinition removes app's definition; there being none is not an
// error.
func (s *Store) DeleteDefinition(ctx context.Context, app string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM app_definitions WHERE app = ?`, app); err != nil {
		return fmt.Errorf("delete definition of %s: %w", app, err)
	}
	return nil
}
