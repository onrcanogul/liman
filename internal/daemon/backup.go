package daemon

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onrcanogul/liman/internal/backup"
	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/store"
)

// What is backed up is the database: history, releases, token hashes,
// sealed secrets and health samples. What is not:
//
//   - the master key. A backup that carries its own key protects nothing
//     that the encryption was there to protect; the key is copied once, by
//     hand, to somewhere else.
//   - containers and images. They are rebuilt by deploying again.
//   - the Nginx config. It is generated on every deploy.

const backupSynopsis = "limand backup create <dir> [--keep <n>] | verify <file> | restore <file>"

// backupNameRe is the shape of the file names "backup create" writes.
var backupNameRe = backup.NameRe

func (d *Daemon) runBackup(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return d.usageError(backupSynopsis)
	}
	switch args[0] {
	case "create":
		return d.backupCreate(ctx, args[1:])
	case "verify":
		return d.backupVerify(ctx, args[1:])
	case "restore":
		return d.backupRestore(ctx, args[1:])
	default:
		return d.usageError(backupSynopsis)
	}
}

// backupReport is what checking a backup file found.
type backupReport = backup.Report

// checkBackup checks a backup file against this daemon's master key.
func (d *Daemon) checkBackup(ctx context.Context, path string, cfg config.Config) (backupReport, error) {
	return backup.Check(ctx, path, d.keyPath(cfg))
}

// printReport writes what a check found, one fact per line.
func (d *Daemon) printReport(rep backupReport, cfg config.Config) {
	switch rep.Signature {
	case backup.SigValid:
		fmt.Fprintln(d.Stdout, "  signature valid: the file is what this master key signed")
	case backup.SigMissing:
		fmt.Fprintln(d.Stdout, "  NOT signed: there is no signature file, so changes to it could not be noticed")
	case backup.SigUnchecked:
		fmt.Fprintf(d.Stdout, "  signature NOT checked: there is no master key at %s\n", d.keyPath(cfg))
	case backup.SigInvalid:
		fmt.Fprintln(d.Stdout, "  signature WRONG: the file was changed after it was written, or it belongs to another master key")
		// Such a file is not opened; there is nothing more to report.
		return
	}

	sum := rep.Summary
	fmt.Fprintf(d.Stdout, "  schema version %d (this liman: %d)\n", sum.SchemaVersion, store.SchemaVersion())

	// A fixed order; ranging over the map would shuffle the lines.
	var parts []string
	for _, table := range []string{"events", "releases", "tokens", "secrets", "health_samples"} {
		if n, ok := sum.Rows[table]; ok {
			parts = append(parts, fmt.Sprintf("%d %s", n, strings.ReplaceAll(table, "_", " ")))
		}
	}
	fmt.Fprintf(d.Stdout, "  %s\n", strings.Join(parts, ", "))

	switch {
	case rep.Secrets == 0:
		// Nothing is encrypted, so there is nothing to test.
	case !rep.KeyChecked:
		fmt.Fprintf(d.Stdout, "  secrets NOT checked: there is no master key at %s\n", d.keyPath(cfg))
	case len(rep.Undecryptable) > 0:
		fmt.Fprintf(d.Stdout, "  %d of %d secrets can NOT be decrypted with %s: %s\n",
			len(rep.Undecryptable), rep.Secrets, d.keyPath(cfg), strings.Join(rep.Undecryptable, ", "))
	default:
		fmt.Fprintf(d.Stdout, "  all %d secrets can be decrypted with %s\n", rep.Secrets, d.keyPath(cfg))
	}
}

func (d *Daemon) backupCreate(ctx context.Context, args []string) int {
	const synopsis = "limand backup create <dir> [--keep <n>]"

	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		return d.usageError(synopsis)
	}
	dir := args[0]

	fs := flag.NewFlagSet("backup create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	keep := fs.Int("keep", 0, "remove all but the newest n backups in the directory")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *keep < 0 {
		return d.usageError(synopsis)
	}

	cfg, err := config.Load(filepath.Join(d.Home, "config.yaml"))
	if err != nil {
		return d.fail(err)
	}
	// Refuse before a database is created out of thin air: opening the
	// store creates the file, and a backup of an empty database that was
	// born a moment ago would look like a success.
	if _, err := os.Stat(filepath.Join(d.Home, "liman.db")); err != nil {
		return d.fail(fmt.Errorf("there is no database in %s to back up", d.Home))
	}

	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(err)
	}
	dest, rep, err := backup.Create(ctx, st, dir, d.keyPath(cfg))
	_ = st.Close()
	if err != nil {
		return d.fail(err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		return d.fail(err)
	}
	fmt.Fprintf(d.Stdout, "Wrote %s (%s)\n", dest, formatSize(info.Size()))
	d.printReport(rep, cfg)
	if len(rep.Undecryptable) > 0 {
		return d.fail(errors.New("the backup holds secrets that the current master key cannot decrypt; the live database has the same problem"))
	}

	// Old backups go only after the new one is known to be good: a failed
	// run never leaves fewer usable backups than it found.
	if *keep > 0 {
		removed, err := backup.Prune(dir, *keep)
		if err != nil {
			return d.fail(err)
		}
		if removed > 0 {
			fmt.Fprintf(d.Stdout, "Removed %d older backups, kept the newest %d\n", removed, *keep)
		}
	}

	if rep.Secrets > 0 {
		fmt.Fprintf(d.Stdout, "\nThe master key is not part of the backup. Without a copy of %s the secrets in it are lost; keep that copy somewhere else, not next to the backups.\n", d.keyPath(cfg))
	}
	return exitOK
}

func (d *Daemon) backupVerify(ctx context.Context, args []string) int {
	if len(args) != 1 {
		return d.usageError("limand backup verify <file>")
	}

	cfg, err := config.Load(filepath.Join(d.Home, "config.yaml"))
	if err != nil {
		return d.fail(err)
	}

	rep, err := d.checkBackup(ctx, args[0], cfg)
	if err != nil {
		return d.fail(err)
	}
	if rep.Signature == backup.SigInvalid {
		fmt.Fprintf(d.Stdout, "%s\n", args[0])
		d.printReport(rep, cfg)
		return d.fail(errors.New("do not restore this backup: its signature does not match"))
	}
	fmt.Fprintf(d.Stdout, "%s is intact\n", args[0])
	d.printReport(rep, cfg)
	if len(rep.Undecryptable) > 0 {
		return d.fail(errors.New("this backup belongs to a different master key"))
	}
	return exitOK
}

func (d *Daemon) backupRestore(ctx context.Context, args []string) int {
	const synopsis = "limand backup restore <file> [--allow-unsigned]"
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		return d.usageError(synopsis)
	}
	source := args[0]

	flags := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	allowUnsigned := flags.Bool("allow-unsigned", false, "restore a backup whose signature cannot be checked")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return d.usageError(synopsis)
	}

	cfg, err := config.Load(filepath.Join(d.Home, "config.yaml"))
	if err != nil {
		return d.fail(err)
	}

	target := filepath.Join(d.Home, "liman.db")
	staged := target + ".restoring"
	stamp := time.Now().UTC().Format("20060102-150405")

	// 1. Copy the backup next to the database, then check the copy. The
	// file that is checked is the file that is used: checking the original
	// and copying it afterwards would leave a moment in which it could be
	// swapped for another. The copy is private and nobody else can write
	// to it.
	_ = os.Remove(staged) // left behind by a run that was interrupted
	if err := copyFileExclusive(source, staged); err != nil {
		return d.fail(fmt.Errorf("copy backup: %w", err))
	}
	// Until the copy has become the database, every way out removes it.
	placed := false
	defer func() {
		if !placed {
			_ = os.Remove(staged)
		}
	}()

	// The private copy is checked against the signature that came with
	// the original.
	rep, err := backup.CheckWithSignature(ctx, staged, source+backup.SigSuffix, d.keyPath(cfg))
	if err != nil {
		return d.fail(fmt.Errorf("%s: %w", source, err))
	}
	switch rep.Signature {
	case backup.SigInvalid:
		return d.fail(fmt.Errorf("%s does not match its signature: it was changed after it was written, or it belongs to another master key. Nothing was restored", source))
	case backup.SigMissing, backup.SigUnchecked:
		// The tokens in a backup become valid tokens. Without a checked
		// signature nothing says the file is the one that was written, so
		// that has to be a decision, not a default.
		if !*allowUnsigned {
			return d.fail(fmt.Errorf("the signature of %s could not be checked (%s). If you are sure where this file has been, restore it with --allow-unsigned and look at the list of tokens afterwards", source, rep.Signature))
		}
	}
	if rep.Secrets > 0 && !rep.KeyChecked {
		return d.fail(fmt.Errorf("the backup holds %d secrets, but there is no master key at %s; put the key that belongs to this backup there first", rep.Secrets, d.keyPath(cfg)))
	}
	if len(rep.Undecryptable) > 0 {
		return d.fail(fmt.Errorf("%d of the %d secrets in the backup cannot be decrypted with %s; put the key that belongs to this backup there first", len(rep.Undecryptable), rep.Secrets, d.keyPath(cfg)))
	}

	// 2. Nobody may be using the database. A running daemon holds it open
	// and would keep writing to the file that is about to be moved away.
	if daemonAnswers(ctx, cfg.Daemon.Listen) {
		return d.fail(fmt.Errorf("the daemon is running on %s; stop it first (systemctl stop limand)", cfg.Daemon.Listen))
	}

	// 3. The current database is moved aside, never deleted: if the restore
	// turns out to have been a mistake, it can be undone by hand.
	aside := ""
	if _, err := os.Stat(target); err == nil {
		aside = target + ".replaced-" + stamp
		if err := d.setAside(ctx, target, aside); err != nil {
			return d.fail(err)
		}
	}

	// 4. Rename the checked copy into place: the database is either the old
	// one or the complete new one, never half a file.
	if err := os.Rename(staged, target); err != nil {
		return d.fail(fmt.Errorf("place database: %w", err))
	}
	placed = true

	// 5. Open it the normal way. That brings an older schema up to date and
	// proves the daemon will be able to start.
	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(fmt.Errorf("the restored database does not open: %w", err))
	}
	defer func() { _ = st.Close() }()

	fmt.Fprintf(d.Stdout, "Restored %s\n", source)
	d.printReport(rep, cfg)
	if aside != "" {
		fmt.Fprintf(d.Stdout, "The previous database was kept as %s\n", aside)
	}

	// A restore turns the clock back for the tokens too: one that was
	// revoked after the backup was made is valid again.
	if tokens, err := st.Tokens(ctx); err == nil && len(tokens) > 0 {
		names := make([]string, 0, len(tokens))
		for _, t := range tokens {
			names = append(names, t.Name)
		}
		fmt.Fprintf(d.Stdout, "\nThese tokens are valid now: %s\nIf one of them was revoked after this backup was made, revoke it again: limand token revoke <name>\n", strings.Join(names, ", "))
	}
	fmt.Fprintln(d.Stdout, "\nStart the daemon again: systemctl start limand")
	return exitOK
}

// setAside renames the database at path to aside, as one complete file.
func (d *Daemon) setAside(ctx context.Context, path, aside string) error {
	// While the daemon ran, recent writes went to liman.db-wal. Folding
	// them into the main file first makes the file that is kept complete on
	// its own. If the database is too damaged to open (a likely reason for
	// restoring), it is kept as it is.
	if st, err := store.Open(ctx, path); err == nil {
		_ = st.Checkpoint(ctx)
		_ = st.Close()
	}

	if err := os.Rename(path, aside); err != nil {
		return fmt.Errorf("set the current database aside: %w", err)
	}
	// The companions belong to the old file. Left behind, SQLite would
	// apply them to the restored one.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Rename(path+suffix, aside+suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("set the current database aside: %w", err)
		}
	}
	return nil
}

// copyFileExclusive copies src to dst, which must not exist, with
// permissions for the owner only, and makes sure it is on the disk.
func copyFileExclusive(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if err := errors.Join(copyErr, out.Sync(), out.Close()); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// daemonAnswers reports whether a liman daemon is serving on address.
func daemonAnswers(ctx context.Context, address string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/v1/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Nothing listening, or not reachable: not running, as far as can
		// be told from here.
		return false
	}
	_ = resp.Body.Close()
	return true
}

// formatSize writes a byte count the way people say it.
func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
