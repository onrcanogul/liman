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

	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/engine/enginetest"
	"github.com/onrcanogul/liman/internal/history"
	"github.com/onrcanogul/liman/internal/nginx"
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

// harness bundles a CLI with the fakes behind it.
type harness struct {
	cli    *CLI
	fake   *enginetest.Fake
	stdout bytes.Buffer
	stderr bytes.Buffer
	// connectErr makes Connect fail, simulating Docker being down.
	connectErr error
}

func newHarness(t *testing.T, stdin string) *harness {
	t.Helper()

	h := &harness{fake: enginetest.New()}
	h.cli = &CLI{
		Stdin:  strings.NewReader(stdin),
		Stdout: &h.stdout,
		Stderr: &h.stderr,
		Connect: func(context.Context) (engine.Engine, error) {
			if h.connectErr != nil {
				return nil, h.connectErr
			}
			return h.fake, nil
		},
		History: history.Store{Path: filepath.Join(t.TempDir(), "history.jsonl")},
	}
	return h
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
			setup:      func(h *harness) { h.connectErr = errors.New("docker is down") },
			wantCode:   exitOK,
			wantStdout: "Dry run for orders-api: nothing will be changed.",
		},

		{name: "deploy", args: []string{"deploy", validFile}, wantCode: exitOK, wantStdout: "orders-api is running at http://127.0.0.1:"},
		{name: "deploy without file", args: []string{"deploy"}, wantCode: exitUsage, wantStderr: "usage: liman deploy"},
		{name: "deploy with unknown flag", args: []string{"deploy", "--force", validFile}, wantCode: exitUsage, wantStderr: "usage: liman deploy"},
		{name: "deploy with invalid file", args: []string{"deploy", invalidFile}, wantCode: exitError, wantStderr: "must include a tag"},
		{
			name:       "deploy without docker",
			args:       []string{"deploy", validFile},
			setup:      func(h *harness) { h.connectErr = errors.New("docker is down") },
			wantCode:   exitError,
			wantStderr: "docker is down",
		},
		{
			name:       "deploy dry run works without docker",
			args:       []string{"deploy", "--dry-run", validFile},
			setup:      func(h *harness) { h.connectErr = errors.New("docker is down") },
			wantCode:   exitOK,
			wantStdout: "==> [5/5] Remove previous containers of orders-api",
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
			setup:      func(h *harness) { h.fake.PullErr = errors.New("manifest unknown") },
			wantCode:   exitError,
			wantStderr: "liman: Pull image ghcr.io/user/orders-api:1.4: manifest unknown",
		},

		{name: "ps with no apps", args: []string{"ps"}, wantCode: exitOK, wantStdout: "No apps."},
		{
			name:       "ps",
			args:       []string{"ps"},
			setup:      func(h *harness) { h.fake.Add(web) },
			wantCode:   exitOK,
			wantStdout: "web  web-r1     web:1.0  running  127.0.0.1:40123  Up 2 minutes",
		},
		{name: "ps with arguments", args: []string{"ps", "extra"}, wantCode: exitUsage, wantStderr: "usage: liman ps"},

		{
			name: "logs",
			args: []string{"logs", "web"},
			setup: func(h *harness) {
				h.fake.Add(web)
				h.fake.LogOutput = "listening on :3000\n"
			},
			wantCode:   exitOK,
			wantStdout: "listening on :3000",
		},
		{
			name: "logs with follow flag",
			args: []string{"logs", "-f", "web"},
			setup: func(h *harness) {
				h.fake.Add(web)
				h.fake.LogOutput = "listening on :3000\n"
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
			setup:      func(h *harness) { h.fake.Add(web) },
			wantCode:   exitOK,
			wantStdout: "Stopped and removed web-r1",
		},
		{name: "stop unknown app", args: []string{"stop", "web"}, wantCode: exitError, wantStderr: "web: container not found"},
		{name: "stop without app", args: []string{"stop"}, wantCode: exitUsage, wantStderr: "usage: liman stop <app>"},

		{name: "history when empty", args: []string{"history"}, wantCode: exitOK, wantStdout: "No history yet."},
		{name: "history with bad limit", args: []string{"history", "-n", "0"}, wantCode: exitUsage, wantStderr: "usage: liman history"},
		{name: "history with too many arguments", args: []string{"history", "a", "b"}, wantCode: exitUsage, wantStderr: "usage: liman history"},
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
			// Errors must not leak into stdout, and a failing command must
			// say something on stderr.
			if tt.wantStderr == "" && h.stderr.Len() != 0 {
				t.Errorf("unexpected stderr: %q", h.stderr.String())
			}
			if tt.wantCode != exitOK && h.stderr.Len() == 0 {
				t.Error("command failed without writing to stderr")
			}
		})
	}
}

func TestLogsShowsNewestContainer(t *testing.T) {
	h := newHarness(t, "")
	h.fake.Add(engine.Container{Name: "web-r1", App: "web"})
	h.fake.Add(engine.Container{Name: "web-r2", App: "web"})
	h.fake.Add(engine.Container{Name: "other-r9", App: "other"})

	if code := h.cli.Run(context.Background(), []string{"logs", "web"}); code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr.String())
	}

	if want := []string{"logs web-r2"}; !equal(h.fake.Calls, want) {
		t.Errorf("calls = %v, want %v", h.fake.Calls, want)
	}
}

func TestStopRemovesAllContainersOfApp(t *testing.T) {
	h := newHarness(t, "")
	h.fake.Add(engine.Container{Name: "web-r1", App: "web"})
	h.fake.Add(engine.Container{Name: "web-r2", App: "web"})
	h.fake.Add(engine.Container{Name: "other-r9", App: "other"})

	if code := h.cli.Run(context.Background(), []string{"stop", "web"}); code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr.String())
	}

	if _, ok := h.fake.Containers["other-r9"]; !ok || len(h.fake.Containers) != 1 {
		t.Errorf("containers = %v, want only other-r9", h.fake.Containers)
	}
}

func TestDeployRecordsHistory(t *testing.T) {
	h := newHarness(t, "")
	file := writeFile(t, validDefinition)

	if code := h.cli.Run(context.Background(), []string{"deploy", file}); code != exitOK {
		t.Fatalf("deploy exit code = %d, stderr: %s", code, h.stderr.String())
	}

	events, err := h.cli.History.Read()
	if err != nil {
		t.Fatalf("History.Read() error = %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("history has %d events, want 5", len(events))
	}
	first := events[0]
	if first.App != "orders-api" || first.Action != "run" || !first.OK || first.Release == "" {
		t.Errorf("first event = %+v, want a successful run for orders-api with a release", first)
	}

	// The same events must be visible through the history command.
	h.stdout.Reset()
	if code := h.cli.Run(context.Background(), []string{"history", "orders-api"}); code != exitOK {
		t.Fatalf("history exit code = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "Pull image ghcr.io/user/orders-api:1.4") || strings.Count(out, "\n") != 6 {
		t.Errorf("history output should have a header and 5 rows:\n%s", out)
	}

	h.stdout.Reset()
	h.cli.Run(context.Background(), []string{"history", "-n", "2"})
	if got := strings.Count(h.stdout.String(), "\n"); got != 3 {
		t.Errorf("history -n 2 printed %d lines, want header and 2 rows", got)
	}

	h.stdout.Reset()
	h.cli.Run(context.Background(), []string{"history", "another-app"})
	if !strings.Contains(h.stdout.String(), "No history yet.") {
		t.Errorf("history of another app = %q, want it to be empty", h.stdout.String())
	}
}

func TestFailedDeployRecordsFailureAndUndo(t *testing.T) {
	h := newHarness(t, "")
	h.fake.StartState = "exited"
	file := writeFile(t, validDefinition)

	if code := h.cli.Run(context.Background(), []string{"deploy", file}); code != exitError {
		t.Fatalf("deploy exit code = %d, want %d", code, exitError)
	}

	events, err := h.cli.History.Read()
	if err != nil {
		t.Fatalf("History.Read() error = %v", err)
	}

	var summary []string
	for _, ev := range events {
		result := "ok"
		if !ev.OK {
			result = "failed"
		}
		summary = append(summary, ev.Action+" "+result)
	}
	// pull, port, start succeed; health fails; start is undone.
	want := []string{"run ok", "run ok", "run ok", "run failed", "undo ok"}
	if !equal(summary, want) {
		t.Errorf("history = %v, want %v", summary, want)
	}
	if last := events[3]; last.Error == "" {
		t.Error("the failed event has no error message")
	}
}

func TestDryRunRecordsNoHistory(t *testing.T) {
	h := newHarness(t, "")
	file := writeFile(t, validDefinition)

	h.cli.Run(context.Background(), []string{"deploy", "--dry-run", file})

	events, err := h.cli.History.Read()
	if err != nil || len(events) != 0 {
		t.Errorf("history after dry run = %v, %v; want empty", events, err)
	}
	if len(h.fake.Calls) != 0 {
		t.Errorf("engine calls during dry run = %v, want none", h.fake.Calls)
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
		{name: "input ends early", stdin: "y\n", wantCode: exitError, wantContainers: 0, wantStderr: "read answer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.stdin)
			file := writeFile(t, validDefinition)

			code := h.cli.Run(context.Background(), []string{"deploy", "--step", file})

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, h.stderr.String())
			}
			if len(h.fake.Containers) != tt.wantContainers {
				t.Errorf("%d containers left, want %d", len(h.fake.Containers), tt.wantContainers)
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

func TestHistoryWriteFailureWarnsOnce(t *testing.T) {
	h := newHarness(t, "")
	// A path below a regular file cannot be created.
	blocker := writeFile(t, "not a directory")
	h.cli.History = history.Store{Path: filepath.Join(blocker, "history.jsonl")}
	file := writeFile(t, validDefinition)

	code := h.cli.Run(context.Background(), []string{"deploy", file})

	if code != exitOK {
		t.Errorf("exit code = %d, want the deploy to succeed despite the history failure", code)
	}
	if got := strings.Count(h.stderr.String(), "warning: history is not being recorded"); got != 1 {
		t.Errorf("warning printed %d times, want once:\n%s", got, h.stderr.String())
	}
}

// withProxy gives the harness a real nginx.Manager that writes into a
// temporary directory. Nginx itself is replaced by two stand-ins:
//
//   - the test and reload commands are the standard Unix programs true and
//     false, which do nothing and exit with success or failure;
//   - a local HTTP server answers the release probe by looking at the files
//     in the config directory, as an Nginx that reloads instantly would.
func (h *harness) withProxy(t *testing.T, reloadCommand string) *nginx.Manager {
	t.Helper()

	confDir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		files, _ := filepath.Glob(filepath.Join(confDir, "*.conf"))
		for _, file := range files {
			content, err := os.ReadFile(file)
			if err == nil && strings.Contains(string(content), "server_name "+r.Host+";") {
				w.Header().Set(nginx.ReleaseHeader, nginx.ReleaseOf(content))
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	m := nginx.New(config.Nginx{
		ConfDir:       confDir,
		UpstreamHost:  "127.0.0.1",
		ProbeAddr:     strings.TrimPrefix(srv.URL, "http://"),
		TestCommand:   []string{"true"},
		ReloadCommand: []string{reloadCommand},
	})
	h.cli.Proxy = m
	return m
}

func TestDeployWithProxyWritesConfig(t *testing.T) {
	h := newHarness(t, "")
	proxy := h.withProxy(t, "true")
	file := writeFile(t, validDefinition)

	if code := h.cli.Run(context.Background(), []string{"deploy", file}); code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr.String())
	}

	conf, err := os.ReadFile(proxy.Path("orders-api"))
	if err != nil {
		t.Fatalf("config file was not written: %v", err)
	}
	if !strings.Contains(string(conf), "server_name api.example.com;") {
		t.Errorf("config does not route the domain:\n%s", conf)
	}
	for _, want := range []string{"==> [6/7] Reload Nginx", "Nginx routes api.example.com to it"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("stdout does not contain %q:\n%s", want, h.stdout.String())
		}
	}
}

func TestDeployWithProxyReloadFailureLeavesNothingBehind(t *testing.T) {
	h := newHarness(t, "")
	proxy := h.withProxy(t, "false")
	file := writeFile(t, validDefinition)

	if code := h.cli.Run(context.Background(), []string{"deploy", file}); code != exitError {
		t.Fatalf("exit code = %d, want %d", code, exitError)
	}

	if _, err := os.Stat(proxy.Path("orders-api")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config file still exists after a failed first deploy (stat error: %v)", err)
	}
	if len(h.fake.Containers) != 0 {
		t.Errorf("%d containers left, want none", len(h.fake.Containers))
	}
	if !strings.Contains(h.stderr.String(), "nginx reload failed") {
		t.Errorf("stderr = %q, want it to explain the reload failure", h.stderr.String())
	}
}

func TestDeployWithoutProxySaysSo(t *testing.T) {
	h := newHarness(t, "")
	file := writeFile(t, validDefinition)

	h.cli.Run(context.Background(), []string{"deploy", file})

	if !strings.Contains(h.stdout.String(), "No Nginx is configured") {
		t.Errorf("stdout does not mention the missing proxy:\n%s", h.stdout.String())
	}
}

func TestStopRemovesProxyConfig(t *testing.T) {
	h := newHarness(t, "")
	proxy := h.withProxy(t, "true")
	h.fake.Add(engine.Container{Name: "web-r1", App: "web"})
	if err := proxy.Write("web", []byte("server {}\n")); err != nil {
		t.Fatal(err)
	}

	if code := h.cli.Run(context.Background(), []string{"stop", "web"}); code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr.String())
	}

	if _, err := os.Stat(proxy.Path("web")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config file still exists after stop (stat error: %v)", err)
	}
	if len(h.fake.Containers) != 0 {
		t.Error("container still exists after stop")
	}
}

func TestStopKeepsEverythingWhenReloadFails(t *testing.T) {
	h := newHarness(t, "")
	proxy := h.withProxy(t, "false")
	h.fake.Add(engine.Container{Name: "web-r1", App: "web"})
	if err := proxy.Write("web", []byte("server {}\n")); err != nil {
		t.Fatal(err)
	}

	if code := h.cli.Run(context.Background(), []string{"stop", "web"}); code != exitError {
		t.Fatalf("exit code = %d, want %d", code, exitError)
	}

	// Nginx is still serving the old config, so the file must be back and
	// the container it points at must still be running.
	if conf, _ := os.ReadFile(proxy.Path("web")); string(conf) != "server {}\n" {
		t.Errorf("config after failed stop = %q, want the original restored", conf)
	}
	if len(h.fake.Containers) != 1 {
		t.Error("container was removed although the proxy still routes to it")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
