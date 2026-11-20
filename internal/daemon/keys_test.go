package daemon

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/secrets"
	"github.com/onrcanogul/liman/internal/store"
)

// secretsOf opens every secret in the database with the keyring at home
// and returns the IDs of the keys they are sealed with.
func secretsOf(t *testing.T, home string) []secrets.KeyID {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ring, err := secrets.LoadKeyring(filepath.Join(home, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	vault := secrets.NewVaultWithKeyring(ring, st)
	refs, err := st.SecretRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []secrets.KeyID
	for _, r := range refs {
		if _, err := vault.Get(ctx, r.App, r.Name); err != nil {
			t.Errorf("%s/%s does not open: %v", r.App, r.Name, err)
		}
		id, err := vault.SealedBy(ctx, r.App, r.Name)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestKeyRotate(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	ctx := context.Background()
	dir := t.TempDir()
	if code := td.Run(ctx, []string{"backup", "create", dir}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	before := filepath.Join(dir, backupsIn(t, dir)[0])
	oldKey, err := secrets.LoadKey(filepath.Join(td.Home, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	oldID := secrets.IDOf(oldKey)

	if code := td.Run(ctx, []string{"key", "rotate"}); code != exitOK {
		t.Fatalf("key rotate: code %d, stderr %q", code, td.stderr.String())
	}
	newKey, _ := secrets.LoadKey(filepath.Join(td.Home, "master.key"))
	newID := secrets.IDOf(newKey)
	out := td.stdout.String()
	for _, want := range []string{
		"Rotated the master key: " + oldID.String() + " -> " + newID.String(),
		"Sealed 1 secret again with the new key.",
		filepath.Join("master.key.old", oldID.String()+".key"),
		"rotating does not change the secrets",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, secretValue) || strings.Contains(td.stderr.String(), secretValue) {
		t.Error("the rotation printed a secret's value")
	}
	if ids := secretsOf(t, td.Home); len(ids) != 1 || ids[0] != newID {
		t.Errorf("secrets are sealed with %v, want %s", ids, newID)
	}

	// The backup from before is still good, with the old key.
	if code := td.Run(ctx, []string{"backup", "verify", before}); code != exitOK || !strings.Contains(td.stdout.String(), "what key "+oldID.String()) {
		t.Errorf("verify of the earlier backup: code %d, stderr %q", code, td.stderr.String())
	}
	// A new one is signed with the new key. (A second backup in the same
	// second would be refused; another directory sidesteps that.)
	dir2 := t.TempDir()
	if code := td.Run(ctx, []string{"backup", "create", dir2}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	sig, _ := os.ReadFile(filepath.Join(dir2, backupsIn(t, dir2)[0]) + ".sig")
	if !strings.HasPrefix(string(sig), "liman-backup-v2 hmac-sha256 "+newID.String()+" ") {
		t.Errorf("new signature = %q, want it to name %s", sig, newID)
	}
}

func TestKeyRotateNeedsAKey(t *testing.T) {
	td := newTestDaemon(t)
	if code := td.Run(context.Background(), []string{"key", "rotate"}); code != exitError || !strings.Contains(td.stderr.String(), "there is no master key") {
		t.Errorf("key rotate without a key: code %d, stderr %q", code, td.stderr.String())
	}
	if code := td.Run(context.Background(), []string{"key"}); code != exitUsage {
		t.Errorf("key without a subcommand: code %d, want %d", code, exitUsage)
	}
}

func TestKeyListAndRetire(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	ctx := context.Background()
	// Backups go where the daemon would put them, so that list and retire
	// count them.
	backups := filepath.Join(td.Home, "backups")
	if code := td.Run(ctx, []string{"backup", "create", backups}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	oldKey, _ := secrets.LoadKey(filepath.Join(td.Home, "master.key"))
	oldID := secrets.IDOf(oldKey)
	if code := td.Run(ctx, []string{"key", "rotate"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	newKey, _ := secrets.LoadKey(filepath.Join(td.Home, "master.key"))
	newID := secrets.IDOf(newKey)

	td.stdout.buf.Reset()
	if code := td.Run(ctx, []string{"key", "list"}); code != exitOK {
		t.Fatalf("key list: code %d, stderr %q", code, td.stderr.String())
	}
	out := td.stdout.String()
	for _, re := range []string{
		`(?m)^KEY\s+ROLE\s+SECRETS\s+BACKUPS$`,
		`(?m)^` + newID.String() + `\s+primary\s+1\s+0$`,
		`(?m)^` + oldID.String() + `\s+old \(opens only\)\s+0\s+1$`,
	} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("key list does not match %s:\n%s", re, out)
		}
	}

	// The primary key cannot be retired.
	if code := td.Run(ctx, []string{"key", "retire", newID.String()}); code != exitError || !strings.Contains(td.stderr.String(), "is the master key") {
		t.Errorf("retire the primary: code %d, stderr %q", code, td.stderr.String())
	}
	// The old one has a backup that needs it: refused, and the backup named.
	if code := td.Run(ctx, []string{"key", "retire", oldID.String()}); code != exitError || !strings.Contains(td.stderr.String(), backupsIn(t, backups)[0]) {
		t.Errorf("retire with a dependent backup: code %d, stderr %q", code, td.stderr.String())
	}
	if exists, _ := secrets.KeyExists(filepath.Join(td.Home, "master.key.old", oldID.String()+".key")); !exists {
		t.Fatal("the old key was removed although retiring was refused")
	}
	// --force retires it, and the backup can no longer be checked.
	if code := td.Run(ctx, []string{"key", "retire", oldID.String(), "--force"}); code != exitOK || !strings.Contains(td.stdout.String(), "Retired key "+oldID.String()) {
		t.Fatalf("retire --force: code %d, stderr %q", code, td.stderr.String())
	}
	if code := td.Run(ctx, []string{"backup", "verify", filepath.Join(backups, backupsIn(t, backups)[0])}); code != exitError || !strings.Contains(td.stderr.String(), "cannot be checked without key "+oldID.String()) {
		t.Errorf("verify after retiring its key: code %d, stderr %q", code, td.stderr.String())
	}
	if code := td.Run(ctx, []string{"key", "retire", oldID.String()}); code != exitError || !strings.Contains(td.stderr.String(), "there is no old key") {
		t.Errorf("retire twice: code %d, stderr %q", code, td.stderr.String())
	}
}

func TestKeyRetireRefusesWhileSecretsNeedIt(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	ctx := context.Background()
	// The key changes without resealing (as by hand, not by rotate): the
	// secret stays sealed with the old key.
	oldID, _ := rotateByHand(t, td.Home)

	if code := td.Run(ctx, []string{"key", "retire", oldID.String(), "--force"}); code != exitError || !strings.Contains(td.stderr.String(), "1 secret in the database is still sealed with") {
		t.Errorf("retire a key secrets need: code %d, stderr %q", code, td.stderr.String())
	}
}

func TestKeysForAPI(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	ctx := context.Background()
	if code := td.Run(ctx, []string{"backup", "create", filepath.Join(td.Home, "backups")}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	oldKey, _ := secrets.LoadKey(filepath.Join(td.Home, "master.key"))
	if code := td.Run(ctx, []string{"key", "rotate"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	newKey, _ := secrets.LoadKey(filepath.Join(td.Home, "master.key"))

	keys, err := td.keysForAPI(config.Config{})(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []api.Key{
		{ID: secrets.IDOf(newKey).String(), Role: "primary", Secrets: 1},
		{ID: secrets.IDOf(oldKey).String(), Role: "old", Backups: 1},
	}
	if !slices.Equal(keys.Keys, want) {
		t.Errorf("keys = %+v, want %+v", keys.Keys, want)
	}
}
