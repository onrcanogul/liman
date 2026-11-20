package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Registration is an app as registered on the server: what a deploy key
// may not change.
type Registration struct {
	Name string
	// Repo is the image's repository, without a tag or a digest.
	Repo      string
	Domain    string
	CreatedAt time.Time
}

// ErrDomainTaken is returned when a domain is registered to another app.
var ErrDomainTaken = errors.New("domain is registered to another app")

// Register stores a new registration. A name that is registered already
// is ErrExists; a domain registered to another app is ErrDomainTaken.
func (s *Store) Register(ctx context.Context, r Registration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("register %s: %w", r.Name, err)
	}
	defer func() { _ = tx.Rollback() }()
	// The name is looked at first: with both taken, "already registered"
	// is the answer that helps.
	var one int
	switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM apps WHERE name = ?`, r.Name).Scan(&one); {
	case err == nil:
		return fmt.Errorf("app %s: %w", r.Name, ErrExists)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("register %s: %w", r.Name, err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO apps (name, repo, domain, created_at) VALUES (?, ?, ?, ?)`,
		r.Name, r.Repo, r.Domain, formatTime(time.Now()))
	// SQLite names the column in a constraint failure; there is no error
	// type to look at.
	if err != nil && strings.Contains(err.Error(), "apps.domain") {
		return fmt.Errorf("%s: %w", r.Domain, ErrDomainTaken)
	}
	if err != nil {
		return fmt.Errorf("register %s: %w", r.Name, err)
	}
	return tx.Commit()
}

// Registration returns the registration of app; found is false if it is
// not registered.
func (s *Store) Registration(ctx context.Context, app string) (r Registration, found bool, err error) {
	var created string
	err = s.db.QueryRowContext(ctx, `SELECT name, repo, domain, created_at FROM apps WHERE name = ?`, app).
		Scan(&r.Name, &r.Repo, &r.Domain, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Registration{}, false, nil
	}
	if err != nil {
		return Registration{}, false, fmt.Errorf("read registration of %s: %w", app, err)
	}
	r.CreatedAt, err = parseTime(created)
	return r, err == nil, err
}

// DomainOwner returns the app the domain is registered to, "" for none.
func (s *Store) DomainOwner(ctx context.Context, domain string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM apps WHERE domain = ?`, domain).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("look up domain %s: %w", domain, err)
	}
	return name, nil
}

// Registrations returns every registration, by name.
func (s *Store) Registrations(ctx context.Context) ([]Registration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, repo, domain, created_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list registrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Registration
	for rows.Next() {
		var r Registration
		var created string
		if err := rows.Scan(&r.Name, &r.Repo, &r.Domain, &created); err != nil {
			return nil, fmt.Errorf("list registrations: %w", err)
		}
		if r.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Unregister removes app's registration and what its last triggered
// deploy left. Its deploy keys stay until revoked; without a registration
// they are refused.
func (s *Store) Unregister(ctx context.Context, app string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("unregister %s: %w", app, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM apps WHERE name = ?`, app)
	if err != nil {
		return fmt.Errorf("unregister %s: %w", app, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("app %s: %w", app, ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_state WHERE app = ?`, app); err != nil {
		return fmt.Errorf("unregister %s: %w", app, err)
	}
	return tx.Commit()
}

// DeployState is what the last deploy triggered from a pipeline ran, as
// long as nothing else has changed the app since.
type DeployState struct {
	App     string
	Release string
	// Image is the image deployed, repo@digest.
	Image string
	// Checksum is the HMAC of the whole definition, Fields that of each
	// value by its path ("env.LOG_LEVEL"): enough to tell whether and
	// where a definition changed, without keeping any value.
	Checksum  string
	Fields    map[string]string
	UpdatedAt time.Time
}

// SaveDeployState replaces app's deploy state.
func (s *Store) SaveDeployState(ctx context.Context, st DeployState) error {
	fields, err := json.Marshal(st.Fields)
	if err != nil {
		return fmt.Errorf("save deploy state of %s: %w", st.App, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO app_state (app, release, image, checksum, fields, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (app) DO UPDATE SET release = excluded.release, image = excluded.image,
		   checksum = excluded.checksum, fields = excluded.fields, updated_at = excluded.updated_at`,
		st.App, st.Release, st.Image, st.Checksum, string(fields), formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("save deploy state of %s: %w", st.App, err)
	}
	return nil
}

// DeployStateOf returns app's deploy state; found is false if there is
// none.
func (s *Store) DeployStateOf(ctx context.Context, app string) (st DeployState, found bool, err error) {
	var fields, updated string
	err = s.db.QueryRowContext(ctx, `SELECT app, release, image, checksum, fields, updated_at FROM app_state WHERE app = ?`, app).
		Scan(&st.App, &st.Release, &st.Image, &st.Checksum, &fields, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return DeployState{}, false, nil
	}
	if err != nil {
		return DeployState{}, false, fmt.Errorf("read deploy state of %s: %w", app, err)
	}
	if err := json.Unmarshal([]byte(fields), &st.Fields); err != nil {
		return DeployState{}, false, fmt.Errorf("read deploy state of %s: %w", app, err)
	}
	if st.UpdatedAt, err = parseTime(updated); err != nil {
		return DeployState{}, false, err
	}
	return st, true, nil
}

// ForgetDeployState removes app's deploy state, for when something other
// than a trigger changed what runs (a deploy by hand, a rollback, a stop):
// the next trigger then deploys whatever it brings. There being none is
// not an error.
func (s *Store) ForgetDeployState(ctx context.Context, app string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM app_state WHERE app = ?`, app); err != nil {
		return fmt.Errorf("forget deploy state of %s: %w", app, err)
	}
	return nil
}
