package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sealV1 seals the way liman did before key IDs: [1][nonce][ciphertext].
func sealV1(t *testing.T, key, plaintext, context []byte) []byte {
	t.Helper()
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	return aead.Seal(append([]byte{formatV1}, nonce...), nonce, plaintext, context)
}

func TestSealNamesTheKey(t *testing.T) {
	key := testKey(t)
	box, _ := NewBox(key)
	sealed, err := box.Seal([]byte("value"), []byte("ctx"))
	if err != nil {
		t.Fatal(err)
	}
	if sealed[0] != formatV2 {
		t.Errorf("format byte = %d, want %d", sealed[0], formatV2)
	}
	if id, ok := SealedWith(sealed); !ok || id != IDOf(key) || id != box.ID() {
		t.Errorf("SealedWith() = %s, %v; want %s", id, ok, IDOf(key))
	}
	if IDOf(key) == IDOf(testKey(t)) {
		t.Error("two keys have the same ID")
	}
	if got, err := ParseKeyID(IDOf(key).String()); err != nil || got != IDOf(key) {
		t.Errorf("ParseKeyID(String()) = %s, %v", got, err)
	}
}

func TestBoxStillOpensV1(t *testing.T) {
	key := testKey(t)
	box, _ := NewBox(key)
	old := sealV1(t, key, []byte("from before"), []byte("web\x00KEY"))
	if got, err := box.Open(old, []byte("web\x00KEY")); err != nil || string(got) != "from before" {
		t.Errorf("Open(v1) = %q, %v", got, err)
	}
	if _, ok := SealedWith(old); ok {
		t.Error("a v1 value claims to name its key")
	}
}

func TestKeyringOpensWithTheKeyAValueNames(t *testing.T) {
	oldKey, newKey := testKey(t), testKey(t)
	before, _ := NewKeyring(oldKey)
	after, err := NewKeyring(newKey, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := []byte("web\x00KEY")

	v2old, _ := before.Seal([]byte("sealed before"), ctx)
	v1old := sealV1(t, oldKey, []byte("sealed long before"), ctx)
	v2new, _ := after.Seal([]byte("sealed after"), ctx)

	for name, tt := range map[string]struct {
		sealed []byte
		want   string
		by     KeyID
	}{
		"v2, old key": {v2old, "sealed before", IDOf(oldKey)},
		"v1, old key": {v1old, "sealed long before", IDOf(oldKey)},
		"v2, new key": {v2new, "sealed after", IDOf(newKey)},
	} {
		if got, err := after.Open(tt.sealed, ctx); err != nil || string(got) != tt.want {
			t.Errorf("%s: Open() = %q, %v", name, got, err)
		}
		if by, err := after.SealedBy(tt.sealed, ctx); err != nil || by != tt.by {
			t.Errorf("%s: SealedBy() = %s, %v; want %s", name, by, err, tt.by)
		}
	}
	if id, _ := SealedWith(v2new); id != after.Primary() {
		t.Errorf("Seal used %s, not the primary %s", id, after.Primary())
	}

	// A keyring without the old key says which key it would need.
	alone, _ := NewKeyring(newKey)
	_, err = alone.Open(v2old, ctx)
	var unknown *UnknownKeyError
	if !errors.As(err, &unknown) || unknown.ID != IDOf(oldKey) || !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open() without the key = %v; want an UnknownKeyError for %s", err, IDOf(oldKey))
	}
	// The key ID is authenticated: pointing a value at another key the
	// ring holds does not make that key try it.
	moved := slices.Clone(v2old)
	newID := IDOf(newKey)
	copy(moved[1:9], newID[:])
	if _, err := after.Open(moved, ctx); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open() of a value whose key ID was changed = %v, want ErrDecrypt", err)
	}
}

// --- files -----------------------------------------------------------------

func TestLoadKeyring(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	primary, err := CreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := LoadKeyring(path)
	if err != nil || ring.Primary() != IDOf(primary) || len(ring.IDs()) != 1 {
		t.Fatalf("LoadKeyring() with one key = %v, %v", ring.IDs(), err)
	}

	// An old key, filed under its ID.
	if err := os.MkdirAll(OldKeysDir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := testKey(t)
	if err := writeKeyFile(filepath.Join(OldKeysDir(path), IDOf(old).String()+".key"), old); err != nil {
		t.Fatal(err)
	}
	ring, err = LoadKeyring(path)
	if err != nil || !ring.Has(IDOf(old)) || ring.Primary() != IDOf(primary) {
		t.Fatalf("LoadKeyring() with an old key = %v, %v", ring.IDs(), err)
	}
	if _, pending := ring.Pending(); pending {
		t.Error("Pending() without a rotation in progress")
	}

	// A file whose name is another key's ID is refused.
	other := testKey(t)
	if err := writeKeyFile(filepath.Join(OldKeysDir(path), IDOf(primary).String()+".key"), other); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(path); err == nil || !strings.Contains(err.Error(), "not the one its name says") {
		t.Errorf("LoadKeyring() with a misnamed key = %v", err)
	}
}
