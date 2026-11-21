package app

import (
	"strings"
	"testing"
	"time"
)

func TestWorkerDefinition(t *testing.T) {
	a, err := Parse([]byte(`
name: orders-worker
image: ghcr.io/user/orders-api:1.4
kind: worker
command: [node, worker.js, --queue, orders]
deploy:
  strategy: replace
healthcheck:
  settle: 30s
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !a.Worker() || a.Strategy() != StrategyReplace || a.Settle() != 30*time.Second {
		t.Errorf("worker = %v, strategy %s, settle %s", a.Worker(), a.Strategy(), a.Settle())
	}
	if strings.Join(a.Command, " ") != "node worker.js --queue orders" {
		t.Errorf("Command = %q", a.Command)
	}
}

func TestDefaults(t *testing.T) {
	web := &App{Name: "web", Domain: "web.example.com", Port: 80}
	worker := &App{Name: "w", Kind: KindWorker}
	if web.Worker() || web.Strategy() != StrategyOverlap || web.Settle() != 0 {
		t.Errorf("web: worker %v, strategy %s, settle %s", web.Worker(), web.Strategy(), web.Settle())
	}
	if worker.Settle() != DefaultSettle {
		t.Errorf("worker settle = %s, want %s", worker.Settle(), DefaultSettle)
	}
}

func TestValidateKinds(t *testing.T) {
	tests := []struct {
		name, yaml, wantErr string
	}{
		{"web without domain", "name: w\nimage: a:1\nport: 80\n", "write kind: worker"},
		{"worker with domain", "name: w\nimage: a:1\nkind: worker\ndomain: w.example.com\n", "a worker is not reached from outside"},
		{"worker with port", "name: w\nimage: a:1\nkind: worker\nport: 80\n", "a worker listens on no port"},
		{"worker with health path", "name: w\nimage: a:1\nkind: worker\nhealthcheck:\n  path: /health\n", "a worker has no port to ask"},
		{"unknown kind", "name: w\nimage: a:1\nkind: cron\n", "kind: must be web or worker"},
		{"unknown strategy", "name: w\nimage: a:1\nkind: worker\ndeploy:\n  strategy: canary\n", "deploy.strategy: must be overlap or replace"},
		{"settle too long", "name: w\nimage: a:1\nkind: worker\nhealthcheck:\n  settle: 1h\n", "healthcheck.settle: must be between"},
		{"settle not a duration", "name: w\nimage: a:1\nkind: worker\nhealthcheck:\n  settle: ten\n", "is not a duration"},
		{"empty program", "name: w\nimage: a:1\nkind: worker\ncommand: ['', x]\n", "command: the first argument"},
		{"empty healthcheck", "name: w\nimage: a:1\ndomain: w.example.com\nport: 80\nhealthcheck: {}\n", "healthcheck.path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Parse() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
	// A web app may set only settle, for an app without a health path.
	if _, err := Parse([]byte("name: w\nimage: a:1\ndomain: w.example.com\nport: 80\nhealthcheck:\n  settle: 5s\n")); err != nil {
		t.Errorf("web with settle only: %v", err)
	}
}

// Upgrading liman must not make every app look changed: the checksum of a
// definition that uses none of the new fields is what it was.
func TestFieldsOfAnOldDefinitionAreUnchanged(t *testing.T) {
	a := &App{Name: "web", Image: "ghcr.io/u/web@sha256:" + strings.Repeat("a", 64), Domain: "web.example.com", Port: 80,
		Healthcheck: &Healthcheck{Path: "/health"}}
	want := `{"domain":"web.example.com","healthcheck.path":"/health","image":"ghcr.io/u/web@sha256:` + strings.Repeat("a", 64) + `","name":"web","port":"80"}`
	if got := string(Canonical(a.Fields())); got != want {
		t.Errorf("Canonical(Fields()) =\n%s\nwant\n%s", got, want)
	}
	// Writing the defaults out changes nothing either.
	a.Kind, a.Deploy.Strategy = KindWeb, StrategyOverlap
	if got := string(Canonical(a.Fields())); got != want {
		t.Errorf("with the defaults written out =\n%s", got)
	}
}

func TestFieldsSeeTheNewFields(t *testing.T) {
	a := &App{Name: "w", Image: "a:1", Kind: KindWorker, Command: []string{"a", "b"}, Deploy: Deploy{Strategy: StrategyReplace},
		Healthcheck: &Healthcheck{Settle: Duration(30 * time.Second)}}
	f := a.Fields()
	for k, want := range map[string]string{"kind": "worker", "command": `["a","b"]`, "deploy.strategy": "replace", "healthcheck.settle": "30s"} {
		if f[k] != want {
			t.Errorf("Fields()[%s] = %q, want %q", k, f[k], want)
		}
	}
	// The order of the arguments is part of the command.
	b := *a
	b.Command = []string{"b", "a"}
	if b.Fields()["command"] == f["command"] {
		t.Error("reordered command arguments look the same")
	}
}
