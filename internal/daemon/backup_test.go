package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/secrets"
	"github.com/onrcanogul/liman/internal/store"
)

// secretValue is recognisable, so a test can search output and files for it.
const secretValue = "s3cr3t-value-that-must-never-be-printed"

// seed gives a test daemon a database with a token, an event and one
// secret encrypted with a fresh master key.
func seed(t *testing.T, td *testDaemon) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(td.Home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if _, err := st.CreateToken(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, store.Event{Time: time.Now(), App: "web", Release: "r1", Step: "Pull image", Action: "run", OK: true}); err != nil {
		t.Fatal(err)
	}

	key, err := secrets.CreateKey(filepath.Join(td.Home, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	vault, err := secrets.NewVault(key, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Set(ctx, "web", "DATABASE_URL", []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
}

// backupsIn returns the backup files in dir, oldest first.
func backupsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if backupNameRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names
}

// replaceKey swaps the master key for a different one, as restoring on a
// machine with the wrong key file would.
func replaceKey(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(home, "master.key")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.CreateKey(path); err != nil {
		t.Fatal(err)
	}
}

func TestBackupCreateWritesACheckedPrivateCopy(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	dir := filepath.Join(t.TempDir(), "backups")

	if code := td.Run(context.Background(), []string{"backup", "create", dir}); code != exitOK {
		t.Fatalf("backup create: code %d, stderr %q", code, td.stderr.String())
	}

	names := backupsIn(t, dir)
	if len(names) != 1 {
		t.Fatalf("backups written = %v, want one", names)
	}
	file := filepath.Join(dir, names[0])

	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("backup file: %v, mode %v; want mode 600", err, info.Mode().Perm())
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("backup directory mode = %v, want 700", info.Mode().Perm())
	}

	out := td.stdout.String()
	for _, want := range []string{names[0], "signature valid", "1 event,", "1 token,", "1 secret,", "the one secret can be decrypted", "master key is not part of the backup"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}

	// The backup holds the secret sealed, like the database does; and the
	// key is nowhere near it.
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secretValue) || strings.Contains(out+td.stderr.String(), secretValue) {
		t.Error("the secret's value appears in the backup or in the output")
	}
	// Next to it: its signature, and nothing else (no key, no leftovers).
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("the backup directory holds %d entries, want the backup and its signature", len(entries))
	}
	sig, err := os.Stat(file + ".sig")
	if err != nil || sig.Mode().Perm() != 0o600 {
		t.Errorf("signature file: %v, want it to exist with mode 600", err)
	}
}

func TestBackupCreateDoesNotInventADatabase(t *testing.T) {
	td := newTestDaemon(t)
	dir := t.TempDir()

	if code := td.Run(context.Background(), []string{"backup", "create", dir}); code != exitError {
		t.Errorf("backup create without a database: code %d, want %d", code, exitError)
	}
	if _, err := os.Stat(filepath.Join(td.Home, "liman.db")); err == nil {
		t.Error("a database was created by trying to back one up")
	}
	if names := backupsIn(t, dir); len(names) != 0 {
		t.Errorf("backups written = %v, want none", names)
	}
}

func TestBackupKeepRemovesOnlyOlderBackups(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	dir := t.TempDir()

	// Three earlier backups and two files that only look similar.
	old := []string{"liman-20200101-000000.db", "liman-20200102-000000.db", "liman-20200103-000000.db"}
	others := []string{"notes.txt", "liman-important.db"}
	for _, name := range append(old, others...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if code := td.Run(context.Background(), []string{"backup", "create", dir, "--keep", "2"}); code != exitOK {
		t.Fatalf("backup create --keep: code %d, stderr %q", code, td.stderr.String())
	}

	names := backupsIn(t, dir)
	if len(names) != 2 || names[0] != "liman-20200103-000000.db" {
		t.Errorf("backups after --keep 2 = %v, want the newest old one and the new one", names)
	}
	for _, name := range others {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed; only files named like backups may be", name)
		}
	}
}

func TestBackupVerify(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	dir := t.TempDir()
	ctx := context.Background()

	if code := td.Run(ctx, []string{"backup", "create", dir}); code != exitOK {
		t.Fatalf("backup create: code %d, stderr %q", code, td.stderr.String())
	}
	file := filepath.Join(dir, backupsIn(t, dir)[0])

	if code := td.Run(ctx, []string{"backup", "verify", file}); code != exitOK || !strings.Contains(td.stdout.String(), "is intact") {
		t.Errorf("verify of a good backup: code %d, output %q", code, td.stdout.String())
	}

	// With another key the file is still intact, but useless for secrets.
	replaceKey(t, td.Home)
	if code := td.Run(ctx, []string{"backup", "verify", file}); code != exitError {
		t.Errorf("verify with the wrong key: code %d, want %d", code, exitError)
	}
	// The signature is checked first, and with another key it is wrong;
	// the file is then not even opened.
	if out := td.stdout.String(); !strings.Contains(out, "signature WRONG") {
		t.Errorf("verify with the wrong key does not report the signature:\n%s", out)
	}

	// Without any key it says so instead of claiming success for them.
	if err := os.Remove(filepath.Join(td.Home, "master.key")); err != nil {
		t.Fatal(err)
	}
	if code := td.Run(ctx, []string{"backup", "verify", file}); code != exitOK || !strings.Contains(td.stdout.String(), "secrets NOT checked") || !strings.Contains(td.stdout.String(), "signature NOT checked") {
		t.Errorf("verify without a key: code %d, output %q", code, td.stdout.String())
	}

	// Something that is not a backup at all.
	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := td.Run(ctx, []string{"backup", "verify", junk}); code != exitError {
		t.Errorf("verify of a text file: code %d, want %d", code, exitError)
	}
}

func TestBackupRestoreBringsBackTheStateOfTheBackup(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	dir := t.TempDir()
	ctx := context.Background()

	if code := td.Run(ctx, []string{"backup", "create", dir}); code != exitOK {
		t.Fatalf("backup create: code %d, stderr %q", code, td.stderr.String())
	}
	file := filepath.Join(dir, backupsIn(t, dir)[0])

	// After the backup: a token is revoked and a new one is created.
	if code := td.Run(ctx, []string{"token", "revoke", "laptop"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	if code := td.Run(ctx, []string{"token", "create", "later"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}

	if code := td.Run(ctx, []string{"backup", "restore", file}); code != exitOK {
		t.Fatalf("backup restore: code %d, stderr %q", code, td.stderr.String())
	}
	out := td.stdout.String()
	// The warning that a revoked token is back.
	if !strings.Contains(out, "These tokens are valid now: laptop") {
		t.Errorf("restore does not warn about the tokens that are valid again:\n%s", out)
	}

	st, err := store.Open(ctx, filepath.Join(td.Home, "liman.db"))
	if err != nil {
		t.Fatalf("the restored database does not open: %v", err)
	}
	tokens, err := st.Tokens(ctx)
	if err != nil || len(tokens) != 1 || tokens[0].Name != "laptop" {
		t.Errorf("tokens after restore = %+v, %v; want only laptop", tokens, err)
	}
	if n, err := st.CountSecrets(ctx); err != nil || n != 1 {
		t.Errorf("secrets after restore = %d, %v; want 1", n, err)
	}
	_ = st.Close()

	// The database that was replaced is kept, complete, under another name.
	kept, _ := filepath.Glob(filepath.Join(td.Home, "liman.db.replaced-*"))
	var asides []string
	for _, k := range kept {
		if !strings.HasSuffix(k, "-wal") && !strings.HasSuffix(k, "-shm") {
			asides = append(asides, k)
		}
	}
	if len(asides) != 1 {
		t.Fatalf("databases set aside = %v, want one", asides)
	}
	old, err := store.OpenReadOnly(ctx, asides[0])
	if err != nil {
		t.Fatalf("the database that was set aside does not open: %v", err)
	}
	defer func() { _ = old.Close() }()
	if tokens, err := old.Tokens(ctx); err != nil || len(tokens) != 1 || tokens[0].Name != "later" {
		t.Errorf("tokens in the database set aside = %+v, %v; want only later", tokens, err)
	}
}

func TestBackupRestoreIsRefusedWhileTheDaemonRuns(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	dir := t.TempDir()
	ctx := context.Background()

	if code := td.Run(ctx, []string{"backup", "create", dir}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	file := filepath.Join(dir, backupsIn(t, dir)[0])

	addr, stop, exit := startServing(t, td)
	// The test daemon listens on port 0; tell the restore where it ended up.
	writeConfig(t, td.Home, "daemon:\n  listen: "+addr+"\n")

	if code := td.Run(ctx, []string{"backup", "restore", file}); code != exitError || !strings.Contains(td.stderr.String(), "stop it first") {
		t.Errorf("restore under a running daemon: code %d, stderr %q", code, td.stderr.String())
	}
	if kept, _ := filepath.Glob(filepath.Join(td.Home, "liman.db.replaced-*")); len(kept) != 0 {
		t.Errorf("the database was moved although the restore was refused: %v", kept)
	}

	// A backup, by contrast, is fine while the daemon runs.
	other := t.TempDir()
	if code := td.Run(ctx, []string{"backup", "create", other}); code != exitOK {
		t.Errorf("backup create under a running daemon: code %d, stderr %q", code, td.stderr.String())
	}

	stop()
	<-exit
}

func TestBackupRestoreRefusesABackupItCannotUse(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	dir := t.TempDir()
	ctx := context.Background()

	if code := td.Run(ctx, []string{"backup", "create", dir}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	file := filepath.Join(dir, backupsIn(t, dir)[0])
	before, err := os.ReadFile(filepath.Join(td.Home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}

	untouched := func(what string) {
		t.Helper()
		if kept, _ := filepath.Glob(filepath.Join(td.Home, "liman.db.re*")); len(kept) != 0 {
			t.Errorf("%s: files were created although the restore was refused: %v", what, kept)
		}
		if after, err := os.ReadFile(filepath.Join(td.Home, "liman.db")); err != nil || len(after) != len(before) {
			t.Errorf("%s: the database changed although the restore was refused", what)
		}
	}

	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := td.Run(ctx, []string{"backup", "restore", junk}); code != exitError {
		t.Errorf("restore of a text file: code %d, want %d", code, exitError)
	}
	untouched("a text file")

	replaceKey(t, td.Home)
	if code := td.Run(ctx, []string{"backup", "restore", file}); code != exitError || !strings.Contains(td.stderr.String(), "does not match its signature") {
		t.Errorf("restore with the wrong key: code %d, stderr %q", code, td.stderr.String())
	}
	untouched("the wrong key")

	if err := os.Remove(filepath.Join(td.Home, "master.key")); err != nil {
		t.Fatal(err)
	}
	if code := td.Run(ctx, []string{"backup", "restore", file}); code != exitError || !strings.Contains(td.stderr.String(), "could not be checked (unchecked)") {
		t.Errorf("restore without a key: code %d, stderr %q", code, td.stderr.String())
	}
	untouched("no key")
}

func TestBackupUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"backup"},
		{"backup", "nonsense"},
		{"backup", "create"},
		{"backup", "create", "--keep", "3"},
		{"backup", "create", "dir", "--keep", "-1"},
		{"backup", "create", "dir", "extra"},
		{"backup", "verify"},
		{"backup", "verify", "a", "b"},
		{"backup", "restore"},
	} {
		td := newTestDaemon(t)
		if code := td.Run(context.Background(), args); code != exitUsage {
			t.Errorf("limand %s: code %d, want %d", strings.Join(args, " "), code, exitUsage)
		}
	}
}

// forge adds a token an attacker knows to a backup file, as someone with
// write access to wherever the backups are stored could.
func forge(t *testing.T, file string) string {
	t.Helper()
	ctx := context.Background()

	// A scratch database gives a correctly hashed token row to copy over.
	scratch, err := store.Open(ctx, filepath.Join(t.TempDir(), "scratch.db"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := scratch.CreateToken(ctx, "attacker")
	if err != nil {
		t.Fatal(err)
	}
	if err := scratch.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	_ = scratch.Close()

	db, err := sql.Open("sqlite", "file:"+file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// Insert a row whose hash the attacker can produce the token for.
	if _, err := db.ExecContext(ctx, `INSERT INTO tokens (name, hash, scope, created_at) VALUES ('attacker', ?, 'admin', '2026-01-01T00:00:00.000000000Z')`, hashOf(token)); err != nil {
		t.Fatalf("forge the backup: %v", err)
	}
	return token
}

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestAChangedBackupIsRefused(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	dir := t.TempDir()
	ctx := context.Background()

	if code := td.Run(ctx, []string{"backup", "create", dir}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	file := filepath.Join(dir, backupsIn(t, dir)[0])

	token := forge(t, file)

	if code := td.Run(ctx, []string{"backup", "verify", file}); code != exitError || !strings.Contains(td.stdout.String(), "signature WRONG") {
		t.Errorf("verify of a changed backup: code %d, output %q", code, td.stdout.String())
	}
	if code := td.Run(ctx, []string{"backup", "restore", file}); code != exitError || !strings.Contains(td.stderr.String(), "does not match its signature") {
		t.Errorf("restore of a changed backup: code %d, stderr %q", code, td.stderr.String())
	}
	// Not even when told to accept unsigned backups: this one is signed,
	// and the signature says no.
	if code := td.Run(ctx, []string{"backup", "restore", file, "--allow-unsigned"}); code != exitError {
		t.Errorf("restore of a changed backup with --allow-unsigned: code %d, want %d", code, exitError)
	}

	// The attacker's token never became valid.
	st, err := store.Open(ctx, filepath.Join(td.Home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if ok, _ := st.VerifyToken(ctx, token); ok {
		t.Fatal("a token added to the backup is accepted after the refused restore")
	}
	if kept, _ := filepath.Glob(filepath.Join(td.Home, "liman.db.re*")); len(kept) != 0 {
		t.Errorf("files were created although the restore was refused: %v", kept)
	}
}

func TestAnUnsignedBackupNeedsAnExplicitDecision(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	dir := t.TempDir()
	ctx := context.Background()

	if code := td.Run(ctx, []string{"backup", "create", dir}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	file := filepath.Join(dir, backupsIn(t, dir)[0])
	// As a backup made by a version before signatures would be.
	if err := os.Remove(file + ".sig"); err != nil {
		t.Fatal(err)
	}

	if code := td.Run(ctx, []string{"backup", "verify", file}); code != exitOK || !strings.Contains(td.stdout.String(), "NOT signed") {
		t.Errorf("verify of an unsigned backup: code %d, output %q", code, td.stdout.String())
	}
	if code := td.Run(ctx, []string{"backup", "restore", file}); code != exitError || !strings.Contains(td.stderr.String(), "--allow-unsigned") {
		t.Errorf("restore of an unsigned backup: code %d, stderr %q", code, td.stderr.String())
	}
	if code := td.Run(ctx, []string{"backup", "restore", file, "--allow-unsigned"}); code != exitOK {
		t.Errorf("restore with --allow-unsigned: code %d, stderr %q", code, td.stderr.String())
	}
}

func TestASignatureBelongsToOneFile(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	first, second := t.TempDir(), t.TempDir()
	ctx := context.Background()

	if code := td.Run(ctx, []string{"backup", "create", first}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	// A different database state, so a different file.
	if code := td.Run(ctx, []string{"token", "create", "later"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	if code := td.Run(ctx, []string{"backup", "create", second}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	a := filepath.Join(first, backupsIn(t, first)[0])
	b := filepath.Join(second, backupsIn(t, second)[0])

	// A genuine signature, of another genuine backup.
	sig, err := os.ReadFile(a + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b+".sig", sig, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := td.Run(ctx, []string{"backup", "verify", b}); code != exitError {
		t.Errorf("verify with another backup's signature: code %d, want %d", code, exitError)
	}

	// Garbage in the signature file is a wrong signature, not a crash.
	if err := os.WriteFile(b+".sig", []byte("liman-backup-v1 hmac-sha256 zz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := td.Run(ctx, []string{"backup", "verify", b}); code != exitError {
		t.Errorf("verify with a malformed signature: code %d, want %d", code, exitError)
	}
}

func TestKeepRemovesSignaturesWithTheirBackups(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	dir := t.TempDir()

	for _, name := range []string{"liman-20200101-000000.db", "liman-20200101-000000.db.sig", "liman-20200102-000000.db", "liman-20200102-000000.db.sig"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code := td.Run(context.Background(), []string{"backup", "create", dir, "--keep", "1"}); code != exitOK {
		t.Fatalf("backup create --keep: code %d, stderr %q", code, td.stderr.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("after --keep 1 the directory holds %v, want one backup and its signature", names)
	}
}

func TestBackupRestoreDoesNotListExpiredTokensAsValid(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	dir := t.TempDir()
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(td.Home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateScopedToken(ctx, "old-ci", store.ScopeAdmin, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	if code := td.Run(ctx, []string{"backup", "create", dir}); code != exitOK {
		t.Fatalf("backup create: code %d, stderr %q", code, td.stderr.String())
	}
	if code := td.Run(ctx, []string{"backup", "restore", filepath.Join(dir, backupsIn(t, dir)[0])}); code != exitOK {
		t.Fatalf("backup restore: code %d, stderr %q", code, td.stderr.String())
	}
	if out := td.stdout.String(); !strings.Contains(out, "These tokens are valid now: laptop\n") {
		t.Errorf("restore lists an expired token as valid, or none at all:\n%s", out)
	}
}
