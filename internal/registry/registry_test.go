package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	for ref, want := range map[string]Ref{
		"nginx:1.27":                        {"registry-1.docker.io", "library/nginx", "1.27"},
		"traefik/whoami@" + d:               {"registry-1.docker.io", "traefik/whoami", d},
		"docker.io/traefik/whoami:v1.10":    {"registry-1.docker.io", "traefik/whoami", "v1.10"},
		"ghcr.io/team/api@" + d:             {"ghcr.io", "team/api", d},
		"localhost:5000/whoami:ci":          {"localhost:5000", "whoami", "ci"},
		"registry.example.com:8443/a/b/c:2": {"registry.example.com:8443", "a/b/c", "2"},
		"ghcr.io/team/api":                  {"ghcr.io", "team/api", "latest"},
	} {
		got, err := Parse(ref)
		if err != nil || got != want {
			t.Errorf("Parse(%s) = %+v, %v; want %+v", ref, got, err, want)
		}
	}
}

func TestOnlyLocalRegistriesMayBePlainHTTP(t *testing.T) {
	for host, want := range map[string]string{
		"localhost:5000":       "https,http",
		"127.0.0.1:5000":       "https,http",
		"[::1]:5000":           "https,http",
		"ghcr.io":              "https",
		"10.8.0.1:5000":        "https",
		"registry-1.docker.io": "https",
	} {
		if got := strings.Join(Ref{Host: host}.schemes(), ","); got != want {
			t.Errorf("schemes(%s) = %s, want %s", host, got, want)
		}
	}
}

// registry plays a registry on 127.0.0.1 over plain HTTP, which is what a
// local one is, holding one manifest; auth says how it wants logins.
func registry(t *testing.T, auth string) string {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if u, p, ok := r.BasicAuth(); !ok || u != "ci" || p != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("scope") != "repository:team/api:pull" || r.URL.Query().Get("service") != "test" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			http.Error(w, "no accept", http.StatusBadRequest)
			return
		}
		switch auth {
		case "basic":
			if u, p, ok := r.BasicAuth(); !ok || u != "ci" || p != "s3cret" {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		case "bearer":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test",scope="repository:team/api:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		if r.URL.Path == "/v2/team/api/manifests/sha256:"+strings.Repeat("a", 64) {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestExists(t *testing.T) {
	there := "/team/api@sha256:" + strings.Repeat("a", 64)
	missing := "/team/api@sha256:" + strings.Repeat("b", 64)
	good := Credentials{Username: "ci", Password: "s3cret"}
	bad := Credentials{Username: "ci", Password: "wrong"}
	tests := []struct {
		name    string
		auth    string
		ref     string
		creds   Credentials
		want    bool
		wantErr string
	}{
		{"anonymous, there", "", there, Credentials{}, true, ""},
		{"anonymous, missing", "", missing, Credentials{}, false, ""},
		{"basic", "basic", there, good, true, ""},
		{"basic, missing", "basic", missing, good, false, ""},
		{"basic, wrong password", "basic", there, bad, false, "refused the login of ci"},
		{"basic, no login", "basic", there, Credentials{}, false, "wants a login"},
		{"bearer", "bearer", there, good, true, ""},
		{"bearer, missing", "bearer", missing, good, false, ""},
		{"bearer, wrong password", "bearer", there, bad, false, "token service answered 401"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := registry(t, tt.auth)
			got, err := (&Checker{}).Exists(context.Background(), host+tt.ref, tt.creds)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("Exists() = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, p := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:team/api:pull"`)
	if scheme != "Bearer" || p["realm"] != "https://ghcr.io/token" || p["service"] != "ghcr.io" || p["scope"] != "repository:team/api:pull" {
		t.Errorf("parseChallenge = %s %v", scheme, p)
	}
}
