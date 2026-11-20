package daemon

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/audit"
	"github.com/onrcanogul/liman/internal/monitor"
	"github.com/onrcanogul/liman/internal/secrets"
	"github.com/onrcanogul/liman/internal/store"
)

// actionsIn returns "actor action target" for every audit entry, actors cut
// to their kind ("server", "token").
func actionsIn(t *testing.T, home string) []string {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	rows, err := st.AuditEntries(context.Background(), nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range rows {
		kind, _, _ := strings.Cut(e.Actor, ":")
		out = append(out, kind+" "+e.Action+" "+e.Target)
	}
	return out
}

func TestServerCommandsAreAuditedAndVerified(t *testing.T) {
	td := newTestDaemon(t)
	seed(t, td)
	ctx := context.Background()
	oldKey, err := secrets.LoadKey(filepath.Join(td.Home, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	oldID := secrets.IDOf(oldKey).String()

	for _, args := range [][]string{
		{"token", "create", "ci", "--app", "web", "--read-only"},
		{"token", "revoke", "ci"},
		{"backup", "create", t.TempDir()},
		{"key", "rotate"},
	} {
		if code := td.Run(ctx, args); code != exitOK {
			t.Fatalf("%v: code %d, stderr %q", args, code, td.stderr.String())
		}
	}
	got := strings.Join(actionsIn(t, td.Home), "\n")
	for _, want := range []string{
		"server token.create ci (read, web)",
		"server token.revoke ci",
		"server backup.create liman-",
		"server key.rotate " + oldID + " -> ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("audit trail misses %q:\n%s", want, got)
		}
	}
	// The token itself is nowhere in it.
	if strings.Contains(got, "liman_") {
		t.Errorf("the audit trail contains a token:\n%s", got)
	}

	mark := len(td.stdout.String())
	if code := td.Run(ctx, []string{"audit", "verify"}); code != exitOK {
		t.Fatalf("audit verify: code %d, stdout %q, stderr %q", code, td.stdout.String()[mark:], td.stderr.String())
	}
	if out := td.stdout.String()[mark:]; !strings.Contains(out, "intact: 4 records") || !strings.Contains(out, "Last record: 4,") {
		t.Errorf("audit verify says:\n%s", out)
	}

	// Retiring the old key leaves its records unchecked, not broken.
	mark = len(td.stdout.String())
	if code := td.Run(ctx, []string{"key", "retire", oldID, "--force"}); code != exitOK {
		t.Fatalf("key retire: code %d, stderr %q", code, td.stderr.String())
	}
	if out := td.stdout.String()[mark:]; !strings.Contains(out, "3 audit records signed with it can no longer be checked") {
		t.Errorf("key retire says:\n%s", out)
	}
	mark = len(td.stdout.String())
	if code := td.Run(ctx, []string{"audit", "verify"}); code != exitOK {
		t.Fatalf("audit verify after retiring: code %d, stdout %q", code, td.stdout.String()[mark:])
	}
	if out := td.stdout.String()[mark:]; !strings.Contains(out, "3 records signed with key "+oldID+" could not be checked") {
		t.Errorf("audit verify after retiring says:\n%s", out)
	}

	// A record changed in the file is found.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(td.Home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE audit SET target = 'someone-else' WHERE seq = 5`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	mark = len(td.stdout.String())
	if code := td.Run(ctx, []string{"audit", "verify"}); code != exitError {
		t.Errorf("audit verify of a changed trail: code %d, want %d", code, exitError)
	}
	if out := td.stdout.String()[mark:]; !strings.Contains(out, "BROKEN at record 5") {
		t.Errorf("audit verify of a changed trail says:\n%s", out)
	}
}

func TestMonitorRestartsAreAudited(t *testing.T) {
	td := newTestDaemon(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(td.Home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rec := &audit.Recorder{Store: st}
	onRestart := recordRestart(st, rec, slog.New(slog.DiscardHandler))
	onRestart(monitor.Event{App: "web", Release: "r1", Container: "web-r1", Reason: "3 failed checks", Started: time.Now()})
	onRestart(monitor.Event{App: "web", Release: "r1", Container: "web-r1", Reason: "3 failed checks", Err: errors.New("no such container"), Started: time.Now()})

	rows, err := st.AuditEntries(ctx, nil, 10)
	if err != nil || len(rows) != 2 || rows[0].Actor != audit.Monitor || rows[0].Action != audit.Restart || rows[0].App != "web" || rows[0].Target != "web-r1" || !rows[0].OK || rows[1].OK || rows[1].Error != "no such container" {
		t.Errorf("audit trail = %+v, %v; want the monitor's two restarts, the second failed", rows, err)
	}
}

// The daemon writes the head of the trail to its log when it starts, so
// records added while it was stopped are anchored from then on.
func TestServeAnchorsTheHeadOfTheTrail(t *testing.T) {
	td := newTestDaemon(t)
	if code := td.Run(context.Background(), []string{"token", "create", "dev"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	_, stop, exit := startServing(t, td)
	stop()
	<-exit
	if !regexp.MustCompile(`msg=audit seq=1 action=token\.create mac=[0-9a-f]{64} head=true`).MatchString(td.stdout.String() + td.stderr.String()) {
		t.Errorf("no head line in the log:\n%s", td.stdout.String()+td.stderr.String())
	}
}
