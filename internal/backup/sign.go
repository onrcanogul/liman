package backup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/onrcanogul/liman/internal/secrets"
)

// A backup is a database that will one day be trusted completely: the
// tokens in it become valid tokens. The secrets inside are sealed and
// cannot be forged, but a row in the tokens table is only a hash, and
// anyone can compute the hash of a token they made up. Whoever can alter a
// backup on its way to a restore could therefore add a token of their own.
//
// Every backup gets a signature next to it: an HMAC of the whole file,
// keyed from the master key. The key is not in the backup and not next to
// it, so a changed file cannot be given a matching signature.

// SigSuffix is appended to a backup's file name for its signature file.
const SigSuffix = ".sig"

// sigHeader starts a signature file. It names the format, so that a later
// version can change the algorithm and still read old signatures.
const sigHeader = "liman-backup-v1 hmac-sha256 "

// Signature states of a backup.
const (
	// SigValid: the file is exactly what this master key signed.
	SigValid = "valid"
	// SigMissing: there is no signature file. The backup was made by an
	// older version, without a master key, or the file was left behind.
	SigMissing = "missing"
	// SigInvalid: the file, or its signature, is not what was written, or
	// it was signed with another master key.
	SigInvalid = "invalid"
	// SigUnchecked: there is a signature but no master key to check it with.
	SigUnchecked = "unchecked"
)

// signingKey derives the key that signs backups from the master key.
//
// The master key itself encrypts the secrets. Using one key for two
// different jobs is how keys end up broken in ways neither job would have
// allowed alone; deriving a second key with a fixed label keeps the two
// apart while still needing only one key to be kept safe.
func signingKey(master []byte) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("liman backup signature v1"))
	return mac.Sum(nil)
}

// fileMAC computes the HMAC of the file at path.
func fileMAC(path string, key []byte) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	mac := hmac.New(sha256.New, key)
	// Copy feeds the file through in pieces; a large backup is never held
	// in memory as a whole.
	if _, err := io.Copy(mac, f); err != nil {
		return nil, err
	}
	return mac.Sum(nil), nil
}

// loadSigningKey returns the signing key, or nil if there is no master key.
func loadSigningKey(keyPath string) ([]byte, error) {
	exists, err := secrets.KeyExists(keyPath)
	if err != nil || !exists {
		return nil, err
	}
	master, err := secrets.LoadKey(keyPath)
	if err != nil {
		return nil, err
	}
	return signingKey(master), nil
}

// Sign writes the signature file for the backup at path. It reports false
// if there is no master key to sign with.
func Sign(path, keyPath string) (bool, error) {
	key, err := loadSigningKey(keyPath)
	if err != nil || key == nil {
		return false, err
	}
	sum, err := fileMAC(path, key)
	if err != nil {
		return false, fmt.Errorf("sign backup: %w", err)
	}

	// A new file, renamed into place: like the backup itself, a signature
	// is never written into something that already exists.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".liman-sig-*")
	if err != nil {
		return false, fmt.Errorf("sign backup: %w", err)
	}
	_, writeErr := tmp.WriteString(sigHeader + hex.EncodeToString(sum) + "\n")
	if err := errors.Join(writeErr, tmp.Sync(), tmp.Close()); err != nil {
		_ = os.Remove(tmp.Name())
		return false, fmt.Errorf("sign backup: %w", err)
	}
	if err := os.Rename(tmp.Name(), path+SigSuffix); err != nil {
		_ = os.Remove(tmp.Name())
		return false, fmt.Errorf("sign backup: %w", err)
	}
	return true, nil
}

// signatureState compares the file at dataPath with the signature at
// sigPath. The two are separate arguments because a restore checks its own
// private copy of the data against the signature that came with the
// original.
func signatureState(dataPath, sigPath, keyPath string) (string, error) {
	raw, err := os.ReadFile(sigPath)
	if errors.Is(err, fs.ErrNotExist) {
		return SigMissing, nil
	}
	if err != nil {
		return "", fmt.Errorf("read signature: %w", err)
	}

	key, err := loadSigningKey(keyPath)
	if err != nil {
		return "", err
	}
	if key == nil {
		return SigUnchecked, nil
	}

	// Anything that is not a well-formed signature is an invalid one: the
	// caller should not have to tell "damaged" from "forged".
	text, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), sigHeader)
	if !ok {
		return SigInvalid, nil
	}
	want, err := hex.DecodeString(text)
	if err != nil || len(want) != sha256.Size {
		return SigInvalid, nil
	}

	got, err := fileMAC(dataPath, key)
	if err != nil {
		return "", fmt.Errorf("check signature: %w", err)
	}
	// hmac.Equal compares in constant time. With an ordinary comparison
	// the time taken would tell an attacker how many leading bytes of a
	// guessed signature were right.
	if !hmac.Equal(got, want) {
		return SigInvalid, nil
	}
	return SigValid, nil
}
