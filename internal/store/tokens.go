package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// tokenNameRe is the shape of a token's name.
var tokenNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// TokenPrefix starts every token. It makes a leaked token recognisable in
// logs and by secret scanners.
const TokenPrefix = "liman_"

// Scopes say what a token may do.
const (
	// ScopeAdmin may do everything, which on this daemon means: run
	// arbitrary containers on the server.
	ScopeAdmin = "admin"
	// ScopeRead may look at everything and change nothing. It is what a
	// dashboard on a wall, or a panel opened on a machine that is not
	// entirely trusted, should be given.
	ScopeRead = "read"
)

// TokenInfo describes a token without revealing it.
type TokenInfo struct {
	Name      string
	Scope     string
	CreatedAt time.Time
	// ExpiresAt is when the token stops working. The zero time means
	// never: that is what every token created before expiry existed has,
	// and what a token created without --expires gets.
	ExpiresAt time.Time
	// LastUsedAt is when the token was last accepted, to within a minute.
	// The zero time means not since liman started keeping track.
	LastUsedAt time.Time
}

// Expired reports whether the token has stopped working at now.
func (t TokenInfo) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// CreateToken generates a new API token, stores its hash under name and
// returns the token itself. This is the only moment the token is known; it
// cannot be recovered later.
func (s *Store) CreateToken(ctx context.Context, name string) (string, error) {
	return s.CreateScopedToken(ctx, name, ScopeAdmin, time.Time{})
}

// CreateScopedToken is CreateToken for a token with the given scope that
// stops working at expiresAt; the zero time means never. A time that has
// already passed is accepted (the token is then useless from the start):
// deciding what a sensible lifetime is belongs to the caller.
func (s *Store) CreateScopedToken(ctx context.Context, name, scope string, expiresAt time.Time) (string, error) {
	if name == "" {
		return "", fmt.Errorf("token name must not be empty")
	}
	// The name is printed in listings and written to the log with every
	// request; it must not be able to carry line breaks or escape codes
	// there, or to impersonate another field.
	if len(name) > 64 || !tokenNameRe.MatchString(name) {
		return "", fmt.Errorf("token name %q: use up to 64 letters, digits, dots, dashes and underscores", name)
	}
	if scope != ScopeAdmin && scope != ScopeRead {
		return "", fmt.Errorf("unknown token scope %q", scope)
	}

	// crypto/rand reads from the operating system's secure random source.
	// math/rand must never be used for secrets: its output is predictable.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := TokenPrefix + hex.EncodeToString(raw)

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (name, hash, scope, created_at, expires_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (name) DO NOTHING`,
		name, hashToken(token), scope, formatTime(time.Now()), formatOptionalTime(expiresAt))
	if err != nil {
		return "", fmt.Errorf("store token: %w", err)
	}
	// With DO NOTHING a duplicate name is not an error; it simply inserts
	// zero rows.
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("token %q: %w", name, ErrExists)
	}
	return token, nil
}

// VerifyToken reports whether token is valid.
func (s *Store) VerifyToken(ctx context.Context, token string) (bool, error) {
	_, ok, err := s.LookupToken(ctx, token)
	return ok, err
}

// LookupToken returns what is known about token. ok is false if the token
// is not valid: unknown, revoked or expired.
//
// An expired token is the one case where info is filled in although ok is
// false, so that the caller can say why it was refused (and whose token it
// was) instead of a bare "invalid". Code that only wants a yes or no must
// look at ok and nothing else.
func (s *Store) LookupToken(ctx context.Context, token string) (info TokenInfo, ok bool, err error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return TokenInfo{}, false, nil
	}
	candidate := []byte(hashToken(token))

	rows, err := s.db.QueryContext(ctx, `SELECT hash, name, scope, created_at, expires_at, last_used_at FROM tokens`)
	if err != nil {
		return TokenInfo{}, false, fmt.Errorf("read tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	valid := false
	for rows.Next() {
		var hash string
		var row TokenInfo
		if err := scanToken(rows, &row, &hash); err != nil {
			return TokenInfo{}, false, err
		}
		// ConstantTimeCompare takes the same time whether the first or the
		// last byte differs. An ordinary comparison stops at the first
		// difference, and that timing can be measured to guess a secret
		// byte by byte. The loop also deliberately does not stop early.
		if subtle.ConstantTimeCompare(candidate, []byte(hash)) == 1 {
			valid = true
			info = row
		}
	}
	if err := rows.Err(); err != nil {
		return TokenInfo{}, false, fmt.Errorf("read tokens: %w", err)
	}
	// The clock is read once, after the loop: whether a token has expired
	// must not depend on where in the table it happens to be.
	if valid && info.Expired(time.Now()) {
		return info, false, nil
	}
	return info, valid, nil
}

// Tokens lists the stored tokens by name.
func (s *Store) Tokens(ctx context.Context) ([]TokenInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, scope, created_at, expires_at, last_used_at FROM tokens ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tokens []TokenInfo
	for rows.Next() {
		var info TokenInfo
		if err := scanToken(rows, &info); err != nil {
			return nil, err
		}
		tokens = append(tokens, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tokens: %w", err)
	}
	return tokens, nil
}

// RevokeToken deletes the token stored under name.
func (s *Store) RevokeToken(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("token %q: %w", name, ErrNotFound)
	}
	return nil
}

// TouchToken records that the token stored under name was used at at. It
// never moves the time backwards, and a token that has been revoked in the
// meantime is simply not found: neither is an error.
//
// It is a write, and the database has one writer: a caller on the path of
// every request should not call it for every request (the server does so
// at most once a minute per token).
func (s *Store) TouchToken(ctx context.Context, name string, at time.Time) error {
	stamp := formatTime(at)
	// Times are stored in one fixed format, so comparing them as strings
	// compares them as times.
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE name = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
		stamp, name, stamp)
	if err != nil {
		return fmt.Errorf("record token use: %w", err)
	}
	return nil
}

// scanToken reads a row of name, scope, created_at, expires_at, last_used_at into info.
// Columns the query selects before those go into leading.
func scanToken(rows *sql.Rows, info *TokenInfo, leading ...any) error {
	var created string
	// NullString, because expires_at is NULL for a token that never
	// expires. Scanning NULL into a plain string is an error in
	// database/sql: Go has no null for strings, so the "is it there?" bit
	// has to live somewhere, and NullString is a struct that carries it.
	var expires, used sql.NullString
	dest := append(leading, &info.Name, &info.Scope, &created, &expires, &used)
	if err := rows.Scan(dest...); err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	var err error
	if info.CreatedAt, err = parseTime(created); err != nil {
		return err
	}
	if expires.Valid {
		if info.ExpiresAt, err = parseTime(expires.String); err != nil {
			return err
		}
	}
	if used.Valid {
		if info.LastUsedAt, err = parseTime(used.String); err != nil {
			return err
		}
	}
	return nil
}

// formatOptionalTime is formatTime for a column where the zero time is
// stored as NULL.
func formatOptionalTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}

// hashToken returns the hex SHA-256 of a token.
//
// Only the hash is stored, so reading the database does not reveal usable
// tokens. A plain, fast hash is enough here because tokens are 256 random
// bits and cannot be guessed. Passwords chosen by people are different: they
// need a deliberately slow hash such as bcrypt or argon2.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
