package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/engine/enginetest"
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

func TestRun(t *testing.T) {
	validFile := writeFile(t, validDefinition)
	invalidFile := writeFile(t, invalidDefinition)

	tests := []struct {
		name string
		args []string
		// setup prepares the fake engine; nil leaves it empty.
		setup      func(f *enginetest.Fake)
		connectErr error
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"version"}, wantCode: exitOK, wantStdout: "liman dev"},
		{name: "help", args: []string{"help"}, wantCode: exitOK, wantStdout: "Usage:"},
		{name: "help flag", args: []string{"--help"}, wantCode: exitOK, wantStdout: "Usage:"},
		{name: "no arguments", args: nil, wantCode: exitUsage, wantStderr: "Usage:"},
		{name: "unknown command", args: []string{"nope"}, wantCode: exitUsage, wantStderr: `unknown command "nope"`},

		{name: "plan", args: []string{"plan", validFile}, wantCode: exitOK, wantStdout: "1. Pull image ghcr.io/user/orders-api:1.4"},
		{name: "plan without file", args: []string{"plan"}, wantCode: exitUsage, wantStderr: "usage: liman plan <file>"},
		{name: "plan with missing file", args: []string{"plan", "missing.yaml"}, wantCode: exitError, wantStderr: "no such file"},
		{name: "plan with invalid file", args: []string{"plan", invalidFile}, wantCode: exitError, wantStderr: "must include a tag"},
		{
			name:       "plan works without docker",
			args:       []string{"plan", validFile},
			connectErr: errors.New("docker is down"),
			wantCode:   exitOK,
			wantStdout: "Plan for orders-api",
		},

		{name: "deploy", args: []string{"deploy", validFile}, wantCode: exitOK, wantStdout: "orders-api is running at http://127.0.0.1:"},
		{name: "deploy without file", args: []string{"deploy"}, wantCode: exitUsage, wantStderr: "usage: liman deploy <file>"},
		{name: "deploy with invalid file", args: []string{"deploy", invalidFile}, wantCode: exitError, wantStderr: "must include a tag"},
		{
			name:       "deploy without docker",
			args:       []string{"deploy", validFile},
			connectErr: errors.New("docker is down"),
			wantCode:   exitError,
			wantStderr: "docker is down",
		},

		{name: "ps with no apps", args: []string{"ps"}, wantCode: exitOK, wantStdout: "No apps."},
		{
			name: "ps",
			args: []string{"ps"},
			setup: func(f *enginetest.Fake) {
				f.Containers["web"] = engine.Container{Name: "web", Image: "web:1.0", State: "running", Status: "Up 2 minutes", HostPort: 40123}
			},
			wantCode:   exitOK,
			wantStdout: "web   web:1.0  running  127.0.0.1:40123  Up 2 minutes",
		},
		{name: "ps with arguments", args: []string{"ps", "extra"}, wantCode: exitUsage, wantStderr: "usage: liman ps"},

		{
			name: "logs",
			args: []string{"logs", "web"},
			setup: func(f *enginetest.Fake) {
				f.Containers["web"] = engine.Container{Name: "web"}
				f.LogOutput = "listening on :3000\n"
			},
			wantCode:   exitOK,
			wantStdout: "listening on :3000",
		},
		{
			name: "logs with follow flag",
			args: []string{"logs", "-f", "web"},
			setup: func(f *enginetest.Fake) {
				f.Containers["web"] = engine.Container{Name: "web"}
				f.LogOutput = "listening on :3000\n"
			},
			wantCode:   exitOK,
			wantStdout: "listening on :3000",
		},
		{name: "logs of unknown app", args: []string{"logs", "web"}, wantCode: exitError, wantStderr: "web: container not found"},
		{name: "logs without app", args: []string{"logs"}, wantCode: exitUsage, wantStderr: "usage: liman logs [-f] <app>"},
		{name: "logs with unknown flag", args: []string{"logs", "-x", "web"}, wantCode: exitUsage, wantStderr: "usage: liman logs [-f] <app>"},

		{
			name: "stop",
			args: []string{"stop", "web"},
			setup: func(f *enginetest.Fake) {
				f.Containers["web"] = engine.Container{Name: "web", State: "running"}
			},
			wantCode:   exitOK,
			wantStdout: "Stopped and removed web",
		},
		{name: "stop unknown app", args: []string{"stop", "web"}, wantCode: exitError, wantStderr: "web: container not found"},
		{name: "stop without app", args: []string{"stop"}, wantCode: exitUsage, wantStderr: "usage: liman stop <app>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := enginetest.New()
			if tt.setup != nil {
				tt.setup(fake)
			}

			var stdout, stderr bytes.Buffer
			c := &CLI{
				Stdout: &stdout,
				Stderr: &stderr,
				Connect: func(context.Context) (engine.Engine, error) {
					if tt.connectErr != nil {
						return nil, tt.connectErr
					}
					return fake, nil
				},
			}

			code := c.Run(context.Background(), tt.args)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
			// Errors must not leak into stdout, and a failing command must
			// say something on stderr.
			if tt.wantStderr == "" && stderr.Len() != 0 {
				t.Errorf("unexpected stderr: %q", stderr.String())
			}
			if tt.wantCode != exitOK && stderr.Len() == 0 {
				t.Error("command failed without writing to stderr")
			}
		})
	}
}

func TestStopRemovesContainer(t *testing.T) {
	fake := enginetest.New()
	fake.Containers["web"] = engine.Container{Name: "web", State: "running"}

	var out bytes.Buffer
	c := &CLI{
		Stdout:  &out,
		Stderr:  &out,
		Connect: func(context.Context) (engine.Engine, error) { return fake, nil },
	}

	if code := c.Run(context.Background(), []string{"stop", "web"}); code != exitOK {
		t.Fatalf("exit code = %d, output: %s", code, out.String())
	}
	if _, exists := fake.Containers["web"]; exists {
		t.Error("container still exists after stop")
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
