package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/secrets"
)

const keySynopsis = "limand key rotate"

func (d *Daemon) runKey(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return d.usageError(keySynopsis)
	}
	cfg, err := config.Load(filepath.Join(d.Home, "config.yaml"))
	if err != nil {
		return d.fail(err)
	}
	switch {
	case args[0] == "rotate" && len(args) == 1:
		return d.keyRotate(ctx, cfg)
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
