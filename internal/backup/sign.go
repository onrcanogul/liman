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

// Signature file formats. The first line names the format, so that a later
// version can change the algorithm and still read old signatures.
const (
	// v1: "liman-backup-v1 hmac-sha256 <mac>". It does not say which key
	// signed; every key in the keyring is tried.
	sigHeaderV1 = "liman-backup-v1 hmac-sha256 "
	// v2: "liman-backup-v2 hmac-sha256 <key ID> <mac>".
	sigHeaderV2 = "liman-backup-v2 hmac-sha256 "
)

// sigLabel derives the signing key from a master key. The master key
// itself seals the secrets; using one key for two different jobs is how
// keys end up broken in ways neither job would have allowed alone.
const sigLabel = "liman backup signature v1"

// Signature states of a backup.
const (
	// SigValid: the file is exactly what one of the master keys signed.
	SigValid = "valid"
	// SigMissing: there is no signature file. The backup was made by an
	// older version, without a master key, or the file was left behind.
	SigMissing = "missing"
	// SigInvalid: the file, or its signature, is not what was written. (A
	// v1 signature also ends up here when the key that made it is gone,
	// since it does not say which key that was.)
	SigInvalid = "invalid"
	// SigUnknownKey: the signature names a key the server does not have,
	// one that was retired or never was here.
	SigUnknownKey = "unknown-key"
	// SigUnchecked: there is a signature but no master key to check it with.
	SigUnchecked = "unchecked"
)

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

// loadKeyring returns the keyring at keyPath, or nil if there is no
// master key.
func loadKeyring(keyPath string) (*secrets.Keyring, error) {
	exists, err := secrets.KeyExists(keyPath)
	if err != nil || !exists {
		return nil, err
	}
	return secrets.LoadKeyring(keyPath)
}

// Sign writes the signature file for the backup at path, with the primary
// key. It reports false if there is no master key to sign with.
func Sign(path, keyPath string) (bool, error) {
	ring, err := loadKeyring(keyPath)
	if err != nil || ring == nil {
		return false, err
	}
	id := ring.Primary()
	key, _ := ring.Derive(id, sigLabel)
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
	_, writeErr := tmp.WriteString(sigHeaderV2 + id.String() + " " + hex.EncodeToString(sum) + "\n")
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
// sigPath, using the keyring at keyPath. The two paths are separate
// because a restore checks its own private copy of the data against the
// signature that came with the original.
func signatureState(dataPath, sigPath, keyPath string) (state, signedBy string, err error) {
	if _, err := os.Stat(sigPath); errors.Is(err, fs.ErrNotExist) {
		return SigMissing, "", nil
	}
	ring, err := loadKeyring(keyPath)
	if err != nil {
		return "", "", err
	}
	if ring == nil {
		return SigUnchecked, "", nil
	}
	return SignatureWith(dataPath, sigPath, ring)
}

// SignatureWith checks the signature at sigPath of the file at dataPath
// against the keys in ring. signedBy is the ID of the key that signed it,
// as far as is known: the key that matched, or the one a v2 signature
// names.
func SignatureWith(dataPath, sigPath string, ring *secrets.Keyring) (state, signedBy string, err error) {
	raw, err := os.ReadFile(sigPath)
	if errors.Is(err, fs.ErrNotExist) {
		return SigMissing, "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read signature: %w", err)
	}

	// Anything that is not a well-formed signature is an invalid one: the
	// caller should not have to tell "damaged" from "forged".
	line := strings.TrimSpace(string(raw))
	var candidates []secrets.KeyID
	var macHex string
	if rest, ok := strings.CutPrefix(line, sigHeaderV2); ok {
		idText, m, found := strings.Cut(rest, " ")
		id, err := secrets.ParseKeyID(idText)
		if !found || err != nil {
			return SigInvalid, "", nil
		}
		if !ring.Has(id) {
			return SigUnknownKey, id.String(), nil
		}
		candidates, macHex, signedBy = []secrets.KeyID{id}, m, id.String()
	} else if m, ok := strings.CutPrefix(line, sigHeaderV1); ok {
		candidates, macHex = ring.IDs(), m
	} else {
		return SigInvalid, signedBy, nil
	}
	want, err := hex.DecodeString(macHex)
	if err != nil || len(want) != sha256.Size {
		return SigInvalid, signedBy, nil
	}

	for _, id := range candidates {
		key, _ := ring.Derive(id, sigLabel)
		got, err := fileMAC(dataPath, key)
		if err != nil {
			return "", "", fmt.Errorf("check signature: %w", err)
		}
		// hmac.Equal compares in constant time. With an ordinary
		// comparison the time taken would tell an attacker how many
		// leading bytes of a guessed signature were right.
		if hmac.Equal(got, want) {
			return SigValid, id.String(), nil
		}
	}
	return SigInvalid, signedBy, nil
}
