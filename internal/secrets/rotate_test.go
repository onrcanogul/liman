package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// memStore is a Resealer that keeps secrets in memory and changes all of
// them or none, like a transaction.
type memStore struct {
	rows map[string][]byte // "app/name" -> sealed
	fail error
}

func (m *memStore) ResealSecrets(_ context.Context, fn func(app, name string, sealed []byte) ([]byte, bool, error)) (int, error) {
	if m.fail != nil {
		return 0, m.fail
	}
	keys := make([]string, 0, len(m.rows))
	for k := range m.rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	next := map[string][]byte{}
	n := 0
	for _, k := range keys {
		app, name, _ := strings.Cut(k, "/")
		sealed, changed, err := fn(app, name, m.rows[k])
		if err != nil {
			return 0, err
		}
		next[k] = sealed
		if changed {
			n++
		}
	}
	m.rows = next
	return n, nil
}

// put seals value for app/name with ring.
func (m *memStore) put(t *testing.T, ring *Keyring, app, name, value string) {
	t.Helper()
	sealed, err := ring.Seal([]byte(value), sealContext(app, name))
	if err != nil {
		t.Fatal(err)
	}
	m.rows[app+"/"+name] = sealed
}

// check opens every row with the keyring at path and wants all of them
// sealed with its primary key.
func (m *memStore) check(t *testing.T, path string) {
	t.Helper()
	ring, err := LoadKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, sealed := range m.rows {
		app, name, _ := strings.Cut(k, "/")
		if _, err := ring.Open(sealed, sealContext(app, name)); err != nil {
			t.Errorf("%s does not open: %v", k, err)
		}
		if id, _ := SealedWith(sealed); id != ring.Primary() {
			t.Errorf("%s is sealed with %s, not the primary %s", k, id, ring.Primary())
		}
	}
}

func TestRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	oldKey, err := CreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	oldRing, _ := NewKeyring(oldKey)
	st := &memStore{rows: map[string][]byte{}}
	st.put(t, oldRing, "web", "API_KEY", "a")
	st.put(t, oldRing, "api", "DATABASE_URL", "b")
	st.rows["api/LEGACY"] = sealV1(t, oldKey, []byte("c"), sealContext("api", "LEGACY"))

	rot, err := Rotate(context.Background(), path, st)
	if err != nil {
		t.Fatal(err)
	}
	if rot.Old != IDOf(oldKey) || rot.New == rot.Old || rot.Resealed != 3 || rot.Resumed {
		t.Errorf("Rotate() = %+v", rot)
	}
	newKey, _ := LoadKey(path)
	if IDOf(newKey) != rot.New {
		t.Errorf("master.key holds %s, want %s", IDOf(newKey), rot.New)
	}
	if exists, _ := KeyExists(PendingKeyPath(path)); exists {
		t.Error("the pending key is still there")
	}
	kept, err := LoadKey(filepath.Join(OldKeysDir(path), rot.Old.String()+".key"))
	if err != nil || IDOf(kept) != rot.Old {
		t.Errorf("the old key was not kept: %v", err)
	}
	if st, err := os.Stat(filepath.Join(OldKeysDir(path), rot.Old.String()+".key")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("old key file mode = %v, %v", st.Mode().Perm(), err)
	}
	st.check(t, path)

	// And again: the first new key becomes old in its turn.
	rot2, err := Rotate(context.Background(), path, st)
	if err != nil || rot2.Old != rot.New || rot2.Resealed != 3 {
		t.Errorf("second Rotate() = %+v, %v", rot2, err)
	}
	ring, _ := LoadKeyring(path)
	if len(ring.IDs()) != 3 {
		t.Errorf("keyring after two rotations = %v, want three keys", ring.IDs())
	}
	st.check(t, path)
}

func TestRotateThatFailsChangesNothingAndCanBeResumed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	oldKey, _ := CreateKey(path)
	oldRing, _ := NewKeyring(oldKey)
	st := &memStore{rows: map[string][]byte{}}
	st.put(t, oldRing, "web", "API_KEY", "a")
	before := fmt.Sprint(st.rows)

	st.fail = errors.New("disk full")
	if _, err := Rotate(context.Background(), path, st); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("Rotate() with a failing store = %v", err)
	}
	if fmt.Sprint(st.rows) != before {
		t.Error("the secrets changed although the rotation failed")
	}
	pending, err := LoadKey(PendingKeyPath(path))
	if err != nil {
		t.Fatalf("the pending key is not there to resume with: %v", err)
	}
	// Meanwhile everything still opens, and the daemon can tell that a
	// rotation is unfinished.
	ring, err := LoadKeyring(path)
	if id, ok := ring.Pending(); err != nil || !ok || id != IDOf(pending) || ring.Primary() != IDOf(oldKey) {
		t.Fatalf("LoadKeyring() mid-rotation = %v, %s pending %v", err, id, ok)
	}

	st.fail = nil
	rot, err := Rotate(context.Background(), path, st)
	if err != nil || !rot.Resumed || rot.New != IDOf(pending) {
		t.Fatalf("resumed Rotate() = %+v, %v; want it to finish with %s", rot, err, IDOf(pending))
	}
	st.check(t, path)
}

func TestRotateInterruptedAfterTheDatabase(t *testing.T) {
	// The state a crash between steps 3 and 4 leaves: the secrets are
	// sealed with the new key, which is still only the pending one.
	path := filepath.Join(t.TempDir(), "master.key")
	oldKey, _ := CreateKey(path)
	pending, _ := CreateKey(PendingKeyPath(path))
	pendingRing, _ := NewKeyring(pending)
	st := &memStore{rows: map[string][]byte{}}
	st.put(t, pendingRing, "web", "API_KEY", "a")

	// Everything opens in this state.
	ring, err := LoadKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Open(st.rows["web/API_KEY"], sealContext("web", "API_KEY")); err != nil {
		t.Errorf("a value sealed by the unfinished rotation does not open: %v", err)
	}

	rot, err := Rotate(context.Background(), path, st)
	if err != nil || !rot.Resumed || rot.Old != IDOf(oldKey) || rot.New != IDOf(pending) || rot.Resealed != 0 {
		t.Fatalf("Rotate() = %+v, %v; want it to finish without resealing again", rot, err)
	}
	st.check(t, path)
}

func TestRotateRefusesWhatItCannotOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if _, err := CreateKey(path); err != nil {
		t.Fatal(err)
	}
	stranger, _ := NewKeyring(testKey(t))
	st := &memStore{rows: map[string][]byte{}}
	st.put(t, stranger, "web", "FOREIGN", "x")
	before := fmt.Sprint(st.rows)

	_, err := Rotate(context.Background(), path, st)
	if err == nil || !strings.Contains(err.Error(), "web/FOREIGN") || !strings.Contains(err.Error(), "which this server does not have") {
		t.Fatalf("Rotate() = %v; want it to name the value and the missing key", err)
	}
	if fmt.Sprint(st.rows) != before {
		t.Error("the secrets changed")
	}
}
