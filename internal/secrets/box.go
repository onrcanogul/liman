// Package secrets keeps the secret values of apps (passwords, API keys)
// encrypted at rest and hands them to a deploy as environment variables.
//
// It deliberately contains no cryptography of its own. It combines two
// standard building blocks from Go's standard library, AES and GCM, in the
// one way they are meant to be combined; everything subtle about them is
// inside the library.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeySize is the length of the master key in bytes: 256 bits.
const KeySize = 32

// The first byte of every sealed value names its format, so that a change
// of algorithm or key can tell old values from new ones instead of failing
// to decrypt them.
const (
	// formatV1: AES-256-GCM, [1][12-byte nonce][ciphertext and tag]. It
	// does not say which key sealed it. Still read, no longer written.
	formatV1 = 1
	// formatV2: as v1, with the ID of the sealing key after the format
	// byte: [2][8-byte key ID][12-byte nonce][ciphertext and tag]. The
	// first nine bytes are authenticated too.
	formatV2 = 2
)

// KeyID identifies a master key without revealing anything about it.
type KeyID [8]byte

// String returns the ID in hex, as it appears in file names and listings.
func (id KeyID) String() string { return hex.EncodeToString(id[:]) }

// ParseKeyID reads an ID written by String.
func ParseKeyID(s string) (KeyID, error) {
	var id KeyID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id, fmt.Errorf("%q is not a key ID (16 hex digits)", s)
	}
	copy(id[:], b)
	return id, nil
}

// IDOf returns the ID of key: the first bytes of an HMAC of a fixed label,
// keyed with the key. Like any MAC output it says nothing about the key,
// and the label keeps it from being the same value as anything else
// computed from the key (a hash of it, say, or the backup signing key).
func IDOf(key []byte) KeyID {
	var id KeyID
	copy(id[:], Derive(key, "liman key id v1"))
	return id
}

// Derive returns a key for another purpose, computed from a master key and
// a label naming that purpose.
func Derive(key []byte, label string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

// ErrDecrypt is returned when a sealed value cannot be opened. It does not
// say why (wrong key, modified data, value moved from another row): telling
// an attacker which check failed is how decryption oracles are built.
var ErrDecrypt = errors.New("secret cannot be decrypted (wrong key or modified data)")

// Box encrypts and decrypts values with one key.
type Box struct {
	// aead is "authenticated encryption with associated data": it keeps a
	// value confidential and also detects any change to it. Encryption
	// without authentication is not enough: an attacker who cannot read a
	// value can often still flip bits in it in useful ways.
	aead cipher.AEAD
	id   KeyID
}

// ID returns the ID of the box's key.
func (b *Box) ID() KeyID { return b.id }

// NewBox returns a Box that uses key, which must be KeySize bytes long.
func NewBox(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("the master key must be %d bytes, got %d", KeySize, len(key))
	}

	// AES is the block cipher; with a 32-byte key this is AES-256.
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	// GCM is the mode that turns the block cipher into authenticated
	// encryption for messages of any length.
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return &Box{aead: aead, id: IDOf(key)}, nil
}

// Seal encrypts plaintext. The result is safe to store: it reveals nothing
// about the plaintext except its length.
//
// context is not secret and is not stored in the result, but the same
// context must be given to Open. It ties the sealed value to its place: a
// value sealed for one app and name cannot be opened as another, so nobody
// with write access to the database can swap ciphertexts between rows.
func (b *Box) Seal(plaintext, context []byte) ([]byte, error) {
	// A nonce is a "number used once". With GCM, using the same nonce twice
	// under one key is catastrophic: it reveals the XOR of the two
	// plaintexts and lets an attacker forge messages. A random 96-bit nonce
	// per value makes a repeat astronomically unlikely for the number of
	// values this tool will ever seal.
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	// The header (format and key ID) and the nonce are not secret; they are
	// stored in front of the ciphertext so that Open can find them. Seal
	// appends the ciphertext and its authentication tag to the slice given
	// as its first argument.
	header := b.header()
	out := make([]byte, 0, len(header)+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, header...)
	out = append(out, nonce...)
	// The header goes into the associated data: a value whose key ID was
	// changed fails to open, rather than being tried with another key.
	return b.aead.Seal(out, nonce, plaintext, v2Data(header, context)), nil
}

// header returns the v2 header: the format byte and the key ID.
func (b *Box) header() []byte {
	return append([]byte{formatV2}, b.id[:]...)
}

// v2Data is the associated data of a v2 value. The header has a fixed
// length, so header and context cannot be confused with another split.
func v2Data(header, context []byte) []byte {
	return append(append([]byte{}, header...), context...)
}

// SealedWith returns the ID of the key a v2 value names, and false for a
// v1 value (or anything else), which does not say.
func SealedWith(sealed []byte) (KeyID, bool) {
	var id KeyID
	if len(sealed) < 1+len(id) || sealed[0] != formatV2 {
		return id, false
	}
	copy(id[:], sealed[1:1+len(id)])
	return id, true
}

// Open decrypts a value produced by Seal with the same key and context. It
// reads both formats; a v2 value sealed with another key is refused
// without trying.
func (b *Box) Open(sealed, context []byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, ErrDecrypt
	}
	var header, data []byte
	switch sealed[0] {
	case formatV1:
		header, data = sealed[:1], context
	case formatV2:
		if id, _ := SealedWith(sealed); id != b.id {
			return nil, ErrDecrypt
		}
		header = sealed[:1+len(b.id)]
		data = v2Data(header, context)
	default:
		return nil, ErrDecrypt
	}

	nonceSize := b.aead.NonceSize()
	if len(sealed) < len(header)+nonceSize+b.aead.Overhead() {
		return nil, ErrDecrypt
	}
	nonce, ciphertext := sealed[len(header):len(header)+nonceSize], sealed[len(header)+nonceSize:]

	// Open verifies the authentication tag before it returns a single byte
	// of plaintext. If the key, the context, the header, the nonce or any
	// bit of the ciphertext is not what Seal used, it fails.
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, data)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}
