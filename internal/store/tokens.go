package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
}

// CreateToken generates a new API token, stores its hash under name and
// returns the token itself. This is the only moment the token is known; it
// cannot be recovered later.
func (s *Store) CreateToken(ctx context.Context, name string) (string, error) {
	return s.CreateScopedToken(ctx, name, ScopeAdmin)
}

// CreateScopedToken is CreateToken for a token with the given scope.
func (s *Store) CreateScopedToken(ctx context.Context, name, scope string) (string, error) {
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
		`INSERT INTO tokens (name, hash, scope, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (name) DO NOTHING`,
		name, hashToken(token), scope, formatTime(time.Now()))
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

// LookupToken returns the name and scope of token. ok is false if the
// token is not valid.
func (s *Store) LookupToken(ctx context.Context, token string) (info TokenInfo, ok bool, err error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return TokenInfo{}, false, nil
	}
	candidate := []byte(hashToken(token))

	rows, err := s.db.QueryContext(ctx, `SELECT hash, name, scope FROM tokens`)
	if err != nil {
		return TokenInfo{}, false, fmt.Errorf("read tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	valid := false
	for rows.Next() {
		var hash, name, scope string
		if err := rows.Scan(&hash, &name, &scope); err != nil {
			return TokenInfo{}, false, fmt.Errorf("read token: %w", err)
		}
		// ConstantTimeCompare takes the same time whether the first or the
		// last byte differs. An ordinary comparison stops at the first
		// difference, and that timing can be measured to guess a secret
		// byte by byte. The loop also deliberately does not stop early.
		if subtle.ConstantTimeCompare(candidate, []byte(hash)) == 1 {
			valid = true
			info = TokenInfo{Name: name, Scope: scope}
		}
	}
	if err := rows.Err(); err != nil {
		return TokenInfo{}, false, fmt.Errorf("read tokens: %w", err)
	}
	return info, valid, nil
}

// Tokens lists the stored tokens by name.
func (s *Store) Tokens(ctx context.Context) ([]TokenInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, scope, created_at FROM tokens ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tokens []TokenInfo
	for rows.Next() {
		var info TokenInfo
		var created string
		if err := rows.Scan(&info.Name, &info.Scope, &created); err != nil {
			return nil, fmt.Errorf("read token: %w", err)
		}
		if info.CreatedAt, err = parseTime(created); err != nil {
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
