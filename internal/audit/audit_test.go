package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/onrcanogul/liman/internal/secrets"
	"github.com/onrcanogul/liman/internal/store"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func ring(t *testing.T, primary []byte, others ...[]byte) *secrets.Keyring {
	t.Helper()
	r, err := secrets.NewKeyring(primary, others...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// trail opens a store and returns it with its path, for tampering with
// the file the way somebody with access to it would.
func trail(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "liman.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

func tamper(t *testing.T, path, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func record(t *testing.T, r *Recorder, n int) {
	t.Helper()
	ctx := context.Background()
	acts := []struct{ actor, action, app, target string }{
		{TokenActor("cli", time.Date(2026, 11, 20, 5, 35, 0, 0, time.UTC)), Deploy, "web", "20261120-1"},
		{TokenActor("ci", time.Date(2026, 11, 20, 17, 0, 0, 0, time.UTC)), SecretSet, "web", "API_KEY"},
		{"server:root", TokenRevoke, "", "old-laptop"},
		{Monitor, Restart, "web", "web-20261120-1"},
	}
	for i := range n {
		a := acts[i%len(acts)]
		var err error
		if i%3 == 2 {
			err = errors.New("it failed")
		}
		r.Record(ctx, a.actor, a.action, a.app, a.target, err)
	}
}

func verify(t *testing.T, st *store.Store, keys *secrets.Keyring) Report {
	t.Helper()
	rep, err := Verify(context.Background(), st, keys)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestASignedTrailVerifies(t *testing.T) {
	st, _ := trail(t)
	keys := ring(t, newKey(t))
	var log bytes.Buffer
	r := &Recorder{Store: st, Keys: func() (*secrets.Keyring, error) { return keys, nil }, Log: slog.New(slog.NewTextHandler(&log, nil))}
	record(t, r, 6)

	rep := verify(t, st, keys)
	if !rep.Intact() || rep.Entries != 6 || rep.LastSeq != 6 || rep.Unsigned != 0 || len(rep.Unchecked) != 0 {
		t.Fatalf("Verify() = %+v; want 6 intact, signed entries", rep)
	}
	rows, _ := st.AuditEntries(context.Background(), nil, 10)
	if rows[0].KeyID != keys.Primary().String() || rows[2].OK || rows[2].Error != "it failed" || rows[2].App != "" {
		t.Errorf("stored rows = %+v", rows)
	}
	// Every row also reaches the log with its signature: the anchor that
	// shows rows cut off at the end.
	if !strings.Contains(log.String(), "msg=audit seq=6 ") || !strings.Contains(log.String(), "mac="+rep.LastMAC) {
		t.Errorf("the log does not carry the last row's anchor:\n%s", log.String())
	}
}

func TestChangesToTheTrailShow(t *testing.T) {
	keys := ring(t, newKey(t))
	tests := []struct {
		name   string
		change func(t *testing.T, path string)
		broken int64
	}{
		{"an actor changed", func(t *testing.T, p string) {
			tamper(t, p, `UPDATE audit SET actor = 'token:cli@2026-11-20T05:35:00Z' WHERE seq = 2`)
		}, 2},
		{"an outcome flipped", func(t *testing.T, p string) { tamper(t, p, `UPDATE audit SET ok = 1, error = '' WHERE seq = 3`) }, 3},
		{"a row removed", func(t *testing.T, p string) { tamper(t, p, `DELETE FROM audit WHERE seq = 2`) }, 3},
		{"a row removed and the rest renumbered", func(t *testing.T, p string) {
			tamper(t, p, `DELETE FROM audit WHERE seq = 2`)
			tamper(t, p, `UPDATE audit SET seq = seq - 1 WHERE seq > 2`)
		}, 2},
		{"two rows swapped", func(t *testing.T, p string) {
			tamper(t, p, `UPDATE audit SET seq = 100 WHERE seq = 2`)
			tamper(t, p, `UPDATE audit SET seq = 2 WHERE seq = 3`)
			tamper(t, p, `UPDATE audit SET seq = 3 WHERE seq = 100`)
		}, 2},
		{"the time moved", func(t *testing.T, p string) {
			tamper(t, p, `UPDATE audit SET at = '2026-01-01T00:00:00.000000000Z' WHERE seq = 4`)
		}, 4},
		{"the key ID swapped for nonsense", func(t *testing.T, p string) { tamper(t, p, `UPDATE audit SET key_id = 'zz' WHERE seq = 1`) }, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, path := trail(t)
			record(t, &Recorder{Store: st, Keys: func() (*secrets.Keyring, error) { return keys, nil }}, 5)
			tt.change(t, path)
			rep := verify(t, st, keys)
			if rep.Intact() || rep.Broken != tt.broken {
				t.Errorf("Verify() = broken at %d (%s); want broken at %d", rep.Broken, rep.Problem, tt.broken)
			}
		})
	}
}

// Somebody without the key rewrites the rest of the trail unsigned, with
// correct hashes. That is caught: an unsigned row after a signed one.
func TestARewriteWithoutTheKeyShows(t *testing.T) {
	st, path := trail(t)
	keys := ring(t, newKey(t))
	record(t, &Recorder{Store: st, Keys: func() (*secrets.Keyring, error) { return keys, nil }}, 4)

	rows, _ := st.AuditEntries(context.Background(), nil, 10)
	prev := rows[1].MAC
	for _, e := range rows[2:] {
		e.Actor = "nobody"
		e.KeyID = ""
		sum := sha256.Sum256(encode(prev, e))
		tamper(t, path, `UPDATE audit SET actor = ?, key_id = '', mac = ? WHERE seq = ?`, e.Actor, hex.EncodeToString(sum[:]), e.Seq)
		prev = sum[:]
	}
	if rep := verify(t, st, keys); rep.Intact() || rep.Broken != 3 || !strings.Contains(rep.Problem, "not signed") {
		t.Errorf("Verify() = %+v; want broken at 3, not signed", rep)
	}
}

// What the chain cannot tell: rows cut off at the end. The rest is
// consistent; the log's anchor is what shows the difference.
func TestCuttingTheEndIsNotSeenByTheChain(t *testing.T) {
	st, path := trail(t)
	keys := ring(t, newKey(t))
	record(t, &Recorder{Store: st, Keys: func() (*secrets.Keyring, error) { return keys, nil }}, 5)
	tamper(t, path, `DELETE FROM audit WHERE seq > 3`)
	if rep := verify(t, st, keys); !rep.Intact() || rep.LastSeq != 3 {
		t.Errorf("Verify() = %+v; want intact, ending at 3", rep)
	}
}

func TestKeysChangingOverTime(t *testing.T) {
	st, _ := trail(t)
	old, current := newKey(t), newKey(t)
	keys := ring(t, old)
	r := &Recorder{Store: st, Keys: func() (*secrets.Keyring, error) { return keys, nil }}
	record(t, r, 2)
	// A rotation: the new key signs from now on, the old one stays.
	keys = ring(t, current, old)
	record(t, r, 2)

	if rep := verify(t, st, keys); !rep.Intact() || rep.Entries != 4 || len(rep.Unchecked) != 0 {
		t.Errorf("Verify() after a rotation = %+v; want 4 intact", rep)
	}
	// The old key retired: its rows cannot be checked, but they are not
	// called broken, and the rows after them still are checked.
	retired := ring(t, current)
	rep := verify(t, st, retired)
	oldID := ring(t, old).Primary().String()
	if !rep.Intact() || rep.Unchecked[oldID] != 2 {
		t.Errorf("Verify() without the old key = %+v; want intact, 2 unchecked with %s", rep, oldID)
	}
	// Without any key nothing signed can be checked.
	if rep := verify(t, st, nil); !rep.Intact() || len(rep.Unchecked) != 2 {
		t.Errorf("Verify() without keys = %+v", rep)
	}
}

func TestWithoutAMasterKeyTheTrailIsHashed(t *testing.T) {
	st, path := trail(t)
	record(t, &Recorder{Store: st, Keys: func() (*secrets.Keyring, error) { return nil, nil }}, 3)
	rep := verify(t, st, nil)
	if !rep.Intact() || rep.Unsigned != 3 {
		t.Fatalf("Verify() = %+v; want 3 intact, unsigned", rep)
	}
	// Accidental damage still shows.
	tamper(t, path, `UPDATE audit SET target = 'x' WHERE seq = 2`)
	if rep := verify(t, st, nil); rep.Broken != 2 {
		t.Errorf("Verify() after damage = %+v; want broken at 2", rep)
	}
	// Put back as it was, it checks out again.
	tamper(t, path, `UPDATE audit SET target = 'API_KEY' WHERE seq = 2`)
	if rep := verify(t, st, nil); !rep.Intact() {
		t.Fatalf("Verify() after undoing the damage = %+v", rep)
	}
	// Once there is a master key, the trail goes on signed after the
	// unsigned start.
	keys := ring(t, newKey(t))
	record(t, &Recorder{Store: st, Keys: func() (*secrets.Keyring, error) { return keys, nil }}, 2)
	if rep := verify(t, st, keys); !rep.Intact() || rep.Unsigned != 3 || rep.Entries != 5 {
		t.Errorf("Verify() with signed entries after unsigned ones = %+v; want 5 intact, 3 unsigned", rep)
	}
}

// A key that cannot be read does not lead to an unsigned row.
func TestAnUnreadableKeyWritesNothing(t *testing.T) {
	st, _ := trail(t)
	var log bytes.Buffer
	r := &Recorder{Store: st, Keys: func() (*secrets.Keyring, error) { return nil, errors.New("permission denied") }, Log: slog.New(slog.NewTextHandler(&log, nil))}
	r.Record(context.Background(), "token:cli@x", Deploy, "web", "r1", nil)
	if rows, _ := st.AuditEntries(context.Background(), nil, 10); len(rows) != 0 {
		t.Errorf("rows = %+v; want none", rows)
	}
	if !strings.Contains(log.String(), "level=ERROR") || !strings.Contains(log.String(), "audit entry not written") {
		t.Errorf("the failure was not logged as an error:\n%s", log.String())
	}
	// A nil Recorder is a no-op.
	var none *Recorder
	none.Record(context.Background(), "a", "b", "", "", nil)
}

// The encoding cannot be made ambiguous by a field's content.
func TestEncodingKeepsFieldsApart(t *testing.T) {
	a := store.AuditEntry{Seq: 1, Actor: "a,b", Action: "c"}
	b := store.AuditEntry{Seq: 1, Actor: "a", Action: "b,c"}
	if bytes.Equal(encode(nil, a), encode(nil, b)) {
		t.Error("two different rows encode to the same bytes")
	}
}
