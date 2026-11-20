package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onrcanogul/liman/internal/backup"
	"github.com/onrcanogul/liman/internal/store"
)

// downgrade turns the database in home back into what the previous schema
// version left: the last migration (token expiry) undone.
func downgrade(t *testing.T, home string) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, q := range []string{"ALTER TABLE tokens DROP COLUMN expires_at", "PRAGMA user_version = 4"} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestUpgradeBacksUpFirstAndOnlyTheDaemonUpgrades(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	downgrade(t, td.Home)
	ctx := context.Background()
	dbPath := filepath.Join(td.Home, "liman.db")

	// A command does not upgrade, and says what to do.
	if code := td.Run(ctx, []string{"token", "list"}); code != exitError || !strings.Contains(td.stderr.String(), "start the daemon once") {
		t.Fatalf("token list on an old database: code %d, stderr %q", code, td.stderr.String())
	}
	if v, _ := store.SchemaVersionOf(ctx, dbPath); v != 4 {
		t.Fatalf("a command changed the schema to %d", v)
	}

	// The daemon backs up, then upgrades.
	_, stop, exit := startServing(t, td)
	stop()
	<-exit

	if v, _ := store.SchemaVersionOf(ctx, dbPath); v != store.SchemaVersion() {
		t.Errorf("schema after the daemon started = %d, want %d", v, store.SchemaVersion())
	}
	log := td.stderr.String()
	if !strings.Contains(log, "backed up the database before upgrading it") || !strings.Contains(log, `msg="upgraded the database" from_schema=4`) {
		t.Errorf("the log does not tell about the backup and the upgrade:\n%s", log)
	}

	dir := filepath.Join(td.Home, "backups")
	names := backupsIn(t, dir)
	if len(names) != 1 {
		t.Fatalf("backups = %v, want the one made before the upgrade", names)
	}
	file := filepath.Join(dir, names[0])
	// The backup is the old database, as it was...
	if v, _ := store.SchemaVersionOf(ctx, file); v != 4 {
		t.Errorf("the backup has schema %d, want 4 (taken before the upgrade)", v)
	}
	// ...signed in the format the previous version can check...
	sig, _ := os.ReadFile(file + backup.SigSuffix)
	if !strings.HasPrefix(string(sig), "liman-backup-v1 hmac-sha256 ") {
		t.Errorf("signature = %q, want format v1", sig)
	}
	// ...and good.
	rep, err := backup.Check(ctx, file, filepath.Join(td.Home, "master.key"))
	if err != nil || !rep.Usable() || rep.Signature != backup.SigValid || rep.Secrets != 1 {
		t.Errorf("the pre-upgrade backup checks as %+v, %v", rep, err)
	}

	// Started again on an up-to-date database: no further backup.
	_, stop, exit = startServing(t, td)
	stop()
	<-exit
	if names := backupsIn(t, dir); len(names) != 1 {
		t.Errorf("backups after a second start = %v, want still one", names)
	}
}

func TestUpgradeDoesNotHappenWithoutItsBackup(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	downgrade(t, td.Home)
	// The backup directory cannot be created: a file is in its way.
	if err := os.WriteFile(filepath.Join(td.Home, "backups"), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := td.Run(context.Background(), nil); code != exitError || !strings.Contains(td.stderr.String(), "backing it up before the upgrade failed, so nothing was changed") {
		t.Fatalf("serve without a place for the backup: code %d, stderr %q", code, td.stderr.String())
	}
	if v, _ := store.SchemaVersionOf(context.Background(), filepath.Join(td.Home, "liman.db")); v != 4 {
		t.Errorf("the schema changed to %d without a backup", v)
	}
}
