package secrets

import (
	"fmt"
	"strings"

	"filippo.io/age"
)

// Backups are encrypted in the age format (age-encryption.org/v1): an open,
// reviewed file format with chunked authenticated encryption, which the
// standard "age" tool can also read. Every master key has an age identity,
// derived from it like the backup signing key: a backup is encrypted to
// the primary key's identity and opened with whichever key of the keyring
// matches, so old backups open after a rotation as long as the old key is
// kept.
const backupEncryptionLabel = "liman backup encryption v1"

// backupIdentity returns the age identity derived from key.
//
// age builds an X25519 identity only from its text form, the 32-byte
// secret in bech32 with the prefix AGE-SECRET-KEY-. The derived bytes are
// put into that form here; age's own parser then checks the checksum.
func backupIdentity(key []byte) (*age.X25519Identity, error) {
	text := strings.ToUpper(bech32Encode("age-secret-key-", Derive(key, backupEncryptionLabel)))
	id, err := age.ParseX25519Identity(text)
	if err != nil {
		return nil, fmt.Errorf("derive the backup identity: %w", err)
	}
	return id, nil
}

// BackupRecipient returns the age recipient backups are encrypted to: the
// primary key's.
func (r *Keyring) BackupRecipient() (age.Recipient, error) {
	id, err := backupIdentity(r.keys[r.Primary()])
	if err != nil {
		return nil, err
	}
	return id.Recipient(), nil
}

// BackupIdentities returns the age identities of every key, the primary
// first: what a backup is opened with.
func (r *Keyring) BackupIdentities() ([]age.Identity, error) {
	var ids []age.Identity
	for _, kid := range r.IDs() {
		id, err := backupIdentity(r.keys[kid])
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// BackupIdentityFile returns the identities in the format of an age
// identity file, each preceded by a comment naming its master key, for
// opening backups with the age tool when liman is not at hand.
func (r *Keyring) BackupIdentityFile() (string, error) {
	var b strings.Builder
	b.WriteString("# age identities for liman backups. Whoever has this file can read every\n# backup made with these keys: keep it like the master key.\n")
	for _, kid := range r.IDs() {
		id, err := backupIdentity(r.keys[kid])
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "# master key %s\n%s\n", kid, id)
	}
	return b.String(), nil
}

// bech32Encode encodes data in bech32 (BIP-173) with the human-readable
// part hrp, the encoding age uses for its keys.
func bech32Encode(hrp string, data []byte) string {
	const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

	// Regroup the 8-bit bytes into 5-bit values, padding the last one.
	var values []byte
	acc, bits := 0, 0
	for _, b := range data {
		acc = acc<<8 | int(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			values = append(values, byte(acc>>bits&31))
		}
		acc &= 1<<bits - 1
	}
	if bits > 0 {
		values = append(values, byte(acc<<(5-bits)&31))
	}

	// The checksum covers the expanded hrp, the data and six zeros.
	expanded := make([]byte, 0, 2*len(hrp)+1)
	for _, c := range hrp {
		expanded = append(expanded, byte(c>>5))
	}
	expanded = append(expanded, 0)
	for _, c := range hrp {
		expanded = append(expanded, byte(c&31))
	}
	sum := bech32Polymod(append(append(expanded, values...), 0, 0, 0, 0, 0, 0)) ^ 1

	var out strings.Builder
	out.WriteString(hrp)
	out.WriteByte('1')
	for _, v := range values {
		out.WriteByte(charset[v])
	}
	for i := 0; i < 6; i++ {
		out.WriteByte(charset[sum>>(5*(5-i))&31])
	}
	return out.String()
}

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>i)&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}
