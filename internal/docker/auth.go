package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/registry"
)

// dockerHub is the key Docker uses for Docker Hub in its config file, for
// historical reasons.
const dockerHub = "https://index.docker.io/v1/"

// credentials is a user name and a secret for one registry.
type credentials struct {
	Username string
	Password string
}

// credentialStore finds registry credentials the way the docker CLI does:
// in the config file written by "docker login". liman keeps no credentials
// of its own; logging in once with docker is all the setup there is.
type credentialStore struct {
	// configPath is the Docker config file, normally ~/.docker/config.json.
	configPath string

	// runHelper runs a credential helper program with input on stdin and
	// returns its stdout. It is a field so tests can replace it.
	runHelper func(ctx context.Context, program, stdin string) ([]byte, error)
}

// newCredentialStore returns a store that reads the same config file as the
// docker CLI: $DOCKER_CONFIG/config.json if that variable is set, otherwise
// ~/.docker/config.json.
func newCredentialStore() *credentialStore {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".docker")
		}
	}
	return &credentialStore{
		configPath: filepath.Join(dir, "config.json"),
		runHelper:  execHelper,
	}
}

// dockerConfig is the part of Docker's config file that concerns
// credentials. Fields the file has but this struct lacks are ignored by the
// JSON decoder.
type dockerConfig struct {
	// Auths maps a registry to credentials stored in the file itself.
	Auths map[string]struct {
		// Auth is base64("username:password").
		Auth string `json:"auth"`
	} `json:"auths"`

	// CredsStore names a helper program that keeps the secrets somewhere
	// safer than this file, such as the macOS keychain.
	CredsStore string `json:"credsStore"`

	// CredHelpers overrides CredsStore for individual registries.
	CredHelpers map[string]string `json:"credHelpers"`
}

// lookup returns the credentials for the registry that image lives on.
// found is false, with a nil error, when the user is simply not logged in
// to that registry.
func (s *credentialStore) lookup(ctx context.Context, image string) (creds credentials, found bool, err error) {
	data, err := os.ReadFile(s.configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return credentials{}, false, nil
	}
	if err != nil {
		return credentials{}, false, fmt.Errorf("read docker config: %w", err)
	}

	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return credentials{}, false, fmt.Errorf("parse %s: %w", s.configPath, err)
	}

	host := registryHost(image)

	// A helper configured for this registry takes precedence over the
	// general store, and both take precedence over the file.
	helper := cfg.CredHelpers[host]
	if helper == "" {
		helper = cfg.CredsStore
	}
	if helper != "" {
		return s.fromHelper(ctx, helper, host)
	}

	entry, ok := cfg.Auths[host]
	if !ok || entry.Auth == "" {
		return credentials{}, false, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return credentials{}, false, fmt.Errorf("%s: credentials for %s are not valid base64", s.configPath, host)
	}
	// The password may itself contain colons, so split at the first one
	// only. strings.Cut does exactly that.
	user, pass, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return credentials{}, false, fmt.Errorf("%s: credentials for %s are malformed", s.configPath, host)
	}
	return credentials{Username: user, Password: pass}, true, nil
}

// fromHelper asks a credential helper for the credentials of host.
//
// The protocol is small: run "docker-credential-<name> get", write the
// registry to its stdin, read JSON from its stdout.
func (s *credentialStore) fromHelper(ctx context.Context, helper, host string) (credentials, bool, error) {
	program := "docker-credential-" + helper

	out, err := s.runHelper(ctx, program, host)
	if err != nil {
		// Helpers report "not logged in" as a failure with this message
		// rather than with an empty result.
		if strings.Contains(string(out), "credentials not found") {
			return credentials{}, false, nil
		}
		return credentials{}, false, fmt.Errorf("%s: %w: %s", program, err, strings.TrimSpace(string(out)))
	}

	var reply struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		return credentials{}, false, fmt.Errorf("%s returned invalid JSON: %w", program, err)
	}
	if reply.Secret == "" {
		return credentials{}, false, nil
	}
	return credentials{Username: reply.Username, Password: reply.Secret}, true, nil
}

// execHelper runs a credential helper. Its output is returned even on
// failure, because that is where the helper explains what went wrong.
func execHelper(ctx context.Context, program, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, "get")
	cmd.Stdin = strings.NewReader(stdin)

	// Only stdout is captured: it carries the secret on success and the
	// message on failure. Stderr is left alone.
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	return stdout.Bytes(), err
}

// registryHost returns the registry part of an image reference, in the form
// Docker uses as a key for credentials.
//
//	ghcr.io/user/app:1.4   -> ghcr.io
//	localhost:5000/app:1.0 -> localhost:5000
//	nginx:1.27             -> Docker Hub
//	traefik/whoami:v1.10   -> Docker Hub
func registryHost(image string) string {
	first, _, hasSlash := strings.Cut(image, "/")

	// Docker's own rule: the first path segment is a registry only if it
	// looks like a host name (contains a dot or a port) or is "localhost".
	// Otherwise it is a Docker Hub user or organisation.
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return dockerHub
}

// displayHost returns registryHost in a form meant for people.
func displayHost(image string) string {
	if host := registryHost(image); host != dockerHub {
		return host
	}
	return "Docker Hub"
}

// encodeAuth returns credentials in the form the Docker API expects in its
// X-Registry-Auth header: URL-safe base64 of a small JSON document.
//
// Base64 is an encoding, not encryption. The header is safe only because
// the connection to the daemon is a local Unix socket.
func encodeAuth(creds credentials, image string) (string, error) {
	data, err := json.Marshal(registry.AuthConfig{
		Username:      creds.Username,
		Password:      creds.Password,
		ServerAddress: registryHost(image),
	})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(data), nil
}
