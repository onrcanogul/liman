package secrets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MaxValueBytes bounds the size of one secret. Secrets are passwords, keys
// and connection strings; a certificate chain is the largest thing that
// plausibly belongs here.
const MaxValueBytes = 64 << 10

// ErrNotSet is returned when a secret does not exist.
var ErrNotSet = errors.New("secret is not set")

// nameRe is the shape of a secret's name. Secrets become environment
// variables, so the rules for those apply.
var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidName reports whether name can be used for a secret.
func ValidName(name string) bool {
	return len(name) <= 128 && nameRe.MatchString(name)
}

// Info describes a secret without revealing it.
type Info struct {
	// App is filled in only by a listing that spans every app.
	App       string    `json:"app,omitempty"`
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Storage is where sealed secrets are kept. *store.Store implements it; the
// interface is declared here because this package is the one that needs it.
// Storage only ever sees ciphertext.
type Storage interface {
	PutSecret(ctx context.Context, app, name string, sealed []byte) error
	GetSecret(ctx context.Context, app, name string) (sealed []byte, found bool, err error)
	ListSecrets(ctx context.Context, app string) ([]Info, error)
	// ListAllSecrets lists the secrets of every app, App filled in,
	// including apps that are not deployed (yet, or any more).
	ListAllSecrets(ctx context.Context) ([]Info, error)
	DeleteSecret(ctx context.Context, app, name string) (found bool, err error)
}

// Vault stores and retrieves the secrets of apps. Values are encrypted
// before they reach the storage and decrypted only when a deploy needs them.
type Vault struct {
	ring    *Keyring
	storage Storage
}

// NewVault returns a Vault that encrypts with key and keeps its data in
// storage.
func NewVault(key []byte, storage Storage) (*Vault, error) {
	ring, err := NewKeyring(key)
	if err != nil {
		return nil, err
	}
	return NewVaultWithKeyring(ring, storage), nil
}

// NewVaultWithKeyring returns a Vault that seals with the primary key of
// ring and opens with whichever of its keys a value names.
func NewVaultWithKeyring(ring *Keyring, storage Storage) *Vault {
	return &Vault{ring: ring, storage: storage}
}

// Reseal opens a value stored as app/name with from and seals it again with
// the primary key of to. It reports false, and returns sealed unchanged,
// when the value is already sealed with that key.
func Reseal(from, to *Keyring, app, name string, sealed []byte) ([]byte, bool, error) {
	if id, ok := SealedWith(sealed); ok && id == to.Primary() {
		return sealed, false, nil
	}
	context := sealContext(app, name)
	plaintext, err := from.Open(sealed, context)
	if err != nil {
		return nil, false, fmt.Errorf("%s/%s: %w", app, name, err)
	}
	resealed, err := to.Seal(plaintext, context)
	if err != nil {
		return nil, false, err
	}
	return resealed, true, nil
}

// SealedBy returns the ID of the key the stored value app/name opens with.
func (v *Vault) SealedBy(ctx context.Context, app, name string) (KeyID, error) {
	sealed, found, err := v.storage.GetSecret(ctx, app, name)
	if err != nil {
		return KeyID{}, err
	}
	if !found {
		return KeyID{}, fmt.Errorf("%s: %w", name, ErrNotSet)
	}
	return v.ring.SealedBy(sealed, sealContext(app, name))
}

// Set stores value as the secret name of app, replacing an earlier value.
func (v *Vault) Set(ctx context.Context, app, name string, value []byte) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid secret name %q: use letters, digits and underscores, not starting with a digit", name)
	}
	switch {
	case len(value) == 0:
		return errors.New("the secret value is empty")
	case len(value) > MaxValueBytes:
		return fmt.Errorf("the secret value is %d bytes, more than the limit of %d", len(value), MaxValueBytes)
	case strings.ContainsRune(string(value), 0):
		// A NUL byte ends a string in the environment; everything after it
		// would silently disappear.
		return errors.New("the secret value contains a NUL byte")
	}

	sealed, err := v.ring.Seal(value, sealContext(app, name))
	if err != nil {
		return err
	}
	return v.storage.PutSecret(ctx, app, name, sealed)
}

// Get returns the secret name of app.
func (v *Vault) Get(ctx context.Context, app, name string) ([]byte, error) {
	sealed, found, err := v.storage.GetSecret(ctx, app, name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%s: %w", name, ErrNotSet)
	}

	value, err := v.ring.Open(sealed, sealContext(app, name))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return value, nil
}

// List returns the names of the secrets of app, never their values.
func (v *Vault) List(ctx context.Context, app string) ([]Info, error) {
	return v.storage.ListSecrets(ctx, app)
}

// ListAll returns the names of the secrets of every app, sorted by app and
// name. A secret stays when its app is stopped, and can be stored before
// the app's first deploy: this is the one listing in which those show up.
func (v *Vault) ListAll(ctx context.Context) ([]Info, error) {
	return v.storage.ListAllSecrets(ctx)
}

// Delete removes the secret name of app.
func (v *Vault) Delete(ctx context.Context, app, name string) error {
	found, err := v.storage.DeleteSecret(ctx, app, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s: %w", name, ErrNotSet)
	}
	return nil
}

// Missing returns the names among names that are not set for app, sorted.
// It does not decrypt anything, so it is safe for a dry run.
func (v *Vault) Missing(ctx context.Context, app string, names []string) ([]string, error) {
	existing, err := v.storage.ListSecrets(ctx, app)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(existing))
	for _, info := range existing {
		set[info.Name] = true
	}

	var missing []string
	for _, name := range names {
		if !set[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// Resolve returns the values of the named secrets of app. It fails if any
// of them is not set, so a deploy never starts with part of its
// configuration missing.
func (v *Vault) Resolve(ctx context.Context, app string, names []string) (map[string]string, error) {
	missing, err := v.Missing(ctx, app, names)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, &MissingError{App: app, Names: missing}
	}

	values := make(map[string]string, len(names))
	for _, name := range names {
		value, err := v.Get(ctx, app, name)
		if err != nil {
			return nil, err
		}
		values[name] = string(value)
	}
	return values, nil
}

// MissingError reports secrets an app refers to that have not been set.
type MissingError struct {
	App   string
	Names []string
}

func (e *MissingError) Error() string {
	// The message names only what is missing, and says how to fix it.
	return fmt.Sprintf("%s refers to secrets that are not set: %s; set each with: liman secret set %s <NAME>",
		e.App, strings.Join(e.Names, ", "), e.App)
}

// Is makes errors.Is(err, ErrNotSet) true for a MissingError.
func (e *MissingError) Is(target error) bool {
	return target == ErrNotSet
}

// sealContext is the associated data that binds a sealed value to the app
// and name it was stored under. The NUL separator cannot occur in either
// part, so ("ab", "c") and ("a", "bc") can never produce the same context.
func sealContext(app, name string) []byte {
	return []byte(app + "\x00" + name)
}
