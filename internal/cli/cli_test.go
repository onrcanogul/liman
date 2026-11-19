package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	validFile := writeFile(t, `
name: orders-api
image: ghcr.io/user/orders-api:1.4
domain: api.example.com
port: 3000
`)
	invalidFile := writeFile(t, `
name: orders-api
image: ghcr.io/user/orders-api
domain: api.example.com
port: 3000
`)

	tests := []struct {
		name       string
		args       []string
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := Run(tt.args, &stdout, &stderr)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
			// A command writes to exactly one stream: results to stdout,
			// errors to stderr.
			if tt.wantStdout == "" && stdout.Len() != 0 {
				t.Errorf("unexpected stdout: %q", stdout.String())
			}
			if tt.wantStderr == "" && stderr.Len() != 0 {
				t.Errorf("unexpected stderr: %q", stderr.String())
			}
		})
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
