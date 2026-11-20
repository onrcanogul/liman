package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// chainSigner is a stand-in for package audit's signer: the mac is the
// hash of the previous mac and the row's sequence number and action.
func chainSigner(prev []byte, e AuditEntry) (string, []byte, error) {
	sum := sha256.Sum256(fmt.Appendf(append([]byte{}, prev...), "%d|%s", e.Seq, e.Action))
	return "test", sum[:], nil
}

// checkChain reads the trail back and checks that every row signs its
// predecessor and that the numbers have no gaps.
func checkChain(t *testing.T, s *Store, want int) {
	t.Helper()
	var prev []byte
	n := 0
	err := s.EachAudit(context.Background(), func(e AuditEntry) error {
		n++
		if e.Seq != int64(n) {
			return fmt.Errorf("row %d has seq %d", n, e.Seq)
		}
		_, mac, _ := chainSigner(prev, e)
		if !bytes.Equal(mac, e.MAC) {
			return fmt.Errorf("seq %d does not follow seq %d", e.Seq, e.Seq-1)
		}
		prev = e.MAC
		return nil
	})
	if err != nil || n != want {
		t.Fatalf("the trail: %d rows, %v; want %d rows in one chain", n, err, want)
	}
}

func TestAppendAudit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	var prevs [][]byte
	sign := func(prev []byte, e AuditEntry) (string, []byte, error) {
		prevs = append(prevs, prev)
		return chainSigner(prev, e)
	}
	at := time.Date(2026, 11, 20, 12, 0, 0, 123456789, time.FixedZone("TRT", 3*3600))
	first, err := s.AppendAudit(ctx, AuditEntry{At: at, Actor: "token:cli", Action: "deploy", App: "web", Target: "r1", OK: true}, sign)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AppendAudit(ctx, AuditEntry{At: at, Actor: "os:root", Action: "token.revoke", Target: "ci", Error: "boom"}, sign)
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || second.Seq != 2 || prevs[0] != nil || !bytes.Equal(prevs[1], first.MAC) {
		t.Errorf("seqs %d, %d; prevs %x; want 1, 2, signed after nothing and after the first", first.Seq, second.Seq, prevs)
	}
	// The time is signed as it is stored: in UTC, exactly.
	if first.At.Location() != time.UTC || !first.At.Equal(at) {
		t.Errorf("At = %v, want %v in UTC", first.At, at)
	}

	all, err := s.AuditEntries(ctx, nil, 10)
	if err != nil || len(all) != 2 || all[0].Actor != "token:cli" || !all[0].OK || all[1].Error != "boom" || all[1].KeyID != "test" || !bytes.Equal(all[1].MAC, second.MAC) || !all[0].At.Equal(at) {
		t.Fatalf("AuditEntries(nil) = %+v, %v", all, err)
	}
	// A set of apps leaves out the rows about the daemon as a whole.
	if web, err := s.AuditEntries(ctx, []string{"web"}, 10); err != nil || len(web) != 1 || web[0].App != "web" {
		t.Errorf("AuditEntries(web) = %+v, %v", web, err)
	}
	if last, _ := s.AuditEntries(ctx, nil, 1); len(last) != 1 || last[0].Seq != 2 {
		t.Errorf("AuditEntries(limit 1) = %+v; want the newest", last)
	}

	// A signer that fails adds nothing.
	_, err = s.AppendAudit(ctx, AuditEntry{At: at, Actor: "x", Action: "y"}, func([]byte, AuditEntry) (string, []byte, error) {
		return "", nil, fmt.Errorf("no key")
	})
	if err == nil {
		t.Error("AppendAudit with a failing signer succeeded")
	}
	checkChain(t, s, 2)
}

// Appends from several goroutines, and from a second Store on the same
// file standing in for a limand command running next to the daemon, end
// up in one unbroken chain.
func TestAppendAuditFromTwoWritersAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liman.db")
	ctx := context.Background()
	a, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	const each = 40
	var wg sync.WaitGroup
	errs := make(chan error, 4*each)
	for w, st := range []*Store{a, a, b, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				e := AuditEntry{At: time.Now(), Actor: fmt.Sprintf("writer-%d", w), Action: fmt.Sprintf("act-%d", i), OK: true}
				if _, err := st.AppendAudit(ctx, e, chainSigner); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AppendAudit: %v", err)
	}
	checkChain(t, a, 4*each)
}
