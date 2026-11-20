package secrets

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestBech32Encode(t *testing.T) {
	// Test vectors from BIP-173.
	if got := bech32Encode("a", nil); got != "a12uel5l" {
		t.Errorf("empty data = %q, want a12uel5l", got)
	}
	// The 5-bit values 0..31 in order, as 20 bytes.
	data := []byte{0x00, 0x44, 0x32, 0x14, 0xc7, 0x42, 0x54, 0xb6, 0x35, 0xcf, 0x84, 0x65, 0x3a, 0x56, 0xd7, 0xc6, 0x75, 0xbe, 0x77, 0xdf}
	if got := bech32Encode("abcdef", data); got != "abcdef1qpzry9x8gf2tvdw0s3jn54khce6mua7lmqqqxw" {
		t.Errorf("abcdef = %q", got)
	}
}

func TestBackupIdentities(t *testing.T) {
	oldKey, newKey := testKey(t), testKey(t)
	before, _ := NewKeyring(oldKey)
	after, _ := NewKeyring(newKey, oldKey)

	encrypt := func(ring *Keyring, text string) []byte {
		t.Helper()
		r, err := ring.BackupRecipient()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		w, err := age.Encrypt(&buf, r)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, text)
		_ = w.Close()
		return buf.Bytes()
	}
	open := func(ring *Keyring, data []byte) (string, error) {
		ids, err := ring.BackupIdentities()
		if err != nil {
			return "", err
		}
		r, err := age.Decrypt(bytes.NewReader(data), ids...)
		if err != nil {
			return "", err
		}
		b, err := io.ReadAll(r)
		return string(b), err
	}

	// Made before the rotation, opened after it with the old key kept.
	old := encrypt(before, "made with the old key")
	if got, err := open(after, old); err != nil || got != "made with the old key" {
		t.Errorf("an old backup after the rotation: %q, %v", got, err)
	}
	// A keyring without that key cannot.
	alone, _ := NewKeyring(newKey)
	if _, err := open(alone, old); err == nil {
		t.Error("a backup opened without its key")
	}
	// The same key always gives the same identity.
	a, _ := backupIdentity(newKey)
	b, _ := backupIdentity(newKey)
	if a.String() != b.String() || !strings.HasPrefix(a.String(), "AGE-SECRET-KEY-1") {
		t.Errorf("identities %q and %q", a, b)
	}

	file, err := after.BackupIdentityFile()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := age.ParseIdentities(strings.NewReader(file))
	if err != nil || len(parsed) != 2 {
		t.Fatalf("the identity file does not parse as age identities: %d, %v", len(parsed), err)
	}
	r, err := age.Decrypt(bytes.NewReader(old), parsed...)
	if err != nil {
		t.Fatalf("the age tool's way of reading identities cannot open the backup: %v", err)
	}
	if got, _ := io.ReadAll(r); string(got) != "made with the old key" {
		t.Errorf("opened with the identity file: %q", got)
	}
}
