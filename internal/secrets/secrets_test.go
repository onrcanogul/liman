package secrets

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func testBox(t *testing.T) *Box {
	t.Helper()
	box, err := NewBox(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// --- Box ---------------------------------------------------------------------

func TestSealAndOpen(t *testing.T) {
	box := testBox(t)
	context := []byte("web\x00DATABASE_URL")

	for _, plaintext := range []string{"hunter2", "", "postgres://user:p@ss/db?sslmode=require", strings.Repeat("x", 10_000), "çok gizli ünicode"} {
		sealed, err := box.Seal([]byte(plaintext), context)
		if err != nil {
			t.Fatalf("Seal() error = %v", err)
		}
		if plaintext != "" && bytes.Contains(sealed, []byte(plaintext)) {
			t.Errorf("the sealed value contains the plaintext %q", plaintext)
		}

		got, err := box.Open(sealed, context)
		if err != nil || string(got) != plaintext {
			t.Errorf("Open() = %q, %v; want %q", got, err, plaintext)
		}
	}
}

func TestSealingTheSameValueTwiceGivesDifferentResults(t *testing.T) {
	box := testBox(t)

	// If equal plaintexts produced equal ciphertexts, anyone reading the
	// database could tell which apps share a password.
	first, _ := box.Seal([]byte("same"), []byte("ctx"))
	second, _ := box.Seal([]byte("same"), []byte("ctx"))

	if bytes.Equal(first, second) {
		t.Error("two seals of the same value are identical")
	}
}

func TestOpenRejectsAnythingThatWasChanged(t *testing.T) {
	box := testBox(t)
	context := []byte("web\x00API_KEY")
	sealed, err := box.Seal([]byte("the-secret-value"), context)
	if err != nil {
		t.Fatal(err)
	}

	// Flip one bit at every position in turn: the format byte, the nonce,
	// the ciphertext and the authentication tag.
	for i := range sealed {
		tampered := slices.Clone(sealed)
		tampered[i] ^= 0x01

		if _, err := box.Open(tampered, context); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("Open() accepted a value with byte %d changed (error %v)", i, err)
		}
	}

	tests := []struct {
		name    string
		sealed  []byte
		context []byte
		box     *Box
	}{
		{"another context (value moved to another app)", sealed, []byte("other\x00API_KEY"), box},
		{"another context (value moved to another name)", sealed, []byte("web\x00OTHER"), box},
		{"another key", sealed, context, testBox(t)},
		{"truncated", sealed[:len(sealed)-1], context, box},
		{"too short to be valid", sealed[:5], context, box},
		{"empty", nil, context, box},
		{"extended", append(slices.Clone(sealed), 0), context, box},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.box.Open(tt.sealed, tt.context)
			if !errors.Is(err, ErrDecrypt) || got != nil {
				t.Errorf("Open() = %q, %v; want ErrDecrypt and no data", got, err)
			}
		})
	}
}

func TestNewBoxRejectsWrongKeySize(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33, 64} {
		if _, err := NewBox(make([]byte, size)); err == nil {
			t.Errorf("NewBox() accepted a %d-byte key", size)
		}
	}
}

// --- key file -----------------------------------------------------------------

func TestCreateAndLoadKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "master.key")

	if exists, err := KeyExists(path); err != nil || exists {
		t.Fatalf("KeyExists() before creation = %v, %v", exists, err)
	}

	created, err := CreateKey(path)
	if err != nil {
		t.Fatalf("CreateKey() error = %v", err)
	}
	if len(created) != KeySize {
		t.Errorf("key has %d bytes, want %d", len(created), KeySize)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}

	loaded, err := LoadKey(path)
	if err != nil || !bytes.Equal(loaded, created) {
		t.Errorf("LoadKey() returned a different key (error %v)", err)
	}
	if exists, _ := KeyExists(path); !exists {
		t.Error("KeyExists() after creation = false")
	}
}

func TestCreateKeyNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	first, err := CreateKey(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := CreateKey(path); err == nil {
		t.Fatal("CreateKey() replaced an existing key")
	}

	// The original key must be intact.
	loaded, err := LoadKey(path)
	if err != nil || !bytes.Equal(loaded, first) {
		t.Errorf("the key changed after a refused overwrite (error %v)", err)
	}
}

func TestTwoKeysAreDifferent(t *testing.T) {
	dir := t.TempDir()
	a, _ := CreateKey(filepath.Join(dir, "a.key"))
	b, _ := CreateKey(filepath.Join(dir, "b.key"))
	if bytes.Equal(a, b) {
		t.Error("two generated keys are identical")
	}
}

func TestLoadKeyRejectsUnsafeOrBrokenFiles(t *testing.T) {
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		wantErr string
	}{
		{"readable by group", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 0o640, "readable by others"},
		{"readable by everyone", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 0o644, "readable by others"},
		{"not base64", "this is not a key!", 0o600, "not valid base64"},
		{"too short", "c2hvcnQ=", 0o600, "holds 5 bytes"},
		{"empty", "", 0o600, "holds 0 bytes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "master.key")
			if err := os.WriteFile(path, []byte(tt.content), tt.mode); err != nil {
				t.Fatal(err)
			}
			// WriteFile is subject to the umask; set the mode explicitly.
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}

			_, err := LoadKey(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("LoadKey() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}

	if _, err := LoadKey(filepath.Join(t.TempDir(), "missing.key")); err == nil {
		t.Error("LoadKey() of a missing file succeeded")
	}
}

// --- Vault ---------------------------------------------------------------------

// memoryStorage is a Storage that keeps sealed values in a map and lets
// tests look at exactly what was stored.
type memoryStorage struct {
	sealed map[string][]byte
	times  map[string]time.Time
}

func newMemoryStorage() *memoryStorage {
	return &memoryStorage{sealed: map[string][]byte{}, times: map[string]time.Time{}}
}

func (m *memoryStorage) PutSecret(_ context.Context, app, name string, sealed []byte) error {
	m.sealed[app+"/"+name] = sealed
	m.times[app+"/"+name] = time.Now()
	return nil
}

func (m *memoryStorage) GetSecret(_ context.Context, app, name string) ([]byte, bool, error) {
	sealed, ok := m.sealed[app+"/"+name]
	return sealed, ok, nil
}

func (m *memoryStorage) ListSecrets(_ context.Context, app string) ([]Info, error) {
	var out []Info
	for key, at := range m.times {
		if rest, ok := strings.CutPrefix(key, app+"/"); ok {
			out = append(out, Info{Name: rest, UpdatedAt: at})
		}
	}
	slices.SortFunc(out, func(a, b Info) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (m *memoryStorage) ListAllSecrets(_ context.Context) ([]Info, error) {
	var out []Info
	for key, at := range m.times {
		app, name, _ := strings.Cut(key, "/")
		out = append(out, Info{App: app, Name: name, UpdatedAt: at})
	}
	slices.SortFunc(out, func(a, b Info) int {
		return cmp.Or(strings.Compare(a.App, b.App), strings.Compare(a.Name, b.Name))
	})
	return out, nil
}

func (m *memoryStorage) DeleteSecret(_ context.Context, app, name string) (bool, error) {
	_, ok := m.sealed[app+"/"+name]
	delete(m.sealed, app+"/"+name)
	delete(m.times, app+"/"+name)
	return ok, nil
}

func testVault(t *testing.T) (*Vault, *memoryStorage) {
	t.Helper()
	storage := newMemoryStorage()
	vault, err := NewVault(testKey(t), storage)
	if err != nil {
		t.Fatal(err)
	}
	return vault, storage
}

func TestVaultSetGetListDelete(t *testing.T) {
	vault, storage := testVault(t)
	ctx := context.Background()

	if err := vault.Set(ctx, "web", "DATABASE_URL", []byte("postgres://secret")); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := vault.Set(ctx, "web", "API_KEY", []byte("k-123")); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := vault.Set(ctx, "other", "API_KEY", []byte("k-999")); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// What reached the storage is ciphertext.
	for key, sealed := range storage.sealed {
		for _, plaintext := range []string{"postgres://secret", "k-123", "k-999"} {
			if bytes.Contains(sealed, []byte(plaintext)) {
				t.Errorf("storage entry %s contains the plaintext %q", key, plaintext)
			}
		}
	}

	got, err := vault.Get(ctx, "web", "API_KEY")
	if err != nil || string(got) != "k-123" {
		t.Errorf("Get() = %q, %v; want k-123", got, err)
	}

	// Listing returns names only, per app.
	infos, err := vault.List(ctx, "web")
	if err != nil || len(infos) != 2 || infos[0].Name != "API_KEY" || infos[1].Name != "DATABASE_URL" {
		t.Errorf("List() = %+v, %v; want API_KEY and DATABASE_URL", infos, err)
	}

	// Setting again replaces the value.
	if err := vault.Set(ctx, "web", "API_KEY", []byte("k-456")); err != nil {
		t.Fatal(err)
	}
	if got, _ := vault.Get(ctx, "web", "API_KEY"); string(got) != "k-456" {
		t.Errorf("Get() after replacing = %q, want k-456", got)
	}

	if err := vault.Delete(ctx, "web", "API_KEY"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := vault.Get(ctx, "web", "API_KEY"); !errors.Is(err, ErrNotSet) {
		t.Errorf("Get() after Delete error = %v, want ErrNotSet", err)
	}
	if err := vault.Delete(ctx, "web", "API_KEY"); !errors.Is(err, ErrNotSet) {
		t.Errorf("Delete() twice error = %v, want ErrNotSet", err)
	}
	// The other app's secret of the same name is untouched.
	if got, _ := vault.Get(ctx, "other", "API_KEY"); string(got) != "k-999" {
		t.Errorf("another app's secret = %q, want it untouched", got)
	}
}

func TestVaultRejectsBadInput(t *testing.T) {
	vault, storage := testVault(t)
	ctx := context.Background()

	tests := []struct {
		name    string
		secret  string
		value   []byte
		wantErr string
	}{
		{"empty value", "KEY", nil, "is empty"},
		{"value with NUL", "KEY", []byte("a\x00b"), "NUL byte"},
		{"value too large", "KEY", bytes.Repeat([]byte("x"), MaxValueBytes+1), "more than the limit"},
		{"name with dash", "MY-KEY", []byte("v"), "invalid secret name"},
		{"name starting with digit", "1KEY", []byte("v"), "invalid secret name"},
		{"name with path", "../KEY", []byte("v"), "invalid secret name"},
		{"empty name", "", []byte("v"), "invalid secret name"},
		{"name too long", strings.Repeat("K", 129), []byte("v"), "invalid secret name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := vault.Set(ctx, "web", tt.secret, tt.value)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Set() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
	if len(storage.sealed) != 0 {
		t.Errorf("rejected values reached the storage: %d entries", len(storage.sealed))
	}

	// A value of exactly the limit is fine.
	if err := vault.Set(ctx, "web", "BIG", bytes.Repeat([]byte("x"), MaxValueBytes)); err != nil {
		t.Errorf("Set() of a value at the limit error = %v", err)
	}
}

func TestSealedValueCannotBeMovedBetweenAppsOrNames(t *testing.T) {
	vault, storage := testVault(t)
	ctx := context.Background()
	if err := vault.Set(ctx, "prod", "DB_PASSWORD", []byte("prod-password")); err != nil {
		t.Fatal(err)
	}
	if err := vault.Set(ctx, "attacker", "DB_PASSWORD", []byte("mine")); err != nil {
		t.Fatal(err)
	}

	// Someone who can write to the database (but does not have the key)
	// copies the production ciphertext into a row of an app they control,
	// hoping a deploy of that app will hand them the plaintext.
	storage.sealed["attacker/DB_PASSWORD"] = storage.sealed["prod/DB_PASSWORD"]

	if got, err := vault.Get(ctx, "attacker", "DB_PASSWORD"); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Get() of a moved ciphertext = %q, %v; want ErrDecrypt", got, err)
	}

	// The same within one app, under another name.
	storage.sealed["prod/PUBLIC_NOTE"] = storage.sealed["prod/DB_PASSWORD"]
	if _, err := vault.Get(ctx, "prod", "PUBLIC_NOTE"); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Get() of a renamed ciphertext error = %v, want ErrDecrypt", err)
	}
}

func TestVaultWithAnotherKeyCannotRead(t *testing.T) {
	storage := newMemoryStorage()
	first, _ := NewVault(testKey(t), storage)
	second, _ := NewVault(testKey(t), storage)
	ctx := context.Background()

	if err := first.Set(ctx, "web", "KEY", []byte("value")); err != nil {
		t.Fatal(err)
	}

	got, err := second.Get(ctx, "web", "KEY")
	if !errors.Is(err, ErrDecrypt) || got != nil {
		t.Errorf("Get() with another key = %q, %v; want ErrDecrypt", got, err)
	}
	// The error names the secret but not its value.
	if !strings.Contains(err.Error(), "KEY") {
		t.Errorf("error %q does not say which secret failed", err)
	}
}

func TestResolve(t *testing.T) {
	vault, _ := testVault(t)
	ctx := context.Background()
	_ = vault.Set(ctx, "web", "DATABASE_URL", []byte("postgres://x"))
	_ = vault.Set(ctx, "web", "API_KEY", []byte("k-1"))

	got, err := vault.Resolve(ctx, "web", []string{"API_KEY", "DATABASE_URL"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got) != 2 || got["API_KEY"] != "k-1" || got["DATABASE_URL"] != "postgres://x" {
		t.Errorf("Resolve() = %v", got)
	}

	// Nothing to resolve is not an error.
	if got, err := vault.Resolve(ctx, "web", nil); err != nil || len(got) != 0 {
		t.Errorf("Resolve(nothing) = %v, %v", got, err)
	}

	// All missing names are reported together, sorted.
	_, err = vault.Resolve(ctx, "web", []string{"SMTP_PASSWORD", "API_KEY", "JWT_SECRET"})
	var missing *MissingError
	if !errors.As(err, &missing) || !slices.Equal(missing.Names, []string{"JWT_SECRET", "SMTP_PASSWORD"}) {
		t.Fatalf("Resolve() error = %v, want the two missing names", err)
	}
	if !errors.Is(err, ErrNotSet) {
		t.Error("a MissingError does not match ErrNotSet")
	}
	if want := "liman secret set web <NAME>"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not say how to fix it", err)
	}

	names, err := vault.Missing(ctx, "another-app", []string{"API_KEY"})
	if err != nil || !slices.Equal(names, []string{"API_KEY"}) {
		t.Errorf("Missing() for an app without secrets = %v, %v", names, err)
	}
}
