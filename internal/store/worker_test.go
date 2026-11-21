package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestWorkersHaveNoDomainAndDoNotCollide(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// What the trial showed: under NOT NULL UNIQUE the second worker's
	// empty domain collided with the first one's.
	for _, name := range []string{"mailer", "scheduler"} {
		if err := s.Register(ctx, Registration{Name: name, Repo: "ghcr.io/u/" + name, Kind: "worker"}); err != nil {
			t.Fatalf("Register(%s) error = %v", name, err)
		}
	}
	if err := s.Register(ctx, Registration{Name: "web", Repo: "ghcr.io/u/web", Domain: "web.example.com"}); err != nil {
		t.Fatal(err)
	}
	// A real domain is still one app's.
	if err := s.Register(ctx, Registration{Name: "web2", Repo: "ghcr.io/u/web2", Domain: "web.example.com"}); !errors.Is(err, ErrDomainTaken) {
		t.Errorf("second web.example.com error = %v, want ErrDomainTaken", err)
	}

	r, found, err := s.Registration(ctx, "mailer")
	if err != nil || !found || r.Domain != "" || !r.Worker() {
		t.Errorf("Registration(mailer) = %+v, %v, %v", r, found, err)
	}
	if r, _, _ := s.Registration(ctx, "web"); r.Kind != "web" || r.Worker() {
		t.Errorf("Registration(web) = %+v; a registration without a kind is a web app", r)
	}
	// Nobody owns "no domain".
	if owner, err := s.DomainOwner(ctx, ""); owner != "" || err != nil {
		t.Errorf("DomainOwner(\"\") = %q, %v", owner, err)
	}
	regs, err := s.Registrations(ctx)
	if err != nil || len(regs) != 3 {
		t.Fatalf("Registrations() = %+v, %v", regs, err)
	}
}

// Registrations written before migration 12 come through it as web apps,
// with their domains.
func TestMigrationKeepsRegistrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liman.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:11] {
		if _, err := db.ExecContext(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 11`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO apps (name, repo, domain, created_at) VALUES ('web', 'ghcr.io/u/web', 'web.example.com', '2026-11-20T10:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	r, found, err := s.Registration(ctx, "web")
	if err != nil || !found || r.Domain != "web.example.com" || r.Kind != "web" || r.Repo != "ghcr.io/u/web" {
		t.Errorf("after migration = %+v, %v, %v", r, found, err)
	}
	if err := s.Register(ctx, Registration{Name: "other", Repo: "ghcr.io/u/o", Domain: "web.example.com"}); !errors.Is(err, ErrDomainTaken) {
		t.Errorf("the domain is no longer protected after migration: %v", err)
	}
}
