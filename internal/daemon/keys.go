package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/backup"
	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/secrets"
)

const keySynopsis = "limand key list | rotate | retire <id> [--force]"

func (d *Daemon) runKey(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return d.usageError(keySynopsis)
	}
	cfg, err := config.Load(filepath.Join(d.Home, "config.yaml"))
	if err != nil {
		return d.fail(err)
	}
	switch {
	case args[0] == "list" && len(args) == 1:
		return d.keyList(ctx, cfg)
	case args[0] == "rotate" && len(args) == 1:
		return d.keyRotate(ctx, cfg)
	case args[0] == "retire" && len(args) == 2:
		return d.keyRetire(ctx, cfg, args[1], false)
	case args[0] == "retire" && len(args) == 3 && args[2] == "--force":
		return d.keyRetire(ctx, cfg, args[1], true)
	default:
		return d.usageError(keySynopsis)
	}
}

// keyRotate replaces the master key and seals every secret again with the
// new one. See secrets.Rotate for the order of the steps and why it is safe
// to run again after an interruption.
func (d *Daemon) keyRotate(ctx context.Context, cfg config.Config) int {
	path := d.keyPath(cfg)
	if exists, err := secrets.KeyExists(path); err != nil {
		return d.fail(err)
	} else if !exists {
		return d.fail(fmt.Errorf("there is no master key at %s yet; the daemon creates one when it first starts", path))
	}
	// The daemon holds the key in memory and would go on sealing new
	// secrets with the old one.
	if daemonAnswers(ctx, cfg.Daemon.Listen) {
		return d.fail(fmt.Errorf("the daemon is running on %s; stop it first (systemctl stop limand)", cfg.Daemon.Listen))
	}

	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(err)
	}
	defer func() { _ = st.Close() }()

	rot, err := secrets.Rotate(ctx, path, st)
	if err != nil {
		var unknown *secrets.UnknownKeyError
		if errors.As(err, &unknown) {
			return d.fail(fmt.Errorf("%w. Put that key at %s, or remove the secret (liman secret rm) and set it again", err,
				filepath.Join(secrets.OldKeysDir(path), unknown.ID.String()+".key")))
		}
		return d.fail(err)
	}

	if rot.Resumed {
		fmt.Fprintf(d.Stdout, "Finished an interrupted rotation: the master key is now %s (was %s)\n", rot.New, rot.Old)
	} else {
		fmt.Fprintf(d.Stdout, "Rotated the master key: %s -> %s\n", rot.Old, rot.New)
	}
	fmt.Fprintf(d.Stdout, "Sealed %s again with the new key.\n", count(rot.Resealed, "secret"))
	fmt.Fprintf(d.Stdout, `
The old key is kept at %s.
Backups made until now are signed with it and can still be checked and
restored; new ones are signed with %s.

Now:
  - Back up the new %s. It is not part of any backup.
  - If the old key may have leaked: rotating does not change the secrets
    themselves. Change each value where it comes from (the database
    password, the API key) and store it again with liman secret set.
  - Start the daemon again: systemctl start limand
`, filepath.Join(secrets.OldKeysDir(path), rot.Old.String()+".key"), rot.New, path)
	return exitOK
}

// backupDir is where the daemon writes backups: backup.dir, or "backups"
// in liman's home.
func (d *Daemon) backupDir(cfg config.Config) string {
	if cfg.Backup.Dir != "" {
		return cfg.Backup.Dir
	}
	return filepath.Join(d.Home, "backups")
}

// keyUse is what depends on one key.
type keyUse struct {
	secrets int
	backups []string
}

// keyUses finds, for every key, the secrets in the database sealed with it
// and the backups in the backup directory signed with it. Keys the
// keyring does not hold appear too, when something names them.
func (d *Daemon) keyUses(ctx context.Context, cfg config.Config, ring *secrets.Keyring) (map[secrets.KeyID]*keyUse, error) {
	uses := map[secrets.KeyID]*keyUse{}
	use := func(id secrets.KeyID) *keyUse {
		if uses[id] == nil {
			uses[id] = &keyUse{}
		}
		return uses[id]
	}
	for _, id := range ring.IDs() {
		use(id)
	}

	st, err := d.openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	refs, err := st.SecretRefs(ctx)
	if err != nil {
		return nil, err
	}
	vault := secrets.NewVaultWithKeyring(ring, st)
	for _, r := range refs {
		id, err := vault.SealedBy(ctx, r.App, r.Name)
		var unknown *secrets.UnknownKeyError
		switch {
		case errors.As(err, &unknown):
			use(unknown.ID).secrets++
		case err != nil:
			return nil, fmt.Errorf("%s/%s: %w", r.App, r.Name, err)
		default:
			use(id).secrets++
		}
	}

	dir := d.backupDir(cfg)
	entries, err := backup.List(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name)
		_, by, err := backup.SignatureWith(path, path+backup.SigSuffix, ring)
		if err != nil {
			return nil, err
		}
		if id, err := secrets.ParseKeyID(by); err == nil {
			use(id).backups = append(use(id).backups, e.Name)
		}
	}
	return uses, nil
}

func (d *Daemon) keyList(ctx context.Context, cfg config.Config) int {
	path := d.keyPath(cfg)
	ring, err := secrets.LoadKeyring(path)
	if err != nil {
		return d.fail(err)
	}
	uses, err := d.keyUses(ctx, cfg, ring)
	if err != nil {
		return d.fail(err)
	}

	w := tabwriter.NewWriter(d.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tROLE\tSECRETS\tBACKUPS")
	for _, k := range keyRows(ring, uses) {
		role := k.Role
		switch role {
		case "old":
			role = "old (opens only)"
		case "missing":
			role = "MISSING"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\n", k.ID, role, k.Secrets, k.Backups)
	}
	if err := w.Flush(); err != nil {
		return d.fail(err)
	}
	fmt.Fprintf(d.Stdout, "\nBackups counted in %s; old keys are in %s.\n", d.backupDir(cfg), secrets.OldKeysDir(path))
	if _, hasPending := ring.Pending(); hasPending {
		fmt.Fprintln(d.Stdout, "A rotation did not finish; finish it with: limand key rotate (daemon stopped)")
	}
	return exitOK
}

// keyRows describes every key: those in the keyring, the primary first,
// then any that something names and the keyring lacks. Roles are as in
// api.Key.
func keyRows(ring *secrets.Keyring, uses map[secrets.KeyID]*keyUse) []api.Key {
	pending, hasPending := ring.Pending()
	ids := ring.IDs()
	var missing []secrets.KeyID
	for id := range uses {
		if !ring.Has(id) {
			missing = append(missing, id)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].String() < missing[j].String() })

	var rows []api.Key
	for _, id := range append(ids, missing...) {
		role := "old"
		switch {
		case id == ring.Primary():
			role = "primary"
		case hasPending && id == pending:
			role = "unfinished rotation"
		case !ring.Has(id):
			role = "missing"
		}
		row := api.Key{ID: id.String(), Role: role}
		if u := uses[id]; u != nil {
			row.Secrets, row.Backups = u.secrets, len(u.backups)
		}
		rows = append(rows, row)
	}
	return rows
}

// keysForAPI is GET /v1/keys: the keyring as it is on disk now, with what
// depends on each key.
func (d *Daemon) keysForAPI(cfg config.Config) func(ctx context.Context) (api.Keys, error) {
	return func(ctx context.Context) (api.Keys, error) {
		ring, err := secrets.LoadKeyring(d.keyPath(cfg))
		if err != nil {
			return api.Keys{}, err
		}
		uses, err := d.keyUses(ctx, cfg, ring)
		if err != nil {
			return api.Keys{}, err
		}
		return api.Keys{Keys: keyRows(ring, uses)}, nil
	}
}

// keyRetire deletes an old key. What still depends on it is said first:
// secrets in the database stop it outright, backups only with --force.
func (d *Daemon) keyRetire(ctx context.Context, cfg config.Config, idText string, force bool) int {
	id, err := secrets.ParseKeyID(idText)
	if err != nil {
		return d.fail(err)
	}
	path := d.keyPath(cfg)
	ring, err := secrets.LoadKeyring(path)
	if err != nil {
		return d.fail(err)
	}
	pending, hasPending := ring.Pending()
	switch {
	case id == ring.Primary():
		return d.fail(fmt.Errorf("%s is the master key; rotate first (limand key rotate), then retire it", id))
	case hasPending && id == pending:
		return d.fail(fmt.Errorf("%s is the new key of an unfinished rotation; finish it with: limand key rotate", id))
	case !ring.Has(id):
		return d.fail(fmt.Errorf("there is no old key %s (limand key list shows them)", id))
	}

	uses, err := d.keyUses(ctx, cfg, ring)
	if err != nil {
		return d.fail(err)
	}
	u := uses[id]
	if u.secrets > 0 {
		verb := "are"
		if u.secrets == 1 {
			verb = "is"
		}
		return d.fail(fmt.Errorf("%s in the database %s still sealed with %s; seal them with the master key first: limand key rotate",
			count(u.secrets, "secret"), verb, id))
	}
	if len(u.backups) > 0 && !force {
		fmt.Fprintf(d.Stderr, "These backups in %s are signed with %s and cannot be checked or restored without it:\n", d.backupDir(cfg), id)
		for _, b := range u.backups {
			fmt.Fprintf(d.Stderr, "  %s\n", b)
		}
		return d.fail(errors.New("not retired; remove those backups first, or retire the key anyway with --force"))
	}

	file := filepath.Join(secrets.OldKeysDir(path), id.String()+".key")
	if err := os.Remove(file); err != nil {
		return d.fail(err)
	}
	fmt.Fprintf(d.Stdout, "Retired key %s: removed %s.\n", id, file)
	if len(u.backups) > 0 {
		fmt.Fprintf(d.Stdout, "%s signed with it can no longer be checked or restored.\n", count(len(u.backups), "backup"))
	}
	fmt.Fprintln(d.Stdout, "Copies of the key elsewhere (another backup of the server, a password manager) still exist until you remove them too.")
	return exitOK
}
