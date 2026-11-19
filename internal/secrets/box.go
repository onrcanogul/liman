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
	"crypto/rand"
	"errors"
	"fmt"
)

// KeySize is the length of the master key in bytes: 256 bits.
const KeySize = 32

// formatV1 is the first byte of every sealed value. It names the format
// (AES-256-GCM, 12-byte random nonce in front of the ciphertext), so that a
// future change of algorithm or key can tell old values from new ones
// instead of failing to decrypt them.
const formatV1 = 1

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
}

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
	return &Box{aead: aead}, nil
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

	// The nonce is not secret; it is stored in front of the ciphertext so
	// that Open can find it. Seal appends the ciphertext and its
	// authentication tag to the slice given as its first argument.
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, formatV1)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, context), nil
}

// Open decrypts a value produced by Seal with the same key and context.
func (b *Box) Open(sealed, context []byte) ([]byte, error) {
	nonceSize := b.aead.NonceSize()
	if len(sealed) < 1+nonceSize+b.aead.Overhead() || sealed[0] != formatV1 {
		return nil, ErrDecrypt
	}

	nonce, ciphertext := sealed[1:1+nonceSize], sealed[1+nonceSize:]

	// Open verifies the authentication tag before it returns a single byte
	// of plaintext. If the key, the context, the nonce or any bit of the
	// ciphertext is not what Seal used, it fails.
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, context)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}
