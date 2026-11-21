// Package registry asks a container registry whether it has an image,
// over the registry's own HTTP API (the OCI distribution spec), without
// pulling it.
//
// Docker answers this question itself (GET /distribution/{name}/json).
// Podman's compatible API has no such endpoint, and its own
// /libpod/manifests answers 404 for every failure, a refused password or a
// TLS error as much as a missing image (a trial showed all three). For an
// engine like that, liman asks the registry directly.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Credentials are a registry login; empty for anonymous access.
type Credentials struct {
	Username string
	Password string
}

// Checker asks registries whether they have images.
type Checker struct {
	// Client makes the requests; nil uses one with a timeout.
	Client *http.Client
}

// manifestTypes are the manifests an image reference can name: a single
// image or an index of them, in OCI's and Docker's formats.
var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Exists reports whether the registry has ref (repository@digest or
// repository:tag). A registry that cannot be asked, or refuses the
// credentials, is an error, never a "no".
func (c *Checker) Exists(ctx context.Context, ref string, creds Credentials) (bool, error) {
	r, err := Parse(ref)
	if err != nil {
		return false, err
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	var lastErr error
	for _, scheme := range r.schemes() {
		ok, err := r.head(ctx, client, scheme, creds)
		if err == nil {
			return ok, nil
		}
		lastErr = err
		// Only a registry that does not speak TLS at all gets a second
		// try, and only a local one (see schemes).
		var notTLS tlsMismatch
		if !errors.As(err, &notTLS) {
			break
		}
	}
	return false, fmt.Errorf("ask %s for %s: %w", r.Host, ref, lastErr)
}

// Ref is an image reference taken apart.
type Ref struct {
	// Host is the registry's address, registry-1.docker.io for Docker Hub.
	Host string
	// Repository is the path in the registry: library/nginx, user/app.
	Repository string
	// Reference is the digest (sha256:…) or the tag.
	Reference string
}

// Parse takes ref apart the way Docker does: the first path segment is a
// registry if it has a dot or a port or is localhost; otherwise the image
// is on Docker Hub, where a single name is an official image (library/).
func Parse(ref string) (Ref, error) {
	name, reference := ref, "latest"
	if i := strings.Index(ref, "@"); i >= 0 {
		name, reference = ref[:i], ref[i+1:]
	} else if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, reference = ref[:i], ref[i+1:]
	}
	if name == "" || reference == "" {
		return Ref{}, fmt.Errorf("%q is not an image reference", ref)
	}
	host, repo := "registry-1.docker.io", name
	if first, rest, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		host, repo = first, rest
	}
	if host == "docker.io" || host == "index.docker.io" {
		host = "registry-1.docker.io"
	}
	if host == "registry-1.docker.io" && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return Ref{Host: host, Repository: repo, Reference: reference}, nil
}

// schemes are the protocols to try. Docker takes a registry on the machine
// itself for a plain-HTTP one; so does liman, after trying HTTPS. Any other
// registry is HTTPS only.
func (r Ref) schemes() []string {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if host == "localhost" {
		return []string{"https", "http"}
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return []string{"https", "http"}
	}
	return []string{"https"}
}

// tlsMismatch is a registry answering plain HTTP on an HTTPS request.
type tlsMismatch struct{ err error }

func (e tlsMismatch) Error() string { return e.err.Error() }
func (e tlsMismatch) Unwrap() error { return e.err }

// head asks for the manifest, logging in when the registry asks for it.
func (r Ref) head(ctx context.Context, client *http.Client, scheme string, creds Credentials) (bool, error) {
	u := scheme + "://" + r.Host + "/v2/" + r.Repository + "/manifests/" + r.Reference
	resp, err := r.send(ctx, client, u, "")
	if err != nil {
		return false, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		auth, err := authorize(ctx, client, resp.Header.Get("WWW-Authenticate"), creds)
		if err != nil {
			return false, err
		}
		if resp, err = r.send(ctx, client, u, auth); err != nil {
			return false, err
		}
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		if creds.Username == "" {
			return false, fmt.Errorf("the registry wants a login (%s); log in on the server", resp.Status)
		}
		return false, fmt.Errorf("the registry refused the login of %s (%s)", creds.Username, resp.Status)
	default:
		return false, fmt.Errorf("the registry answered %s", resp.Status)
	}
}

func (r Ref) send(ctx context.Context, client *http.Client, u, auth string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestTypes)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "server gave HTTP response to HTTPS client") {
			return nil, tlsMismatch{err}
		}
		return nil, err
	}
	_ = resp.Body.Close()
	return resp, nil
}

// authorize answers a registry's challenge: Basic with the credentials, or
// Bearer with a token from the registry's token service (Docker Hub, GHCR
// and most others), which the credentials, if any, are given to.
func authorize(ctx context.Context, client *http.Client, challenge string, creds Credentials) (string, error) {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if creds.Username == "" {
			return "", errors.New("the registry wants a login; log in on the server")
		}
		req, _ := http.NewRequest(http.MethodGet, "http://x", nil)
		req.SetBasicAuth(creds.Username, creds.Password)
		return req.Header.Get("Authorization"), nil
	case "bearer":
		return bearer(ctx, client, params, creds)
	default:
		return "", fmt.Errorf("the registry asks for %q, which liman cannot answer", challenge)
	}
}

func bearer(ctx context.Context, client *http.Client, params map[string]string, creds Credentials) (string, error) {
	realm := params["realm"]
	u, err := url.Parse(realm)
	if err != nil || realm == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("the registry's token service %q is not a URL", realm)
	}
	q := u.Query()
	for _, k := range []string{"service", "scope"} {
		if v := params[k]; v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if creds.Username != "" {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get a token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the registry's token service answered %s", resp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("read the token: %w", err)
	}
	t := tok.Token
	if t == "" {
		t = tok.AccessToken
	}
	if t == "" {
		return "", errors.New("the registry's token service gave no token")
	}
	return "Bearer " + t, nil
}

// parseChallenge reads `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		var value string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				value, rest = after[1:], ""
			} else {
				value, rest = after[1:end+1], after[end+2:]
			}
		} else {
			value, rest, _ = strings.Cut(after, ",")
		}
		params[strings.ToLower(strings.TrimSpace(key))] = value
	}
	return scheme, params
}
