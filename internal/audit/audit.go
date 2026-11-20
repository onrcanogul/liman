// Package audit keeps the audit trail: who changed what, when, and how it
// ended. The operational log answers "what is happening"; this answers
// "who did that", for longer, in the database (and so in its backups), and
// in a form where a row changed or removed later shows.
//
// Every row is signed together with the signature of the row before it:
//
//	mac_n = HMAC(K, mac_(n-1) || encoding of row n)
//
// with K derived from the master key ("liman audit v1"), so that changing,
// removing, inserting or reordering a row breaks every signature after it,
// and nobody without the master key can sign a rewritten trail. What the
// chain cannot tell is rows cut off at the end, or the whole database
// replaced by an older copy: what is left is consistent. For that, every
// row also goes to the log with its signature, an anchor outside the
// database.
package audit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os/user"
	"strconv"
	"time"

	"github.com/onrcanogul/liman/internal/secrets"
	"github.com/onrcanogul/liman/internal/store"
)

// label derives the signing key from a master key, and starts every
// encoded row: a change of the encoding is a change of this label.
const label = "liman audit v1"

// Actions.
const (
	Deploy        = "deploy"
	Rollback      = "rollback"
	Stop          = "stop"
	Restart       = "restart"
	SecretSet     = "secret.set"
	SecretDelete  = "secret.delete"
	TokenCreate   = "token.create"
	TokenRevoke   = "token.revoke"
	BackupCreate  = "backup.create"
	BackupRestore = "backup.restore"
	KeyRotate     = "key.rotate"
	KeyRetire     = "key.retire"
	CertObtain    = "cert.obtain"
)

// Actors that are not a token or a person.
const (
	Monitor = "monitor"
	Renewer = "renewer"
)

// TokenActor names a token as an actor. The name alone is not enough: a
// revoked token's name can be given to a new one, so its creation time
// tells the two apart.
func TokenActor(name string, created time.Time) string {
	return "token:" + name + "@" + created.UTC().Format(time.RFC3339)
}

// ServerActor names whoever runs a limand command on the server: the
// operating system user, as the process sees it.
func ServerActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "server:" + u.Username
	}
	return "server:unknown"
}

// maxErrorBytes bounds the error text kept with a row.
const maxErrorBytes = 1000

// Recorder writes rows. A nil Recorder writes nothing, so that code with
// nothing to record into (tests, a daemon without a store) needs no checks.
type Recorder struct {
	Store *store.Store
	// Keys returns the master keys to sign with; nil when there is no
	// master key. It is called for every row, so a key rotated meanwhile
	// is picked up without a restart.
	Keys func() (*secrets.Keyring, error)
	// Log receives the anchor line of every row, and the errors.
	Log *slog.Logger
	// Now returns the time; nil means time.Now.
	Now func() time.Time
}

// KeysAt returns a Keys function for the master key at path: no keyring
// when the file does not exist.
func KeysAt(path string) func() (*secrets.Keyring, error) {
	return func() (*secrets.Keyring, error) {
		exists, err := secrets.KeyExists(path)
		if err != nil || !exists {
			return nil, err
		}
		return secrets.LoadKeyring(path)
	}
}

// Record writes one row: actor did action to target (in app, or the
// daemon as a whole when app is ""), and failed with err if it is not nil.
//
// It is called after the action, with its outcome. A row that cannot be
// written does not undo the action; it is logged as an error, loudly, and
// the action stands.
func (r *Recorder) Record(ctx context.Context, actor, action, app, target string, err error) {
	if r == nil || r.Store == nil {
		return
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	e := store.AuditEntry{At: now(), Actor: actor, Action: action, App: app, Target: target, OK: err == nil}
	if err != nil {
		e.Error = err.Error()
		if len(e.Error) > maxErrorBytes {
			e.Error = e.Error[:maxErrorBytes]
		}
	}
	log := r.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	fail := func(err error) {
		log.Error("audit entry not written", "actor", actor, "action", action, "app", app, "target", target, "error", err)
	}

	var ring *secrets.Keyring
	if r.Keys != nil {
		var kerr error
		// A key that cannot be read is not a reason to write the row
		// unsigned: that would be a quiet downgrade, and an unsigned row
		// after signed ones reads as tampering.
		if ring, kerr = r.Keys(); kerr != nil {
			fail(kerr)
			return
		}
	}

	// The action is done whether or not the client is still there, so
	// its row is written regardless; but not forever.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	stored, aerr := r.Store.AppendAudit(ctx, e, Signer(ring))
	if aerr != nil {
		fail(aerr)
		return
	}
	log.Info("audit", "seq", stored.Seq, "actor", actor, "action", action, "app", app, "target", target,
		"ok", stored.OK, "mac", hex.EncodeToString(stored.MAC))
}

// Signer signs rows with the primary key of ring, or, without a ring,
// hashes them: a chain that shows accidental damage, but that anybody can
// recompute.
func Signer(ring *secrets.Keyring) store.AuditSigner {
	return func(prev []byte, e store.AuditEntry) (string, []byte, error) {
		if ring == nil {
			e.KeyID = ""
			sum := sha256.Sum256(encode(prev, e))
			return "", sum[:], nil
		}
		id := ring.Primary()
		e.KeyID = id.String()
		key, ok := ring.Derive(id, label)
		if !ok {
			return "", nil, fmt.Errorf("no primary key")
		}
		return e.KeyID, sign(key, prev, e), nil
	}
}

func sign(key, prev []byte, e store.AuditEntry) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(encode(prev, e))
	return m.Sum(nil)
}

// encode turns a row and its predecessor's signature into the bytes that
// are signed. Each field is written with its length in front ("5:hello,"),
// so no value can be made to read as two fields, and the order is fixed.
// JSON would not do: the same data can be written as different bytes.
func encode(prev []byte, e store.AuditEntry) []byte {
	if prev == nil {
		prev = make([]byte, sha256.Size)
	}
	var b bytes.Buffer
	b.WriteString(label)
	b.WriteByte('\n')
	b.Write(prev)
	ok := "0"
	if e.OK {
		ok = "1"
	}
	for _, f := range []string{
		strconv.FormatInt(e.Seq, 10),
		e.At.UTC().Format(time.RFC3339Nano),
		e.Actor, e.Action, e.App, e.Target, ok, e.Error, e.KeyID,
	} {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteByte(':')
		b.WriteString(f)
		b.WriteByte(',')
	}
	return b.Bytes()
}

// Report is what Verify found.
type Report struct {
	// Entries is how many rows were read; LastSeq and LastMAC describe
	// the last one, to compare with the log's anchor lines.
	Entries int64
	LastSeq int64
	LastMAC string
	// Unsigned counts rows written without a master key.
	Unsigned int64
	// Unchecked counts, by key ID, rows signed with a key this server no
	// longer has: retired, or from another server.
	Unchecked map[string]int64
	// Broken is the seq of the first row that does not check out, 0 if
	// none; Problem says why. Rows after it are not looked at.
	Broken  int64
	Problem string
}

// Intact reports whether every row that could be checked checked out.
func (r Report) Intact() bool { return r.Broken == 0 }

var errStop = errors.New("stop")

// Verify checks the trail in st against the keys in ring (nil for none).
func Verify(ctx context.Context, st *store.Store, ring *secrets.Keyring) (Report, error) {
	rep := Report{Unchecked: map[string]int64{}}
	var prev []byte
	signed := false
	err := st.EachAudit(ctx, func(e store.AuditEntry) error {
		rep.Entries++
		rep.LastSeq, rep.LastMAC = e.Seq, hex.EncodeToString(e.MAC)
		broken := func(problem string) error {
			rep.Broken, rep.Problem = e.Seq, problem
			return errStop
		}
		if e.Seq != rep.Entries {
			return broken(fmt.Sprintf("entry %d was expected here: entries before it are missing", rep.Entries))
		}
		switch e.KeyID {
		case "":
			// Unsigned rows only ever come first, from before there was
			// a master key. One after a signed row is a trail rewritten
			// from there by somebody without the key.
			if signed {
				return broken("not signed, although entries before it are: the trail was rewritten from here")
			}
			rep.Unsigned++
			sum := sha256.Sum256(encode(prev, e))
			if !hmac.Equal(sum[:], e.MAC) {
				return broken("does not match: it was changed, or an entry before it was")
			}
		default:
			signed = true
			id, err := secrets.ParseKeyID(e.KeyID)
			if err != nil {
				return broken("names an unreadable key ID")
			}
			var key []byte
			ok := false
			if ring != nil {
				key, ok = ring.Derive(id, label)
			}
			if !ok {
				// It cannot be checked, but the chain goes on: the next
				// row's signature still covers this one's.
				rep.Unchecked[e.KeyID]++
				break
			}
			if !hmac.Equal(sign(key, prev, e), e.MAC) {
				return broken("does not match its signature: it was changed, or an entry before it was")
			}
		}
		prev = e.MAC
		return nil
	})
	if errors.Is(err, errStop) {
		err = nil
	}
	return rep, err
}
