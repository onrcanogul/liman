package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/registry"
)

func TestRegistryHost(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"ghcr.io/user/app:1.4", "ghcr.io"},
		{"ghcr.io/user/app@sha256:abc", "ghcr.io"},
		{"localhost:5000/app:1.0", "localhost:5000"},
		{"localhost/app:1.0", "localhost"},
		{"registry.example.com:8443/team/app:2", "registry.example.com:8443"},
		// No registry in the reference: Docker Hub.
		{"nginx:1.27", dockerHub},
		{"traefik/whoami:v1.10", dockerHub},
		{"library/nginx:1.27", dockerHub},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			if got := registryHost(tt.image); got != tt.want {
				t.Errorf("registryHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

// storeWithConfig returns a credential store reading a config file with the
// given content. Any helper call fails the test unless helper is set.
func storeWithConfig(t *testing.T, content string) *credentialStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &credentialStore{
		configPath: path,
		runHelper: func(context.Context, string, string) ([]byte, error) {
			t.Error("a credential helper was run unexpectedly")
			return nil, errors.New("unexpected helper call")
		},
	}
}

func inlineAuth(userAndPassword string) string {
	return base64.StdEncoding.EncodeToString([]byte(userAndPassword))
}

func TestLookupFromConfigFile(t *testing.T) {
	store := storeWithConfig(t, `{
		"auths": {
			"ghcr.io": {"auth": "`+inlineAuth("octocat:ghp_token:with:colons")+`"},
			"https://index.docker.io/v1/": {"auth": "`+inlineAuth("hubuser:hubpass")+`"},
			"empty.example.com": {}
		},
		"currentContext": "desktop-linux"
	}`)

	tests := []struct {
		name      string
		image     string
		wantFound bool
		want      credentials
	}{
		{name: "private registry", image: "ghcr.io/user/app:1.4", wantFound: true, want: credentials{"octocat", "ghp_token:with:colons"}},
		{name: "docker hub", image: "nginx:1.27", wantFound: true, want: credentials{"hubuser", "hubpass"}},
		{name: "not logged in", image: "quay.io/org/app:1", wantFound: false},
		{name: "entry without secret", image: "empty.example.com/app:1", wantFound: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, err := store.lookup(context.Background(), tt.image)
			if err != nil {
				t.Fatalf("lookup() error = %v", err)
			}
			if found != tt.wantFound || got != tt.want {
				t.Errorf("lookup() = %+v, %v; want %+v, %v", got, found, tt.want, tt.wantFound)
			}
		})
	}
}

func TestLookupWithoutConfigFile(t *testing.T) {
	store := storeWithConfig(t, "")

	_, found, err := store.lookup(context.Background(), "ghcr.io/user/app:1.4")

	if err != nil || found {
		t.Errorf("lookup() = found %v, error %v; want not found and no error", found, err)
	}
}

func TestLookupRejectsBrokenConfig(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"not json", `{"auths": `, "parse"},
		{"not base64", `{"auths": {"ghcr.io": {"auth": "%%%"}}}`, "not valid base64"},
		{"no colon", `{"auths": {"ghcr.io": {"auth": "` + inlineAuth("justauser") + `"}}}`, "malformed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := storeWithConfig(t, tt.content).lookup(context.Background(), "ghcr.io/user/app:1.4")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("lookup() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLookupUsesCredentialHelper(t *testing.T) {
	tests := []struct {
		name        string
		config      string
		image       string
		wantProgram string
	}{
		{
			name:        "general store",
			config:      `{"credsStore": "osxkeychain"}`,
			image:       "ghcr.io/user/app:1.4",
			wantProgram: "docker-credential-osxkeychain",
		},
		{
			name:        "per-registry helper wins over the store",
			config:      `{"credsStore": "osxkeychain", "credHelpers": {"ghcr.io": "pass"}}`,
			image:       "ghcr.io/user/app:1.4",
			wantProgram: "docker-credential-pass",
		},
		{
			name:        "helper wins over inline credentials",
			config:      `{"credsStore": "desktop", "auths": {"ghcr.io": {"auth": "` + inlineAuth("stale:stale") + `"}}}`,
			image:       "ghcr.io/user/app:1.4",
			wantProgram: "docker-credential-desktop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := storeWithConfig(t, tt.config)

			var gotProgram, gotStdin string
			store.runHelper = func(_ context.Context, program, stdin string) ([]byte, error) {
				gotProgram, gotStdin = program, stdin
				return []byte(`{"ServerURL":"ghcr.io","Username":"octocat","Secret":"ghp_token"}`), nil
			}

			got, found, err := store.lookup(context.Background(), tt.image)
			if err != nil {
				t.Fatalf("lookup() error = %v", err)
			}

			if want := (credentials{"octocat", "ghp_token"}); !found || got != want {
				t.Errorf("lookup() = %+v, %v; want %+v, true", got, found, want)
			}
			if gotProgram != tt.wantProgram {
				t.Errorf("ran %q, want %q", gotProgram, tt.wantProgram)
			}
			// The helper is asked by registry, not by image.
			if gotStdin != "ghcr.io" {
				t.Errorf("helper stdin = %q, want the registry host", gotStdin)
			}
		})
	}
}

func TestLookupHelperNotLoggedIn(t *testing.T) {
	store := storeWithConfig(t, `{"credsStore": "desktop"}`)
	store.runHelper = func(context.Context, string, string) ([]byte, error) {
		return []byte("credentials not found in native keychain\n"), errors.New("exit status 1")
	}

	_, found, err := store.lookup(context.Background(), "ghcr.io/user/app:1.4")

	// Being logged out is a normal state, not a failure.
	if err != nil || found {
		t.Errorf("lookup() = found %v, error %v; want not found and no error", found, err)
	}
}

func TestLookupHelperFailure(t *testing.T) {
	store := storeWithConfig(t, `{"credsStore": "desktop"}`)
	helperErr := errors.New("exec: \"docker-credential-desktop\": executable file not found in $PATH")
	store.runHelper = func(context.Context, string, string) ([]byte, error) {
		return nil, helperErr
	}

	_, _, err := store.lookup(context.Background(), "ghcr.io/user/app:1.4")

	if !errors.Is(err, helperErr) {
		t.Errorf("lookup() error = %v, want it to wrap the helper error", err)
	}
}

func TestExecHelperPassesStdinAndReturnsOutputOnFailure(t *testing.T) {
	// A stand-in helper: a shell script on PATH that echoes what it got.
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"arg=$1 stdin=$(cat)\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-credential-fake"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// t.Setenv restores the variable when the test ends.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := execHelper(context.Background(), "docker-credential-fake", "ghcr.io")

	if err == nil {
		t.Error("execHelper() error = nil, want the exit status")
	}
	if got := strings.TrimSpace(string(out)); got != "arg=get stdin=ghcr.io" {
		t.Errorf("output = %q, want the helper to receive \"get\" and the host", got)
	}
}

func TestEncodeAuth(t *testing.T) {
	encoded, err := encodeAuth(credentials{"octocat", "ghp_token"}, "ghcr.io/user/app:1.4")
	if err != nil {
		t.Fatalf("encodeAuth() error = %v", err)
	}

	// Decode it the way the daemon does and check the content.
	data, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("result is not URL-safe base64: %v", err)
	}
	var got registry.AuthConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("result does not contain JSON: %v", err)
	}

	want := registry.AuthConfig{Username: "octocat", Password: "ghp_token", ServerAddress: "ghcr.io"}
	if got != want {
		t.Errorf("decoded = %+v, want %+v", got, want)
	}
}

func TestExplainPullError(t *testing.T) {
	denied := errors.New("pull ghcr.io/user/app:1.4: denied")

	tests := []struct {
		name     string
		err      error
		image    string
		loggedIn bool
		want     string // empty means the error must be unchanged
	}{
		{
			name:  "denied and not logged in",
			err:   denied,
			image: "ghcr.io/user/app:1.4",
			want:  "no credentials for ghcr.io were found; if the image is private, log in with: docker login ghcr.io",
		},
		{
			name:     "denied although logged in",
			err:      denied,
			image:    "ghcr.io/user/app:1.4",
			loggedIn: true,
			want:     "stored credentials for ghcr.io were rejected or lack access to this image; log in again with: docker login ghcr.io",
		},
		{
			name:  "docker hub needs no host argument",
			err:   errors.New("pull user/app:1: unauthorized: authentication required"),
			image: "user/app:1",
			want:  "no credentials for Docker Hub were found; if the image is private, log in with: docker login",
		},
		{
			name:  "unrelated errors are left alone",
			err:   errors.New("pull ghcr.io/user/app:9: manifest unknown"),
			image: "ghcr.io/user/app:9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := explainPullError(tt.err, tt.image, tt.loggedIn)

			// The original error must stay reachable either way.
			if !errors.Is(got, tt.err) {
				t.Errorf("result %q does not wrap the original error", got)
			}
			if tt.want == "" {
				if got.Error() != tt.err.Error() {
					t.Errorf("error was changed to %q", got)
				}
				return
			}
			if !strings.Contains(got.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", got, tt.want)
			}
			if strings.HasSuffix(got.Error(), " ") {
				t.Errorf("error %q has trailing whitespace", got.Error())
			}
		})
	}
}
