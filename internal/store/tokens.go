package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// TokenPrefix starts every token. It makes a leaked token recognisable in
// logs and by secret scanners.
const TokenPrefix = "liman_"

// TokenInfo describes a token without revealing it.
type TokenInfo struct {
	Name      string
	CreatedAt time.Time
}

// CreateToken generates a new API token, stores its hash under name and
// returns the token itself. This is the only moment the token is known; it
// cannot be recovered later.
func (s *Store) CreateToken(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("token name must not be empty")
	}

	// crypto/rand reads from the operating system's secure random source.
	// math/rand must never be used for secrets: its output is predictable.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := TokenPrefix + hex.EncodeToString(raw)

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (name, hash, created_at) VALUES (?, ?, ?)
		 ON CONFLICT (name) DO NOTHING`,
		name, hashToken(token), formatTime(time.Now()))
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
	if !strings.HasPrefix(token, TokenPrefix) {
		return false, nil
	}
	candidate := []byte(hashToken(token))

	rows, err := s.db.QueryContext(ctx, `SELECT hash FROM tokens`)
	if err != nil {
		return false, fmt.Errorf("read tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	valid := false
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return false, fmt.Errorf("read token: %w", err)
		}
		// ConstantTimeCompare takes the same time whether the first or the
		// last byte differs. An ordinary comparison stops at the first
		// difference, and that timing can be measured to guess a secret
		// byte by byte. The loop also deliberately does not stop early.
		if subtle.ConstantTimeCompare(candidate, []byte(hash)) == 1 {
			valid = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read tokens: %w", err)
	}
	return valid, nil
}

// Tokens lists the stored tokens by name.
func (s *Store) Tokens(ctx context.Context) ([]TokenInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, created_at FROM tokens ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tokens []TokenInfo
	for rows.Next() {
		var info TokenInfo
		var created string
		if err := rows.Scan(&info.Name, &created); err != nil {
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
