package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/onrcanogul/liman/internal/audit"
	"github.com/onrcanogul/liman/internal/config"
)

const auditSynopsis = "limand audit verify"

func (d *Daemon) runAudit(ctx context.Context, args []string) int {
	if len(args) != 1 || args[0] != "verify" {
		return d.usageError(auditSynopsis)
	}
	return d.auditVerify(ctx)
}

// auditVerify checks the audit trail against the master keys. It works
// with the daemon running or stopped: it only reads.
func (d *Daemon) auditVerify(ctx context.Context) int {
	cfg, err := config.Load(filepath.Join(d.Home, "config.yaml"))
	if err != nil {
		return d.fail(err)
	}
	keys, err := audit.KeysAt(d.keyPath(cfg))()
	if err != nil {
		return d.fail(err)
	}
	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(err)
	}
	defer func() { _ = st.Close() }()

	rep, err := audit.Verify(ctx, st, keys)
	if err != nil {
		return d.fail(err)
	}
	if rep.Entries == 0 {
		fmt.Fprintln(d.Stdout, "The audit trail is empty.")
		return exitOK
	}
	if !rep.Intact() {
		fmt.Fprintf(d.Stdout, "The audit trail is BROKEN at record %d: %s.\n", rep.Broken, rep.Problem)
		fmt.Fprintln(d.Stdout, "Records from there on cannot be trusted. Compare with the system log: journalctl -t limand | grep 'msg=audit'")
		return exitError
	}
	fmt.Fprintf(d.Stdout, "The audit trail is intact: %s, every one that could be checked checks out.\n", count(int(rep.Entries), "record"))
	if rep.Unsigned > 0 {
		fmt.Fprintf(d.Stdout, "%s at the start were written before there was a master key; they are hashed, not signed.\n", count(int(rep.Unsigned), "record"))
	}
	ids := make([]string, 0, len(rep.Unchecked))
	for id := range rep.Unchecked {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(d.Stdout, "%s signed with key %s could not be checked: this server no longer has that key.\n", count(int(rep.Unchecked[id]), "record"), id)
	}
	// What the chain cannot tell is records cut off at the end. The log
	// has every record's signature; the last one there must be this one.
	fmt.Fprintf(d.Stdout, "\nLast record: %d, signature %s\n", rep.LastSeq, rep.LastMAC)
	fmt.Fprintln(d.Stdout, "The chain cannot show records removed from the end. Check that the system log ends with the same record:")
	fmt.Fprintln(d.Stdout, "  journalctl -t limand | grep 'msg=audit' | tail -1")
	return exitOK
}

// fanout is a slog handler that passes every record to each of its
// handlers that wants it.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}
