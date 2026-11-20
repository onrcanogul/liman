// Package backup creates, lists and checks backups of the liman database.
//
// It is shared by the two ways of reaching it: the "limand backup" command
// and the daemon's API, which the panel uses. Restoring is not here: it
// needs the daemon stopped and belongs to the command alone.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/onrcanogul/liman/internal/secrets"
	"github.com/onrcanogul/liman/internal/store"
)

// nameLayout is the timestamp inside a backup's file name. It is UTC, so
// the names sort in the order the backups were made.
const nameLayout = "20060102-150405"

// NameRe is the shape of the file names Create writes. Nothing that does
// not match it is ever listed, opened or removed: a directory shared with
// other files loses nothing, and a name from a request cannot point
// outside the directory.
var NameRe = regexp.MustCompile(`^liman-\d{8}-\d{6}\.db$`)

// ErrBusy is returned when a backup is requested while one is being made.
var ErrBusy = errors.New("a backup is already being made")

// ErrNotFound is returned when the named backup does not exist.
var ErrNotFound = errors.New("no such backup")

// Report is what checking a backup file found.
type Report struct {
	Summary store.Summary
	// Secrets is how many secrets the file holds.
	Secrets int
	// KeyChecked is false when there was no master key to test them with.
	KeyChecked bool
	// Undecryptable names the secrets the master key does not open.
	Undecryptable []string
	// MissingKeys are the IDs of keys some of the secrets were sealed with
	// and the keyring does not hold.
	MissingKeys []string
	// Signature is one of the Sig constants.
	Signature string
	// SignedBy is the ID of the key that signed the file, when known.
	SignedBy string
}

// Usable reports whether the backup can be relied on: its signature is not
// wrong, and every secret in it can be decrypted. A missing signature does
// not make a backup unusable by itself; a restore decides about that.
func (r Report) Usable() bool {
	return r.Signature != SigInvalid && r.Signature != SigUnknownKey && len(r.Undecryptable) == 0 && (r.Secrets == 0 || r.KeyChecked)
}

// Entry describes one backup file.
type Entry struct {
	Name string
	Size int64
	// Created is taken from the file's name, which cannot change by
	// copying the file around the way its modification time can.
	Created time.Time
}

// Check opens a backup without changing it, checks it for damage and tries
// every secret in it against the master key at keyPath.
//
// A backup that cannot be decrypted is worth as much as no backup, and the
// time to find that out is now, not on the day it is needed.
func Check(ctx context.Context, path, keyPath string) (Report, error) {
	return CheckWithSignature(ctx, path, path+SigSuffix, keyPath)
}

// CheckWithSignature is Check for a file whose signature is kept elsewhere.
func CheckWithSignature(ctx context.Context, path, sigPath, keyPath string) (Report, error) {
	// The signature comes first. A file that fails it is not opened as a
	// database at all: parsing is exactly what should not be done with
	// bytes somebody may have crafted.
	sig, signedBy, err := signatureState(path, sigPath, keyPath)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Signature: sig, SignedBy: signedBy}
	if sig == SigInvalid || sig == SigUnknownKey {
		return rep, nil
	}

	st, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return rep, err
	}
	defer func() { _ = st.Close() }()

	if rep.Summary, err = st.Inspect(ctx); err != nil {
		return rep, err
	}

	refs, err := st.SecretRefs(ctx)
	if err != nil {
		return rep, err
	}
	rep.Secrets = len(refs)
	if len(refs) == 0 {
		return rep, nil
	}

	ring, err := loadKeyring(keyPath)
	if err != nil || ring == nil {
		return rep, err
	}
	vault := secrets.NewVaultWithKeyring(ring, st)

	rep.KeyChecked = true
	missing := map[string]bool{}
	for _, ref := range refs {
		// The value is decrypted and dropped at once; only whether it
		// worked is kept, and, if a key is missing, which one.
		if _, err := vault.Get(ctx, ref.App, ref.Name); err != nil {
			rep.Undecryptable = append(rep.Undecryptable, ref.App+"/"+ref.Name)
			var unknown *secrets.UnknownKeyError
			if errors.As(err, &unknown) && !missing[unknown.ID.String()] {
				missing[unknown.ID.String()] = true
				rep.MissingKeys = append(rep.MissingKeys, unknown.ID.String())
			}
		}
	}
	return rep, nil
}

// Create writes a backup of st into dir, reads it back and checks it. It
// returns the path of the new file.
func Create(ctx context.Context, st *store.Store, dir, keyPath string) (string, Report, error) {
	return create(ctx, st, dir, keyPath, false)
}

// CreateBeforeUpgrade backs up the database at path exactly as it is,
// before a newer liman upgrades its schema. It does not open the database
// for writing, so nothing about it changes.
//
// The backup is what going back to the previous version restores, so that
// version must be able to check it: the signature is written in format v1,
// which every version that signs backups reads.
func CreateBeforeUpgrade(ctx context.Context, path, dir, keyPath string) (string, Report, error) {
	st, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return "", Report{}, err
	}
	defer func() { _ = st.Close() }()
	return create(ctx, st, dir, keyPath, true)
}

func create(ctx context.Context, st *store.Store, dir, keyPath string, legacySignature bool) (string, Report, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", Report{}, fmt.Errorf("create backup directory: %w", err)
	}

	dest := filepath.Join(dir, "liman-"+time.Now().UTC().Format(nameLayout)+".db")
	// Names have a resolution of one second. Asked twice within one, the
	// second request is told that a backup was just made, which is true,
	// instead of failing on the name.
	if _, err := os.Lstat(dest); err == nil {
		return "", Report{}, fmt.Errorf("%w: one was written less than a second ago", ErrBusy)
	}
	if err := st.Snapshot(ctx, dest); err != nil {
		return "", Report{}, err
	}

	if _, err := sign(dest, keyPath, legacySignature); err != nil {
		return dest, Report{}, err
	}

	// Read the file back. What was written is only known to be a backup
	// once it has been opened as one.
	rep, err := Check(ctx, dest, keyPath)
	if err != nil {
		return dest, rep, fmt.Errorf("the backup %s was written but does not pass its check: %w", dest, err)
	}
	return dest, rep, nil
}

// List returns the backups in dir, newest first. A directory that does not
// exist yet holds no backups.
func List(dir string) ([]Entry, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}

	var out []Entry
	for _, e := range entries {
		// Regular files with exactly our naming scheme; anything else in
		// the directory is somebody else's.
		if !e.Type().IsRegular() || !NameRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed between the listing and this line
		}
		created, err := time.Parse("liman-"+nameLayout+".db", e.Name())
		if err != nil {
			continue // digits that are not a date, such as month 13
		}
		out = append(out, Entry{Name: e.Name(), Size: info.Size(), Created: created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Prune removes all but the newest keep backups in dir and returns how
// many it removed.
func Prune(dir string, keep int) (int, error) {
	entries, err := List(dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for i, e := range entries {
		if i < keep {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name)); err != nil {
			return removed, fmt.Errorf("remove old backup: %w", err)
		}
		// Its signature goes with it; there may be none.
		if err := os.Remove(filepath.Join(dir, e.Name+SigSuffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, fmt.Errorf("remove old backup: %w", err)
		}
		removed++
	}
	return removed, nil
}

// Service is the backup facility of a running daemon: one directory, one
// database, and at most one backup being made at a time.
type Service struct {
	Store *store.Store
	// Dir is where backups are written and looked for.
	Dir string
	// KeyPath is the master key the secrets in a backup are tested with.
	KeyPath string
	// Keep is how many backups stay after a new one was made; zero keeps
	// them all.
	Keep int

	// mu is held while a backup is being made. A second request does not
	// queue up behind it: it is told that one is running.
	mu sync.Mutex
}

// Create makes a backup now and, if Keep is set, removes older ones. It
// returns the new entry, what its check found and how many were removed.
func (s *Service) Create(ctx context.Context) (Entry, Report, int, error) {
	// TryLock returns at once: true with the lock held, or false.
	if !s.mu.TryLock() {
		return Entry{}, Report{}, 0, ErrBusy
	}
	defer s.mu.Unlock()

	path, rep, err := Create(ctx, s.Store, s.Dir, s.KeyPath)
	if err != nil {
		return Entry{}, rep, 0, err
	}
	entry := Entry{Name: filepath.Base(path)}
	if info, err := os.Stat(path); err == nil {
		entry.Size = info.Size()
	}
	entry.Created, _ = time.Parse("liman-"+nameLayout+".db", entry.Name)

	// Older backups go only when the new one is good in every respect.
	removed := 0
	if s.Keep > 0 && rep.Usable() {
		if removed, err = Prune(s.Dir, s.Keep); err != nil {
			return entry, rep, removed, err
		}
	}
	return entry, rep, removed, nil
}

// List returns the backups, newest first.
func (s *Service) List() ([]Entry, error) {
	return List(s.Dir)
}

// Verify checks the backup called name.
func (s *Service) Verify(ctx context.Context, name string) (Report, error) {
	// The name is matched against the pattern before it is joined to the
	// directory: "../../etc/passwd" never becomes a path.
	if !NameRe.MatchString(name) {
		return Report{}, ErrNotFound
	}
	path := filepath.Join(s.Dir, name)
	if _, err := os.Stat(path); err != nil {
		return Report{}, ErrNotFound
	}
	return Check(ctx, path, s.KeyPath)
}
