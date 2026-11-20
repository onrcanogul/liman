package secrets

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Resealer rewrites every stored secret in one transaction: fn gets each
// sealed value and returns its replacement (or reports that it stays as
// it is). If fn fails for any value, nothing is changed.
type Resealer interface {
	ResealSecrets(ctx context.Context, fn func(app, name string, sealed []byte) ([]byte, bool, error)) (int, error)
}

// Rotation describes a completed rotation.
type Rotation struct {
	Old, New KeyID
	// Resealed is how many stored secrets were sealed again.
	Resealed int
	// Resumed is true if this run finished a rotation that had been
	// interrupted, with the new key that rotation had already made.
	Resumed bool
}

// Rotate replaces the master key at keyPath with a new one and seals every
// stored secret again with it. The old key moves to OldKeysDir, where it
// still opens backups made with it.
//
// The database and the key file cannot change in one step, so the order
// matters. At every point in between, each stored value is sealed with a
// key the keyring holds, and running Rotate again finishes the job:
//
//  1. the new key is written to PendingKeyPath (LoadKeyring includes it)
//  2. the old key is copied into OldKeysDir
//  3. all secrets are sealed again, in one transaction
//  4. the new key is renamed over keyPath
//
// The daemon must not run meanwhile: it holds the old key in memory and
// would go on sealing with it.
func Rotate(ctx context.Context, keyPath string, st Resealer) (Rotation, error) {
	ring, err := LoadKeyring(keyPath)
	if err != nil {
		return Rotation{}, err
	}
	old, err := LoadKey(keyPath)
	if err != nil {
		return Rotation{}, err
	}
	rot := Rotation{Old: IDOf(old)}

	// 1. The new key, or the one an interrupted rotation left behind.
	pendingPath := PendingKeyPath(keyPath)
	var fresh []byte
	if _, ok := ring.Pending(); ok {
		if fresh, err = LoadKey(pendingPath); err != nil {
			return Rotation{}, err
		}
		rot.Resumed = true
	} else {
		if fresh, err = CreateKey(pendingPath); err != nil {
			return Rotation{}, err
		}
		// The keyring must be able to open what step 3 is about to seal,
		// in case step 4 never happens.
		if ring, err = LoadKeyring(keyPath); err != nil {
			return Rotation{}, err
		}
	}
	rot.New = IDOf(fresh)
	if rot.New == rot.Old {
		return Rotation{}, fmt.Errorf("%s is the same key as %s; remove it and rotate again", pendingPath, keyPath)
	}

	// 2. The old key stays, under its ID, for what was made with it.
	if err := keepOldKey(keyPath, old); err != nil {
		return Rotation{}, err
	}

	// 3. Every secret, sealed again with the new key.
	target, err := NewKeyring(fresh, old)
	if err != nil {
		return Rotation{}, err
	}
	rot.Resealed, err = st.ResealSecrets(ctx, func(app, name string, sealed []byte) ([]byte, bool, error) {
		return Reseal(ring, target, app, name, sealed)
	})
	if err != nil {
		return Rotation{}, fmt.Errorf("seal the secrets again: %w (nothing was changed in the database; %s stays for the next attempt)", err, pendingPath)
	}

	// 4. The new key becomes the master key.
	if err := os.Rename(pendingPath, keyPath); err != nil {
		return Rotation{}, fmt.Errorf("install the new key: %w", err)
	}
	syncDir(filepath.Dir(keyPath))
	return rot, nil
}

// keepOldKey copies key into OldKeysDir under its ID. A copy that is
// already there (from an interrupted rotation) is fine if it holds the
// same key.
func keepOldKey(keyPath string, key []byte) error {
	dir := OldKeysDir(keyPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, IDOf(key).String()+".key")
	if exists, err := KeyExists(path); err != nil {
		return err
	} else if exists {
		kept, err := LoadKey(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(kept, key) {
			return fmt.Errorf("%s holds another key than its name says", path)
		}
		return nil
	}
	if err := writeKeyFile(path, key); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir makes a rename or a new file in dir durable. Errors are ignored:
// not every platform can sync a directory, and the rename has happened.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
