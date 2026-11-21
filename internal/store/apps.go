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
	Repo string
	// Domain is "" for a worker, which is reached from nowhere.
	Domain string
	// Kind is "web" or "worker".
	Kind string
	// Publish is the app's fixed endpoints as app.Publish writes them,
	// "" for none.
	Publish   string
	CreatedAt time.Time
}

// Worker reports whether the registration is of a worker.
func (r Registration) Worker() bool { return r.Kind == "worker" }

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
	kind := r.Kind
	if kind == "" {
		kind = "web"
	}
	// No domain is NULL, not "": two workers would collide on "".
	_, err = tx.ExecContext(ctx,
		`INSERT INTO apps (name, repo, domain, kind, publish, created_at) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?)`,
		r.Name, r.Repo, r.Domain, kind, r.Publish, formatTime(time.Now()))
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
	err = s.db.QueryRowContext(ctx, `SELECT name, repo, COALESCE(domain, ''), kind, publish, created_at FROM apps WHERE name = ?`, app).
		Scan(&r.Name, &r.Repo, &r.Domain, &r.Kind, &r.Publish, &created)
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
	// No domain is nobody's: "domain = NULL" would match nothing anyway,
	// but not by intent.
	if domain == "" {
		return "", nil
	}
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
	rows, err := s.db.QueryContext(ctx, `SELECT name, repo, COALESCE(domain, ''), kind, publish, created_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list registrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Registration
	for rows.Next() {
		var r Registration
		var created string
		if err := rows.Scan(&r.Name, &r.Repo, &r.Domain, &r.Kind, &r.Publish, &created); err != nil {
			return nil, fmt.Errorf("list registrations: %w", err)
		}
		if r.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// KeyChanges says what happened to an app's deploy and preview keys.
type KeyChanges struct {
	// Revoked are keys that served the app alone, and are gone.
	Revoked []string
	// Detached are keys that serve other apps too: the app was taken off
	// them, and they keep working for the others.
	Detached []string
	// Expiring are keys that served the app alone and keep working until
	// Until, when they expire.
	Expiring []string
	Until    time.Time
}

// Unregister removes app's registration, what its last triggered deploy
// left, and its deploy and preview keys, in one transaction. A key that
// outlived its app would come back to life if the name were registered
// again: whoever registers it next would inherit a key they never saw,
// leaked perhaps. (A trial showed exactly that.) Admin and read tokens
// limited to the app stay: what runs may still need managing.
func (s *Store) Unregister(ctx context.Context, app string) (KeyChanges, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KeyChanges{}, fmt.Errorf("unregister %s: %w", app, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM apps WHERE name = ?`, app)
	if err != nil {
		return KeyChanges{}, fmt.Errorf("unregister %s: %w", app, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return KeyChanges{}, fmt.Errorf("app %s: %w", app, ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_state WHERE app = ?`, app); err != nil {
		return KeyChanges{}, fmt.Errorf("unregister %s: %w", app, err)
	}
	changes, err := retireKeys(ctx, tx, app, 0, ScopeDeploy, ScopePreview)
	if err != nil {
		return KeyChanges{}, err
	}
	return changes, tx.Commit()
}

// RotateKey creates a new key of scope (deploy or preview) named name for
// a registered app, and retires its old keys of that scope: at once, or,
// with keepOld, when keepOld has passed. Either both happen or neither.
// A key shared with other apps loses this app at once either way: it
// cannot expire for one app only.
func (s *Store) RotateKey(ctx context.Context, app, scope, name string, keepOld time.Duration) (string, KeyChanges, error) {
	if scope != ScopeDeploy && scope != ScopePreview {
		return "", KeyChanges{}, fmt.Errorf("only deploy and preview keys are rotated, not %q", scope)
	}
	if len(name) > 64 || !tokenNameRe.MatchString(name) {
		return "", KeyChanges{}, fmt.Errorf("token name %q: use up to 64 letters, digits, dots, dashes and underscores", name)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", KeyChanges{}, fmt.Errorf("rotate key of %s: %w", app, err)
	}
	defer func() { _ = tx.Rollback() }()
	// The old ones first, so that the new one is not among them.
	changes, err := retireKeys(ctx, tx, app, keepOld, scope)
	if err != nil {
		return "", KeyChanges{}, err
	}
	token, err := insertToken(ctx, tx, name, scope, time.Time{}, []string{app})
	if err != nil {
		return "", KeyChanges{}, err
	}
	return token, changes, tx.Commit()
}

// retireKeys retires, in tx, app's keys of the given scopes: a key that
// serves app alone is removed, or set to expire after keepOld; a key that
// serves other apps too has app taken off it.
func retireKeys(ctx context.Context, tx *sql.Tx, app string, keepOld time.Duration, scopes ...string) (KeyChanges, error) {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(scopes)), ",")
	args := []any{app}
	for _, sc := range scopes {
		args = append(args, sc)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT t.name, t.expires_at, (SELECT COUNT(*) FROM token_apps x WHERE x.token = t.name)
		 FROM tokens t JOIN token_apps a ON a.token = t.name
		 WHERE a.app = ? AND t.scope IN (`+marks+`) ORDER BY t.name`, args...)
	if err != nil {
		return KeyChanges{}, fmt.Errorf("list keys of %s: %w", app, err)
	}
	type key struct {
		name    string
		expires sql.NullString
		apps    int
	}
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.name, &k.expires, &k.apps); err != nil {
			_ = rows.Close()
			return KeyChanges{}, fmt.Errorf("list keys of %s: %w", app, err)
		}
		keys = append(keys, k)
	}
	if err := rows.Close(); err != nil {
		return KeyChanges{}, err
	}

	var c KeyChanges
	if keepOld > 0 {
		c.Until = time.Now().Add(keepOld)
	}
	for _, k := range keys {
		switch {
		case k.apps > 1:
			if _, err := tx.ExecContext(ctx, `DELETE FROM token_apps WHERE token = ? AND app = ?`, k.name, app); err != nil {
				return KeyChanges{}, fmt.Errorf("take %s off key %s: %w", app, k.name, err)
			}
			c.Detached = append(c.Detached, k.name)
		case keepOld > 0:
			until := c.Until
			// A key that expires sooner anyway keeps its own end.
			if k.expires.Valid {
				if own, err := parseTime(k.expires.String); err == nil && own.Before(until) {
					until = own
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tokens SET expires_at = ? WHERE name = ?`, formatTime(until), k.name); err != nil {
				return KeyChanges{}, fmt.Errorf("set key %s to expire: %w", k.name, err)
			}
			c.Expiring = append(c.Expiring, k.name)
		default:
			// token_apps goes with it (ON DELETE CASCADE).
			if _, err := tx.ExecContext(ctx, `DELETE FROM tokens WHERE name = ?`, k.name); err != nil {
				return KeyChanges{}, fmt.Errorf("revoke key %s: %w", k.name, err)
			}
			c.Revoked = append(c.Revoked, k.name)
		}
	}
	return c, nil
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
	Checksum string
	Fields   map[string]string
	// Templates holds the HMAC of each value as the definition writes it,
	// before its variables are filled in: what a preview, which has no
	// secrets, can compare. Empty for a state saved before there were
	// any.
	Templates map[string]string
	UpdatedAt time.Time
}

// SaveDeployState replaces app's deploy state.
func (s *Store) SaveDeployState(ctx context.Context, st DeployState) error {
	fields, err := json.Marshal(st.Fields)
	if err != nil {
		return fmt.Errorf("save deploy state of %s: %w", st.App, err)
	}
	templates, err := json.Marshal(st.Templates)
	if err != nil {
		return fmt.Errorf("save deploy state of %s: %w", st.App, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO app_state (app, release, image, checksum, fields, templates, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (app) DO UPDATE SET release = excluded.release, image = excluded.image,
		   checksum = excluded.checksum, fields = excluded.fields, templates = excluded.templates,
		   updated_at = excluded.updated_at`,
		st.App, st.Release, st.Image, st.Checksum, string(fields), string(templates), formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("save deploy state of %s: %w", st.App, err)
	}
	return nil
}

// DeployStateOf returns app's deploy state; found is false if there is
// none.
func (s *Store) DeployStateOf(ctx context.Context, app string) (st DeployState, found bool, err error) {
	var fields, templates, updated string
	err = s.db.QueryRowContext(ctx, `SELECT app, release, image, checksum, fields, templates, updated_at FROM app_state WHERE app = ?`, app).
		Scan(&st.App, &st.Release, &st.Image, &st.Checksum, &fields, &templates, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return DeployState{}, false, nil
	}
	if err != nil {
		return DeployState{}, false, fmt.Errorf("read deploy state of %s: %w", app, err)
	}
	if err := json.Unmarshal([]byte(fields), &st.Fields); err != nil {
		return DeployState{}, false, fmt.Errorf("read deploy state of %s: %w", app, err)
	}
	if err := json.Unmarshal([]byte(templates), &st.Templates); err != nil {
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
