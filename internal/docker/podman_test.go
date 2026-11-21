package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Podman's login writes auth.json, in the same format; Docker Hub is under
// docker.io there. The first file that has the registry wins.
func TestLookupAcrossAuthFiles(t *testing.T) {
	dir := t.TempDir()
	podman := filepath.Join(dir, "auth.json")
	docker := filepath.Join(dir, "config.json")
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(podman, `{"auths": {"ghcr.io": {"auth": "`+inlineAuth("podman-user:p1")+`"}, "docker.io": {"auth": "`+inlineAuth("hub-podman:p2")+`"}}}`)
	write(docker, `{"auths": {"ghcr.io": {"auth": "`+inlineAuth("docker-user:d1")+`"}, "quay.io": {"auth": "`+inlineAuth("quay-user:d2")+`"}}}`)
	store := newCredentialStoreIn([]string{filepath.Join(dir, "missing.json"), podman, docker})

	for image, want := range map[string]string{
		"ghcr.io/u/app:1": "podman-user", // the first file has it
		"nginx:1.27":      "hub-podman",  // docker.io, as Podman writes it
		"quay.io/u/app:1": "quay-user",   // only the second has it
	} {
		creds, found, err := store.lookup(context.Background(), image)
		if err != nil || !found || creds.Username != want {
			t.Errorf("%s: %+v, %v, %v; want %s", image, creds, found, err, want)
		}
	}
	if _, found, _ := store.lookup(context.Background(), "registry.example.com/u/app:1"); found {
		t.Error("credentials for a registry nobody logged in to")
	}
}

func TestPresets(t *testing.T) {
	if o := ForPodman(""); o.Host != "unix:///run/podman/podman.sock" || o.RegistryCheck != CheckRegistry || o.Name != "Podman" {
		t.Errorf("ForPodman() = %+v", o)
	}
	if o := ForPodman("/run/user/1000/podman/podman.sock"); o.Host != "unix:///run/user/1000/podman/podman.sock" {
		t.Errorf("ForPodman(socket).Host = %s", o.Host)
	}
	o := ForPodman("")
	if last := o.AuthFiles[len(o.AuthFiles)-1]; !strings.HasSuffix(last, filepath.Join(".docker", "config.json")) {
		t.Errorf("Podman's auth files end with %s, want Docker's config.json, which Podman falls back to", last)
	}
	if o := ForDocker(""); o.Host != "" || o.RegistryCheck != CheckDistribution {
		t.Errorf("ForDocker() = %+v", o)
	}
}

// On Podman, ImageExists asks the registry itself, with the login found in
// the auth files; Podman's socket is not asked.
func TestImageExistsOnPodmanAsksTheRegistry(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	var asked []string
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		asked = append(asked, r.Method+" "+r.URL.Path+" "+u+":"+p)
		if u != "ci" || p != "s3cret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="t"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/team/api/manifests/"+digest {
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer reg.Close()
	host := strings.TrimPrefix(reg.URL, "http://")
	auth := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(auth, []byte(`{"auths": {"`+host+`": {"auth": "`+inlineAuth("ci:s3cret")+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{creds: newCredentialStoreIn([]string{auth}), opts: Options{Name: "Podman", RegistryCheck: CheckRegistry}}

	if ok, err := c.ImageExists(context.Background(), host+"/team/api@"+digest); err != nil || !ok {
		t.Errorf("an image that is there: %v, %v (asked %v)", ok, err, asked)
	}
	if ok, err := c.ImageExists(context.Background(), host+"/team/api@sha256:"+strings.Repeat("b", 64)); err != nil || ok {
		t.Errorf("an image that is not there: %v, %v", ok, err)
	}
}
