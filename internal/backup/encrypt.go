package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/onrcanogul/liman/internal/secrets"
)

// EncryptedSuffix ends the name of an encrypted backup: liman-<time>.db.age.
const EncryptedSuffix = ".age"

// Encrypted reports whether the backup at path is encrypted, by its name.
func Encrypted(path string) bool {
	return strings.HasSuffix(path, EncryptedSuffix)
}

// encryptFile encrypts the file at src to the primary key of ring into a
// new file at dest, readable by the owner only. dest must not exist.
func encryptFile(src, dest string, ring *secrets.Keyring) error {
	recipient, err := ring.BackupRecipient()
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	// A temporary name in the same directory, linked to its final name
	// once complete: like an unencrypted backup, an encrypted one never
	// replaces a file, and never exists half-written under its name.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".liman-encrypt-*")
	if err != nil {
		return fmt.Errorf("encrypt backup: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	w, err := age.Encrypt(tmp, recipient)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encrypt backup: %w", err)
	}
	// Copy streams the file through in pieces; age encrypts it in chunks
	// of 64 KiB, so a large backup is never held in memory.
	_, copyErr := io.Copy(w, in)
	// Close writes the last chunk, marked as the last one: a file cut
	// short is then noticed when it is read.
	if err := errors.Join(copyErr, w.Close(), tmp.Sync(), tmp.Close()); err != nil {
		return fmt.Errorf("encrypt backup: %w", err)
	}
	if err := os.Link(tmp.Name(), dest); err != nil {
		return fmt.Errorf("place backup: %w", err)
	}
	return nil
}

// DecryptFile decrypts the backup at src into a new file at dest, readable
// by the owner only, with whichever key of the keyring at keyPath it was
// made with. dest must not exist.
func DecryptFile(src, dest, keyPath string) error {
	ring, err := loadKeyring(keyPath)
	if err != nil {
		return err
	}
	if ring == nil {
		return fmt.Errorf("%s is encrypted, and there is no master key at %s to open it", src, keyPath)
	}
	return decryptFile(src, dest, ring)
}

func decryptFile(src, dest string, ring *secrets.Keyring) error {
	identities, err := ring.BackupIdentities()
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	r, err := age.Decrypt(in, identities...)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return fmt.Errorf("%s was encrypted with a master key this server does not have", src)
		}
		return fmt.Errorf("decrypt %s: %w", src, err)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	// Every chunk is authenticated as it is read; a chunk that was changed,
	// moved or cut off fails the copy, and what was written is removed.
	_, copyErr := io.Copy(out, r)
	if err := errors.Join(copyErr, out.Sync(), out.Close()); err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("decrypt %s: %w", src, err)
	}
	return nil
}
