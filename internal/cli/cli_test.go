package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/server/servertest"
)

const validDefinition = `
name: orders-api
image: ghcr.io/user/orders-api:1.4
domain: api.example.com
port: 3000
`

const invalidDefinition = `
name: orders-api
image: ghcr.io/user/orders-api
domain: api.example.com
port: 3000
`

// harness is a CLI wired to a complete test daemon: a real HTTP server and
// database, with Docker and Nginx replaced by stand-ins. Every command
// therefore travels the same path as in production.
type harness struct {
	*servertest.Harness
	cli    *CLI
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func newHarness(t *testing.T, stdin string) *harness {
	t.Helper()

	h := &harness{Harness: servertest.New(t)}
	h.cli = &CLI{
		Stdin:  strings.NewReader(stdin),
		Stdout: &h.stdout,
		Stderr: &h.stderr,
		API:    h.Client,
	}
	return h
}

func (h *harness) run(args ...string) int {
	return h.cli.Run(context.Background(), args)
}

func TestRun(t *testing.T) {
	validFile := writeFile(t, validDefinition)
	invalidFile := writeFile(t, invalidDefinition)

	web := engine.Container{Name: "web-r1", App: "web", Image: "web:1.0", Status: "Up 2 minutes", HostPort: 40123}

	tests := []struct {
		name string
		args []string
		// setup prepares the harness; nil leaves it empty.
		setup      func(h *harness)
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"version"}, wantCode: exitOK, wantStdout: "liman dev"},
		{name: "help", args: []string{"help"}, wantCode: exitOK, wantStdout: "Usage:"},
		{name: "help flag", args: []string{"--help"}, wantCode: exitOK, wantStdout: "Usage:"},
		{name: "no arguments", args: nil, wantCode: exitUsage, wantStderr: "Usage:"},
		{name: "unknown command", args: []string{"nope"}, wantCode: exitUsage, wantStderr: `unknown command "nope"`},

		{name: "plan", args: []string{"plan", validFile}, wantCode: exitOK, wantStdout: "$ docker pull ghcr.io/user/orders-api:1.4"},
		{name: "plan without file", args: []string{"plan"}, wantCode: exitUsage, wantStderr: "usage: liman plan <file>"},
		{name: "plan rejects flags", args: []string{"plan", "--step", validFile}, wantCode: exitUsage, wantStderr: "usage: liman plan <file>"},
		{name: "plan with missing file", args: []string{"plan", "missing.yaml"}, wantCode: exitError, wantStderr: "no such file"},
		{name: "plan with invalid file", args: []string{"plan", invalidFile}, wantCode: exitError, wantStderr: "must include a tag"},
		{
			name:       "plan works without docker",
			args:       []string{"plan", validFile},
			setup:      func(h *harness) { h.ConnectErr = servertest.ErrDockerDown },
			wantCode:   exitOK,
			wantStdout: "Dry run for orders-api: nothing will be changed.",
		},

		{name: "deploy", args: []string{"deploy", validFile}, wantCode: exitOK, wantStdout: "orders-api is running on the server at 127.0.0.1:40001 (release r1)"},
		{name: "deploy without file", args: []string{"deploy"}, wantCode: exitUsage, wantStderr: "usage: liman deploy"},
		{name: "deploy with unknown flag", args: []string{"deploy", "--force", validFile}, wantCode: exitUsage, wantStderr: "usage: liman deploy"},
		{name: "deploy with invalid file", args: []string{"deploy", invalidFile}, wantCode: exitError, wantStderr: "must include a tag"},
		{
			name:       "deploy without docker",
			args:       []string{"deploy", validFile},
			setup:      func(h *harness) { h.ConnectErr = servertest.ErrDockerDown },
			wantCode:   exitError,
			wantStderr: "connect to docker (is it running?)",
		},
		{
			name:       "deploy dry run",
			args:       []string{"deploy", "--dry-run", validFile},
			wantCode:   exitOK,
			wantStdout: "==> [5/5] Retire previous containers of orders-api",
		},
		{
			name:       "deploy rejects dry run with step",
			args:       []string{"deploy", "--dry-run", "--step", validFile},
			wantCode:   exitUsage,
			wantStderr: "cannot be combined",
		},
		{
			name:       "deploy reports failure",
			args:       []string{"deploy", validFile},
			setup:      func(h *harness) { h.Engine.PullErr = errors.New("manifest unknown") },
			wantCode:   exitError,
			wantStderr: "liman: Pull image ghcr.io/user/orders-api:1.4: manifest unknown",
		},
		{
			name:       "deploy without a proxy says so",
			args:       []string{"deploy", validFile},
			wantCode:   exitOK,
			wantStdout: "No Nginx is configured on the server",
		},

		{name: "ps with no apps", args: []string{"ps"}, wantCode: exitOK, wantStdout: "No apps."},
		{
			name:       "ps",
			args:       []string{"ps"},
			setup:      func(h *harness) { h.Engine.Add(web) },
			wantCode:   exitOK,
			wantStdout: "web  web-r1     current  web:1.0  running  -       127.0.0.1:40123  Up 2 minutes",
		},
		{name: "ps with arguments", args: []string{"ps", "extra"}, wantCode: exitUsage, wantStderr: "usage: liman ps"},

		{
			name: "logs",
			args: []string{"logs", "web"},
			setup: func(h *harness) {
				h.Engine.Add(web)
				h.Engine.LogOutput = "listening on :3000\n"
			},
			wantCode:   exitOK,
			wantStdout: "listening on :3000",
		},
		{
			name: "logs with follow flag",
			args: []string{"logs", "-f", "web"},
			setup: func(h *harness) {
				h.Engine.Add(web)
				h.Engine.LogOutput = "listening on :3000\n"
			},
			wantCode:   exitOK,
			wantStdout: "listening on :3000",
		},
		{name: "logs of unknown app", args: []string{"logs", "web"}, wantCode: exitError, wantStderr: "web: container not found"},
		{name: "logs without app", args: []string{"logs"}, wantCode: exitUsage, wantStderr: "usage: liman logs [-f] <app>"},
		{name: "logs with unknown flag", args: []string{"logs", "-x", "web"}, wantCode: exitUsage, wantStderr: "usage: liman logs [-f] <app>"},

		{
			name:       "stop",
			args:       []string{"stop", "web"},
			setup:      func(h *harness) { h.Engine.Add(web) },
			wantCode:   exitOK,
			wantStdout: "Stopped and removed web-r1",
		},
		{name: "stop unknown app", args: []string{"stop", "web"}, wantCode: exitError, wantStderr: "web: container not found"},
		{name: "stop without app", args: []string{"stop"}, wantCode: exitUsage, wantStderr: "usage: liman stop <app>"},

		{name: "rollback without app", args: []string{"rollback"}, wantCode: exitUsage, wantStderr: "usage: liman rollback"},
		{name: "rollback with invalid name", args: []string{"rollback", "../x"}, wantCode: exitError, wantStderr: `invalid app name "../x"`},
		{name: "rollback of unknown app", args: []string{"rollback", "web"}, wantCode: exitError, wantStderr: "web: container not found"},
		{
			name:       "rollback without a previous release",
			args:       []string{"rollback", "web"},
			setup:      func(h *harness) { h.Engine.Add(web) },
			wantCode:   exitError,
			wantStderr: "web: no previous release is kept; deploy the version you want instead",
		},
		{
			name:       "rollback rejects dry run with step",
			args:       []string{"rollback", "--dry-run", "--step", "web"},
			wantCode:   exitUsage,
			wantStderr: "cannot be combined",
		},

		{name: "secret without subcommand", args: []string{"secret"}, wantCode: exitUsage, wantStderr: "usage: liman secret"},
		{name: "secret set with value as argument", args: []string{"secret", "set", "web", "KEY", "the-value"}, wantCode: exitUsage, wantStderr: "the value is read from standard input"},
		{name: "secret set with invalid app", args: []string{"secret", "set", "../x", "KEY"}, wantCode: exitError, wantStderr: `invalid app name "../x"`},
		{name: "secret list when empty", args: []string{"secret", "list", "web"}, wantCode: exitOK, wantStdout: "No secrets for web."},
		{name: "secret rm of unknown secret", args: []string{"secret", "rm", "web", "KEY"}, wantCode: exitError, wantStderr: "KEY: secret is not set"},
		{name: "secret set with nothing on stdin", args: []string{"secret", "set", "web", "KEY"}, wantCode: exitError, wantStderr: "the secret value is empty"},

		{name: "history when empty", args: []string{"history"}, wantCode: exitOK, wantStdout: "No history yet."},
		{name: "history with bad limit", args: []string{"history", "-n", "0"}, wantCode: exitUsage, wantStderr: "usage: liman history"},
		{name: "history with too many arguments", args: []string{"history", "a", "b"}, wantCode: exitUsage, wantStderr: "usage: liman history"},

		{name: "status", args: []string{"status"}, wantCode: exitOK, wantStdout: "Token accepted"},
		{
			name:       "status with a rejected token",
			args:       []string{"status"},
			setup:      func(h *harness) { h.cli.API = &api.Client{BaseURL: h.URL, Token: "liman_wrong"} },
			wantCode:   exitError,
			wantStdout: "is up",
			wantStderr: "the token was rejected",
		},
		{
			name:       "status when docker is down",
			args:       []string{"status"},
			setup:      func(h *harness) { h.ConnectErr = servertest.ErrDockerDown },
			wantCode:   exitError,
			wantStderr: "token accepted, but the daemon reports a problem",
		},
		{
			name:       "status when the daemon is unreachable",
			args:       []string{"status"},
			setup:      func(h *harness) { h.cli.API = &api.Client{BaseURL: "http://127.0.0.1:1"} },
			wantCode:   exitError,
			wantStderr: "is limand running?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, "")
			if tt.setup != nil {
				tt.setup(h)
			}

			code := h.cli.Run(context.Background(), tt.args)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(h.stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", h.stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(h.stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", h.stderr.String(), tt.wantStderr)
			}
			// A successful command writes nothing to stderr, and a failing
			// one always explains itself there.
			if tt.wantCode == exitOK && h.stderr.Len() != 0 {
				t.Errorf("unexpected stderr: %q", h.stderr.String())
			}
			if tt.wantCode != exitOK && h.stderr.Len() == 0 {
				t.Error("command failed without writing to stderr")
			}
		})
	}
}

func TestInvalidFileIsRejectedWithoutTheDaemon(t *testing.T) {
	h := newHarness(t, "")
	// An unreachable daemon proves the validation happens locally.
	h.cli.API = &api.Client{BaseURL: "http://127.0.0.1:1"}

	code := h.run("deploy", writeFile(t, invalidDefinition))

	if code != exitError || !strings.Contains(h.stderr.String(), "must include a tag") {
		t.Errorf("code %d, stderr %q; want the local validation error", code, h.stderr.String())
	}
	if strings.Contains(h.stderr.String(), "limand") {
		t.Errorf("the CLI tried to reach the daemon for an invalid file: %q", h.stderr.String())
	}
}

func TestDeployThenHistory(t *testing.T) {
	h := newHarness(t, "")
	file := writeFile(t, validDefinition)

	if code := h.run("deploy", file); code != exitOK {
		t.Fatalf("deploy exit code = %d, stderr: %s", code, h.stderr.String())
	}

	h.stdout.Reset()
	if code := h.run("history", "orders-api"); code != exitOK {
		t.Fatalf("history exit code = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "Pull image ghcr.io/user/orders-api:1.4") || strings.Count(out, "\n") != 6 {
		t.Errorf("history output should have a header and 5 rows:\n%s", out)
	}

	h.stdout.Reset()
	h.run("history", "-n", "2")
	if got := strings.Count(h.stdout.String(), "\n"); got != 3 {
		t.Errorf("history -n 2 printed %d lines, want header and 2 rows", got)
	}

	h.stdout.Reset()
	h.run("history", "another-app")
	if !strings.Contains(h.stdout.String(), "No history yet.") {
		t.Errorf("history of another app = %q, want it to be empty", h.stdout.String())
	}
}

func TestFailedDeployShowsRollbackAndHistory(t *testing.T) {
	h := newHarness(t, "")
	h.Engine.StartState = "exited"
	file := writeFile(t, validDefinition)

	if code := h.run("deploy", file); code != exitError {
		t.Fatalf("deploy exit code = %d, want %d", code, exitError)
	}
	// The rollback happened on the daemon and must be visible in the
	// streamed output; the summary goes to stderr.
	if !strings.Contains(h.stdout.String(), "<== Undo: Start container orders-api-r1") {
		t.Errorf("stdout does not show the rollback:\n%s", h.stdout.String())
	}
	if !strings.Contains(h.stderr.String(), "liman: Health check: container is exited") {
		t.Errorf("stderr = %q, want the reason", h.stderr.String())
	}

	h.stdout.Reset()
	h.run("history")
	for _, want := range []string{"FAILED", "undo"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("history does not contain %q:\n%s", want, h.stdout.String())
		}
	}
}

func TestStepMode(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		// wantCode and the containers left afterwards.
		wantCode       int
		wantContainers int
		wantStderr     string
	}{
		{name: "approve all", stdin: "y\ny\nyes\nY\ny\n", wantCode: exitOK, wantContainers: 1},
		{name: "decline first", stdin: "n\n", wantCode: exitError, wantContainers: 0, wantStderr: "aborted before"},
		{name: "empty answer means no", stdin: "\n", wantCode: exitError, wantContainers: 0, wantStderr: "aborted before"},
		{name: "decline after start undoes it", stdin: "y\ny\ny\nn\n", wantCode: exitError, wantContainers: 0, wantStderr: `aborted before "Health check"`},
		// Input ending is treated as "no" for the pending question.
		{name: "input ends early", stdin: "y\n", wantCode: exitError, wantContainers: 0, wantStderr: "aborted before"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.stdin)

			code := h.run("deploy", "--step", writeFile(t, validDefinition))

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, h.stderr.String())
			}
			if len(h.Engine.Containers) != tt.wantContainers {
				t.Errorf("%d containers left, want %d", len(h.Engine.Containers), tt.wantContainers)
			}
			if !strings.Contains(h.stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", h.stderr.String(), tt.wantStderr)
			}
			if !strings.Contains(h.stdout.String(), "Run this step? [y/N]") {
				t.Errorf("stdout does not contain the prompt:\n%s", h.stdout.String())
			}
		})
	}
}

func TestDeployWithProxy(t *testing.T) {
	h := newHarness(t, "")
	proxy := h.WithProxy(t, "true")

	if code := h.run("deploy", writeFile(t, validDefinition)); code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr.String())
	}

	conf, err := os.ReadFile(proxy.Path("orders-api"))
	if err != nil || !strings.Contains(string(conf), "server_name api.example.com;") {
		t.Errorf("config = %q, %v; want it to route the domain", conf, err)
	}
	for _, want := range []string{"==> [6/7] Reload Nginx", "Nginx routes api.example.com to it"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("stdout does not contain %q:\n%s", want, h.stdout.String())
		}
	}
}

func TestStopWithProxyReportsRemovedConfig(t *testing.T) {
	h := newHarness(t, "")
	proxy := h.WithProxy(t, "true")
	h.Engine.Add(engine.Container{Name: "web-r1", App: "web"})
	if err := proxy.Write("web", []byte("server {}\n")); err != nil {
		t.Fatal(err)
	}

	if code := h.run("stop", "web"); code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr.String())
	}

	want := "Removed Nginx config " + proxy.Path("web") + "\nStopped and removed web-r1\n"
	if h.stdout.String() != want {
		t.Errorf("stdout = %q, want %q", h.stdout.String(), want)
	}
}

func TestInterruptedCommandExplainsItself(t *testing.T) {
	h := newHarness(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // as if Ctrl+C had already been pressed

	code := h.cli.Run(ctx, []string{"ps"})

	if code != exitError || !strings.Contains(h.stderr.String(), "interrupted") {
		t.Errorf("code %d, stderr %q; want an interruption message", code, h.stderr.String())
	}
}

// writeFile creates an app.yaml in a temporary directory that the testing
// package removes when the test finishes.
func writeFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestPsShowsHealth(t *testing.T) {
	h := newHarness(t, "")
	h.Engine.Add(engine.Container{Name: "web-r1", App: "web", Image: "web:1.0", Status: "Up 2 minutes", HostPort: 40123})
	h.Engine.Add(engine.Container{Name: "api-r3", App: "api", Image: "api:2.0", Status: "Up 5 seconds", HostPort: 40124})

	// Stand in for the health monitor.
	h.Server.HealthOf = func(app string) (string, string, int, bool) {
		switch app {
		case "web":
			return "healthy", "", 0, true
		case "api":
			return "restarting", "health check failed: unexpected status 503; restarted, next check in 20s", 2, true
		}
		return "", "", 0, false
	}

	if code := h.run("ps"); code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr.String())
	}

	out := h.stdout.String()
	for _, want := range []string{
		"HEALTH",
		"healthy ",
		"restarting (2 restarts)",
		// The explanation follows the table.
		"\napi: health check failed: unexpected status 503; restarted, next check in 20s\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	// A healthy app gets no note.
	if strings.Contains(out, "\nweb: ") {
		t.Errorf("a healthy app has a note:\n%s", out)
	}
}

func TestDeployTwiceThenRollback(t *testing.T) {
	h := newHarness(t, "")
	file := writeFile(t, validDefinition)

	for range 2 {
		if code := h.run("deploy", file); code != exitOK {
			t.Fatalf("deploy exit code = %d, stderr: %s", code, h.stderr.String())
		}
	}

	// Both releases are listed, with their roles.
	h.stdout.Reset()
	h.run("ps")
	ps := h.stdout.String()
	for _, want := range []string{"orders-api-r1  previous", "orders-api-r2  current"} {
		if !strings.Contains(ps, want) {
			t.Errorf("ps does not contain %q:\n%s", want, ps)
		}
	}

	// A dry run names the containers and changes nothing.
	h.stdout.Reset()
	if code := h.run("rollback", "--dry-run", "orders-api"); code != exitOK {
		t.Fatalf("rollback --dry-run exit code = %d, stderr: %s", code, h.stderr.String())
	}
	for _, want := range []string{"Dry run of a rollback of orders-api", "$ docker start orders-api-r1", "$ docker rm -f orders-api-r2"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("dry run does not contain %q:\n%s", want, h.stdout.String())
		}
	}
	if got := h.Engine.Running(); len(got) != 1 || got[0] != "orders-api-r2" {
		t.Fatalf("running after the dry run = %v, want only orders-api-r2", got)
	}

	h.stdout.Reset()
	if code := h.run("rollback", "orders-api"); code != exitOK {
		t.Fatalf("rollback exit code = %d, stderr: %s", code, h.stderr.String())
	}
	for _, want := range []string{
		"==> [1/3] Start previous container orders-api-r1",
		"==> [3/3] Remove container orders-api-r2",
		"orders-api is back on release r1",
	} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("rollback output does not contain %q:\n%s", want, h.stdout.String())
		}
	}
	if got := h.Engine.Running(); len(got) != 1 || got[0] != "orders-api-r1" {
		t.Errorf("running after the rollback = %v, want only orders-api-r1", got)
	}

	// The rollback is in the history, under the release it restored.
	h.stdout.Reset()
	h.run("history", "-n", "3", "orders-api")
	if !strings.Contains(h.stdout.String(), "Start previous container orders-api-r1") {
		t.Errorf("history does not show the rollback:\n%s", h.stdout.String())
	}

	// There is nothing further back.
	if code := h.run("rollback", "orders-api"); code != exitError || !strings.Contains(h.stderr.String(), "no previous release is kept") {
		t.Errorf("second rollback: code %d, stderr %q", code, h.stderr.String())
	}
}

func TestRollbackStepModeDeclineKeepsCurrentRelease(t *testing.T) {
	// Approve starting the old release and its health check, decline the
	// removal of the current one.
	h := newHarness(t, "y\ny\nn\n")
	file := writeFile(t, validDefinition)
	for range 2 {
		h.run("deploy", file)
	}

	code := h.run("rollback", "--step", "orders-api")

	if code != exitError || !strings.Contains(h.stderr.String(), `aborted before "Remove container orders-api-r2"`) {
		t.Fatalf("code %d, stderr %q; want an abort before the removal", code, h.stderr.String())
	}
	if got := h.Engine.Running(); len(got) != 1 || got[0] != "orders-api-r2" {
		t.Errorf("running = %v, want the current release to keep serving", got)
	}
	if got := h.Engine.State("orders-api-r1"); got != "exited" {
		t.Errorf("state of the previous release = %q, want it stopped again and kept", got)
	}
}

func TestSecretCommands(t *testing.T) {
	// The value arrives on standard input with the trailing newline that
	// "echo" and editors add.
	h := newHarness(t, "postgres://user:hunter2@db/app\n")

	if code := h.run("secret", "set", "orders-api", "DATABASE_URL"); code != exitOK {
		t.Fatalf("secret set exit code = %d, stderr: %s", code, h.stderr.String())
	}
	// The confirmation states the length, never the value.
	if want := "Stored secret DATABASE_URL of orders-api (30 bytes)"; !strings.Contains(h.stdout.String(), want) {
		t.Errorf("output = %q, want it to contain %q", h.stdout.String(), want)
	}
	if strings.Contains(h.stdout.String()+h.stderr.String(), "hunter2") {
		t.Error("the CLI printed the secret value")
	}

	// Exactly the value, without the newline, reached the daemon.
	env := deployAndGetEnv(t, h, "DATABASE_URL")
	if got := env["DATABASE_URL"]; got != "postgres://user:hunter2@db/app" {
		t.Errorf("the container received a value of %d bytes, want the 30-byte original", len(got))
	}

	h.stdout.Reset()
	if code := h.run("secret", "list", "orders-api"); code != exitOK {
		t.Fatalf("secret list exit code = %d", code)
	}
	if out := h.stdout.String(); !strings.Contains(out, "DATABASE_URL") || strings.Contains(out, "hunter2") {
		t.Errorf("secret list output:\n%s", out)
	}

	h.stdout.Reset()
	if code := h.run("secret", "rm", "orders-api", "DATABASE_URL"); code != exitOK {
		t.Fatalf("secret rm exit code = %d, stderr: %s", code, h.stderr.String())
	}
	h.stdout.Reset()
	h.run("secret", "list", "orders-api")
	if !strings.Contains(h.stdout.String(), "No secrets for orders-api.") {
		t.Errorf("secret list after rm:\n%s", h.stdout.String())
	}
}

// deployAndGetEnv deploys an app that refers to the named secrets and
// returns the environment its container was started with.
func deployAndGetEnv(t *testing.T, h *harness, secretNames ...string) map[string]string {
	t.Helper()

	def := validDefinition + "secrets:\n"
	for _, name := range secretNames {
		def += "  - " + name + "\n"
	}
	if code := h.run("deploy", writeFile(t, def)); code != exitOK {
		t.Fatalf("deploy exit code = %d, stderr: %s", code, h.stderr.String())
	}
	specs := h.Engine.Specs
	return specs[len(specs)-1].Env
}

func TestSecretValueHandlingOfNewlines(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		want  string
	}{
		{"no newline", "value", "value"},
		{"one trailing newline is removed", "value\n", "value"},
		{"windows line ending is removed", "value\r\n", "value"},
		{"only one newline is removed", "value\n\n", "value\n"},
		{"inner newlines are kept", "line1\nline2\n", "line1\nline2"},
		{"leading and inner spaces are kept", "  two  words \n", "  two  words "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.stdin)

			if code := h.run("secret", "set", "orders-api", "KEY"); code != exitOK {
				t.Fatalf("exit code = %d, stderr: %s", code, h.stderr.String())
			}
			if got := deployAndGetEnv(t, h, "KEY")["KEY"]; got != tt.want {
				t.Errorf("stored value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeployWithMissingSecretSaysHowToFixIt(t *testing.T) {
	h := newHarness(t, "")
	file := writeFile(t, validDefinition+"secrets:\n  - API_KEY\n")

	code := h.run("deploy", file)

	if code != exitError {
		t.Fatalf("exit code = %d, want %d", code, exitError)
	}
	if want := "liman secret set orders-api <NAME>"; !strings.Contains(h.stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", h.stderr.String(), want)
	}
}

func TestStopSaysTheSecretsAreKept(t *testing.T) {
	h := newHarness(t, "")
	h.Engine.Add(engine.Container{Name: "web-r1", App: "web"})
	for _, key := range []string{"API_KEY", "DATABASE_URL"} {
		if err := h.Client.SetSecret(context.Background(), "web", key, []byte("value")); err != nil {
			t.Fatal(err)
		}
	}

	if code := h.run("stop", "web"); code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr.String())
	}
	want := "Kept the secrets of web (API_KEY, DATABASE_URL); deploying it again uses them. Remove one with: liman secret rm web <name>\n"
	if !strings.HasSuffix(h.stdout.String(), want) {
		t.Errorf("stdout = %q, want it to end with %q", h.stdout.String(), want)
	}

	// They really are still there.
	if code := h.run("secret", "list", "web"); code != exitOK || !strings.Contains(h.stdout.String(), "DATABASE_URL") {
		t.Errorf("secret list after stop: code %d, stdout %q", code, h.stdout.String())
	}
}

func TestStatusNotesAVersionDifference(t *testing.T) {
	// A daemon that only answers what status asks.
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok","version":"v1.3.0"}`))
		case "/v1/apps":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()

	for _, tt := range []struct {
		mine string
		note bool
	}{
		{"v1.4.0", true},
		{"v1.3.0", false},
		{"dev", false}, // a build from source says nothing about versions
	} {
		var out, errOut bytes.Buffer
		c := &CLI{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, API: &api.Client{BaseURL: daemon.URL, Token: "liman_x"}, Version: tt.mine}
		if code := c.Run(context.Background(), []string{"status"}); code != exitOK {
			t.Fatalf("status as %s: code %d, stderr %q", tt.mine, code, errOut.String())
		}
		got := strings.Contains(out.String(), "Note: this liman is "+tt.mine+" and the daemon is v1.3.0")
		if got != tt.note {
			t.Errorf("status as %s: note shown %v, want %v\n%s", tt.mine, got, tt.note, out.String())
		}
	}
}

func TestTrigger(t *testing.T) {
	h := newHarness(t, "")
	if code := h.run("deploy", writeFile(t, validDefinition)); code != exitOK {
		t.Fatal(h.stderr.String())
	}
	d := "sha256:" + strings.Repeat("0a", 32)
	h.Engine.Registry = map[string]bool{"ghcr.io/user/orders-api@" + d: true}
	_, key := h.DeployKey(t, "ci", "orders-api")
	h.cli.API = key
	h.stdout.Reset()

	if code := h.run("trigger", "--app", "orders-api", "--digest", d); code != exitOK {
		t.Fatalf("trigger: code %d, stderr %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "orders-api is running release r2, image "+d) {
		t.Errorf("stdout = %s", h.stdout.String())
	}

	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"trigger", "--app", "orders-api"}, exitUsage, "usage: liman trigger"},
		{[]string{"trigger", "--app", "orders-api", "--digest", "latest"}, exitError, "invalid digest"},
		{[]string{"trigger", "--app", "orders-api", "--digest", "sha256:" + strings.Repeat("ff", 32)}, exitError, "not in the registry"},
	} {
		h.stderr.Reset()
		if code := h.run(tt.args...); code != tt.code || !strings.Contains(h.stderr.String(), tt.want) {
			t.Errorf("%v: code %d, stderr %q; want %d and %q", tt.args, code, h.stderr.String(), tt.code, tt.want)
		}
	}
}
