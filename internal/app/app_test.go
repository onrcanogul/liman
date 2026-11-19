package app

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// validApp returns a definition that passes validation. Tests copy it and
// break one field at a time.
func validApp() App {
	return App{
		Name:   "orders-api",
		Image:  "ghcr.io/user/orders-api:1.4",
		Domain: "api.example.com",
		Port:   3000,
	}
}

func TestLoadValidFile(t *testing.T) {
	// Files under a directory named testdata are ignored by the Go tool
	// when building, which makes it the conventional place for fixtures.
	a, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if a.Name != "orders-api" {
		t.Errorf("Name = %q, want %q", a.Name, "orders-api")
	}
	if a.Port != 3000 {
		t.Errorf("Port = %d, want 3000", a.Port)
	}
	if got := a.Env["NODE_ENV"]; got != "production" {
		t.Errorf(`Env["NODE_ENV"] = %q, want "production"`, got)
	}
	if a.Resources.CPUs != 0.5 {
		t.Errorf("CPUs = %g, want 0.5", a.Resources.CPUs)
	}
	if a.Healthcheck == nil || a.Healthcheck.Path != "/health" {
		t.Errorf("Healthcheck = %+v, want path /health", a.Healthcheck)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("testdata/does-not-exist.yaml")

	// Load wraps the OS error with %w, so the original cause is still
	// reachable through the chain.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load() error = %v, want it to wrap fs.ErrNotExist", err)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	yaml := `
name: orders-api
imge: ghcr.io/user/orders-api:1.4
domain: api.example.com
port: 3000
`
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "imge") {
		t.Errorf("Parse() error = %v, want it to mention the unknown field imge", err)
	}
}

func TestParseReportsAllProblems(t *testing.T) {
	yaml := `
name: Orders_API
image: ghcr.io/user/orders-api
domain: api.example.com
port: 70000
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("Parse() error = nil, want validation errors")
	}
	for _, want := range []string{"name:", "image:", "port:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		// mutate changes a valid definition; nil leaves it untouched.
		mutate  func(a *App)
		wantErr string // empty means the definition must be valid
	}{
		{name: "minimal valid definition"},
		{
			name:   "image with digest",
			mutate: func(a *App) { a.Image = "ghcr.io/user/api@sha256:abc123" },
		},
		{
			name:   "image from registry with port",
			mutate: func(a *App) { a.Image = "localhost:5000/api:1.0" },
		},
		{
			name:   "all optional fields",
			mutate: func(a *App) { a.Resources = Resources{Memory: "1g", CPUs: 2}; a.Healthcheck = &Healthcheck{Path: "/"} },
		},
		{name: "empty name", mutate: func(a *App) { a.Name = "" }, wantErr: "name:"},
		{name: "uppercase name", mutate: func(a *App) { a.Name = "Orders" }, wantErr: "name:"},
		{name: "name ending with hyphen", mutate: func(a *App) { a.Name = "orders-" }, wantErr: "name:"},
		{name: "empty image", mutate: func(a *App) { a.Image = "" }, wantErr: "image: is required"},
		{name: "image without tag", mutate: func(a *App) { a.Image = "ghcr.io/user/api" }, wantErr: "must include a tag"},
		{name: "registry port is not a tag", mutate: func(a *App) { a.Image = "localhost:5000/api" }, wantErr: "must include a tag"},
		{name: "latest tag", mutate: func(a *App) { a.Image = "ghcr.io/user/api:latest" }, wantErr: "fixed tag"},
		{name: "image with whitespace", mutate: func(a *App) { a.Image = "ghcr.io/user/api:1.0 --privileged" }, wantErr: "not allowed in an image reference"},
		{name: "image with shell syntax", mutate: func(a *App) { a.Image = "api:1.0;rm" }, wantErr: "not allowed in an image reference"},
		{name: "image with terminal escape", mutate: func(a *App) { a.Image = "api:1.0\x1b[2J" }, wantErr: "not allowed in an image reference"},
		{name: "image too long", mutate: func(a *App) { a.Image = "a/" + strings.Repeat("x", 300) + ":1" }, wantErr: "at most 255 characters"},
		{name: "domain with nginx syntax", mutate: func(a *App) { a.Domain = "a.example.com; return 200" }, wantErr: "domain:"},
		{name: "domain with newline", mutate: func(a *App) { a.Domain = "a.example.com\n}" }, wantErr: "domain:"},
		{name: "domain too long", mutate: func(a *App) { a.Domain = strings.Repeat("a.", 130) + "com" }, wantErr: "domain:"},
		{name: "name with path traversal", mutate: func(a *App) { a.Name = "../etc" }, wantErr: "name:"},
		{name: "name with slash", mutate: func(a *App) { a.Name = "a/b" }, wantErr: "name:"},
		{name: "env value with NUL", mutate: func(a *App) { a.Env = map[string]string{"KEY": "a\x00b"} }, wantErr: "contains a NUL byte"},
		{name: "env value with newline is fine", mutate: func(a *App) { a.Env = map[string]string{"KEY": "line1\nline2"} }},
		{name: "healthcheck path with query", mutate: func(a *App) { a.Healthcheck = &Healthcheck{Path: "/health?deep=1"} }},
		{name: "healthcheck path with space", mutate: func(a *App) { a.Healthcheck = &Healthcheck{Path: "/health check"} }, wantErr: "healthcheck.path:"},
		{name: "healthcheck path with newline", mutate: func(a *App) { a.Healthcheck = &Healthcheck{Path: "/h\r\nHost: evil"} }, wantErr: "healthcheck.path:"},
		{name: "healthcheck path too long", mutate: func(a *App) { a.Healthcheck = &Healthcheck{Path: "/" + strings.Repeat("a", 300)} }, wantErr: "at most 200 characters"},
		{name: "domain without dot", mutate: func(a *App) { a.Domain = "localhost" }, wantErr: "domain:"},
		{name: "domain with scheme", mutate: func(a *App) { a.Domain = "https://api.example.com" }, wantErr: "domain:"},
		{name: "port zero", mutate: func(a *App) { a.Port = 0 }, wantErr: "port:"},
		{name: "port too large", mutate: func(a *App) { a.Port = 65536 }, wantErr: "port:"},
		{name: "invalid env name", mutate: func(a *App) { a.Env = map[string]string{"MY-VAR": "x"} }, wantErr: `env: invalid variable name "MY-VAR"`},
		{name: "memory without unit", mutate: func(a *App) { a.Resources.Memory = "512" }, wantErr: "resources.memory:"},
		{name: "memory with unknown unit", mutate: func(a *App) { a.Resources.Memory = "512mb" }, wantErr: "resources.memory:"},
		{name: "negative cpus", mutate: func(a *App) { a.Resources.CPUs = -1 }, wantErr: "resources.cpus:"},
		{name: "healthcheck path without slash", mutate: func(a *App) { a.Healthcheck = &Healthcheck{Path: "health"} }, wantErr: "healthcheck.path:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := validApp()
			if tt.mutate != nil {
				tt.mutate(&a)
			}

			err := a.Validate()

			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestMemoryBytes(t *testing.T) {
	tests := []struct {
		memory string
		want   int64
	}{
		{"", 0},
		{"64k", 64 * 1024},
		{"512m", 512 * 1024 * 1024},
		{"2g", 2 * 1024 * 1024 * 1024},
	}

	for _, tt := range tests {
		t.Run(tt.memory, func(t *testing.T) {
			got, err := Resources{Memory: tt.memory}.MemoryBytes()
			if err != nil {
				t.Fatalf("MemoryBytes() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("MemoryBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTooManyEnvVars(t *testing.T) {
	a := validApp()
	a.Env = map[string]string{}
	for i := range 201 {
		a.Env["VAR_"+strings.Repeat("X", i)] = "v"
	}

	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "at most 200 variables") {
		t.Errorf("Validate() error = %v, want the variable limit", err)
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"web", "orders-api", "a1"} {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false, want true", name)
		}
	}
	// Everything that could escape a directory or change a command.
	for _, name := range []string{"", "..", "../x", "a/b", "a b", "A", "a.conf", "-a", "a\x00", strings.Repeat("a", 64)} {
		if ValidName(name) {
			t.Errorf("ValidName(%q) = true, want false", name)
		}
	}
}
