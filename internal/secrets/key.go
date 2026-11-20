package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LoadKey reads the master key from path.
func LoadKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	// A key that other users of the machine can read protects nothing.
	// Refusing to use it is safer than warning about it: warnings scroll by.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("master key %s is readable by others (mode %o); fix it with: chmod 600 %s", path, perm, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}

	// The file holds the key as base64 text, so it survives being opened
	// in an editor or copied into a password manager.
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("master key %s is not valid base64", path)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("master key %s holds %d bytes, want %d", path, len(key), KeySize)
	}
	return key, nil
}

// CreateKey generates a new master key and writes it to path. It fails if
// the file already exists: overwriting a key makes everything encrypted
// with it unreadable for good.
func CreateKey(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create master key directory: %w", err)
	}

	key, err := newKey()
	if err != nil {
		return nil, err
	}
	if err := writeKeyFile(path, key); err != nil {
		return nil, fmt.Errorf("create master key: %w", err)
	}
	return key, nil
}

// KeyExists reports whether a key file is present at path.
func KeyExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("look for master key: %w", err)
	}
}

// newKey returns a new random master key.
func newKey() ([]byte, error) {
	key := make([]byte, KeySize)
	// The operating system's cryptographic random source. A key from a
	// predictable generator is no key at all.
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	return key, nil
}

// writeKeyFile writes key to a new file at path, readable by the owner
// only, and makes sure it is on the disk before returning.
func writeKeyFile(path string, key []byte) error {
	// O_EXCL makes creation fail if the file exists, and it does so
	// atomically: two processes starting at once cannot both "create" it.
	// The mode applies from the first moment, so the key is never readable
	// by others, not even briefly.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create key file: %w", err)
	}
	_, writeErr := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
	// Sync before the rename that follows in a rotation: a key that only
	// sits in the page cache when the power goes is a key that never was.
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		// A half-written key file would block the next start; remove it.
		_ = os.Remove(path)
		return fmt.Errorf("write key file: %w", err)
	}
	return nil
}
