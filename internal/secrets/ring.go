package secrets

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A Keyring is the master key and the keys it replaced.
//
// The primary key seals everything new. The others only open: values and
// backups made before a rotation name the key they were made with, and it
// is looked up here. This is the arrangement Vault, cloud KMSs and Tink
// use, under names like key versions or keysets.
type Keyring struct {
	primary *Box
	boxes   map[KeyID]*Box
	keys    map[KeyID][]byte
	// pending is the key of a rotation that did not finish, if any. It
	// opens what that rotation already resealed.
	pending *KeyID
}

// NewKeyring returns a keyring with primary as the sealing key and others
// as keys that only open.
func NewKeyring(primary []byte, others ...[]byte) (*Keyring, error) {
	r := &Keyring{boxes: map[KeyID]*Box{}, keys: map[KeyID][]byte{}}
	for i, key := range append([][]byte{primary}, others...) {
		box, err := NewBox(key)
		if err != nil {
			return nil, err
		}
		r.boxes[box.ID()] = box
		r.keys[box.ID()] = key
		if i == 0 {
			r.primary = box
		}
	}
	return r, nil
}

// Primary returns the ID of the key that seals.
func (r *Keyring) Primary() KeyID { return r.primary.ID() }

// Pending returns the key of an unfinished rotation, if there is one.
func (r *Keyring) Pending() (KeyID, bool) {
	if r.pending == nil {
		return KeyID{}, false
	}
	return *r.pending, true
}

// IDs returns the IDs of all keys, the primary first, then the others in
// order.
func (r *Keyring) IDs() []KeyID {
	ids := []KeyID{r.primary.ID()}
	var rest []KeyID
	for id := range r.boxes {
		if id != r.primary.ID() {
			rest = append(rest, id)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].String() < rest[j].String() })
	return append(ids, rest...)
}

// Has reports whether the keyring holds the key id.
func (r *Keyring) Has(id KeyID) bool {
	_, ok := r.boxes[id]
	return ok
}

// Derive returns Derive(key, label) for the key id, and false if the
// keyring does not hold it.
func (r *Keyring) Derive(id KeyID, label string) ([]byte, bool) {
	key, ok := r.keys[id]
	if !ok {
		return nil, false
	}
	return Derive(key, label), true
}

// Seal seals with the primary key.
func (r *Keyring) Seal(plaintext, context []byte) ([]byte, error) {
	return r.primary.Seal(plaintext, context)
}

// UnknownKeyError is returned for a value sealed with a key the keyring
// does not hold. It is an ErrDecrypt too.
type UnknownKeyError struct {
	ID KeyID
}

func (e *UnknownKeyError) Error() string {
	return fmt.Sprintf("secret was sealed with key %s, which this server does not have", e.ID)
}

// Is makes errors.Is(err, ErrDecrypt) true.
func (e *UnknownKeyError) Is(target error) bool { return target == ErrDecrypt }

// Open opens a value with the key it names. A v1 value names none; each
// key is tried in turn (the primary first), which is cheap: a wrong key
// fails at the authentication tag.
func (r *Keyring) Open(sealed, context []byte) ([]byte, error) {
	_, plaintext, err := r.open(sealed, context)
	return plaintext, err
}

// SealedBy returns the ID of the key that opens a value.
func (r *Keyring) SealedBy(sealed, context []byte) (KeyID, error) {
	id, _, err := r.open(sealed, context)
	return id, err
}

func (r *Keyring) open(sealed, context []byte) (KeyID, []byte, error) {
	if id, ok := SealedWith(sealed); ok {
		box, known := r.boxes[id]
		if !known {
			return id, nil, &UnknownKeyError{ID: id}
		}
		plaintext, err := box.Open(sealed, context)
		return id, plaintext, err
	}
	for _, id := range r.IDs() {
		if plaintext, err := r.boxes[id].Open(sealed, context); err == nil {
			return id, plaintext, nil
		}
	}
	return KeyID{}, nil, ErrDecrypt
}

// --- files ---------------------------------------------------------------

// The master key lives at its path (master.key by default). Keys it
// replaced are kept next to it, one file per key, named by ID:
//
//	master.key                         the primary key
//	master.key.old/<id>.key            keys that only open
//	master.key.new                     only during a rotation
//
// The ID in the file name is checked against the key in the file, so a
// renamed file cannot pass itself off as another key.

// OldKeysDir returns the directory of the keys that were replaced.
func OldKeysDir(keyPath string) string { return keyPath + ".old" }

// PendingKeyPath returns where a rotation keeps its new key until the
// rotation is complete.
func PendingKeyPath(keyPath string) string { return keyPath + ".new" }

// LoadKeyring loads the master key at keyPath together with the keys it
// replaced and, if a rotation did not finish, that rotation's new key.
func LoadKeyring(keyPath string) (*Keyring, error) {
	primary, err := LoadKey(keyPath)
	if err != nil {
		return nil, err
	}

	var others [][]byte
	dir := OldKeysDir(keyPath)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read old keys: %w", err)
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".key")
		if !ok || e.IsDir() {
			continue
		}
		want, err := ParseKeyID(name)
		if err != nil {
			return nil, fmt.Errorf("%s: the file name is not a key ID", filepath.Join(dir, e.Name()))
		}
		key, err := LoadKey(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if IDOf(key) != want {
			return nil, fmt.Errorf("%s holds key %s, not the one its name says", filepath.Join(dir, e.Name()), IDOf(key))
		}
		others = append(others, key)
	}

	var pending []byte
	pendingPath := PendingKeyPath(keyPath)
	if exists, err := KeyExists(pendingPath); err != nil {
		return nil, err
	} else if exists {
		if pending, err = LoadKey(pendingPath); err != nil {
			return nil, err
		}
		others = append(others, pending)
	}

	ring, err := NewKeyring(primary, others...)
	if err != nil {
		return nil, err
	}
	if pending != nil {
		id := IDOf(pending)
		ring.pending = &id
	}
	return ring, nil
}
