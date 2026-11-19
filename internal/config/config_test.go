package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingFileDisablesNginx(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Nginx.Enabled() {
		t.Error("Nginx is enabled without a config file")
	}
	if cfg.Daemon.Listen != DefaultListen {
		t.Errorf("Daemon.Listen = %q, want the loopback default", cfg.Daemon.Listen)
	}
}

func TestLoadDaemonListen(t *testing.T) {
	cfg, err := Load(write(t, "daemon:\n  listen: 10.8.0.1:7070\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Daemon.Listen != "10.8.0.1:7070" {
		t.Errorf("Daemon.Listen = %q, want the configured address", cfg.Daemon.Listen)
	}
}

func TestClientConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "client.yaml")
	want := Client{URL: "http://10.8.0.1:7070", Token: "liman_abc"}

	if err := SaveClient(path, want); err != nil {
		t.Fatalf("SaveClient() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600 because the file holds a token", perm)
	}

	got, err := LoadClient(path)
	if err != nil || got != want {
		t.Errorf("LoadClient() = %+v, %v; want %+v", got, err, want)
	}
}

func TestSaveClientTightensExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	if err := os.WriteFile(path, []byte("url: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SaveClient(path, Client{URL: "http://x", Token: "liman_abc"}); err != nil {
		t.Fatalf("SaveClient() error = %v", err)
	}

	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600 even though the file existed with 644", perm)
	}
}

func TestLoadClientEnvironmentWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	if err := SaveClient(path, Client{URL: "http://from-file:7070", Token: "liman_file"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIMAN_URL", "http://from-env:7070")
	t.Setenv("LIMAN_TOKEN", "liman_env")

	got, err := LoadClient(path)

	want := Client{URL: "http://from-env:7070", Token: "liman_env"}
	if err != nil || got != want {
		t.Errorf("LoadClient() = %+v, %v; want %+v", got, err, want)
	}
}

func TestLoadClientDefaults(t *testing.T) {
	t.Setenv("LIMAN_URL", "")
	t.Setenv("LIMAN_TOKEN", "")

	got, err := LoadClient(filepath.Join(t.TempDir(), "missing.yaml"))

	if err != nil || got.URL != "http://"+DefaultListen || got.Token != "" {
		t.Errorf("LoadClient() = %+v, %v; want the default URL and no token", got, err)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	cfg, err := Load(write(t, ""))
	if err != nil || cfg.Nginx.Enabled() {
		t.Errorf("Load() = %+v, %v; want an empty config", cfg, err)
	}
}

func TestLoadAppliesHostDefaults(t *testing.T) {
	cfg, err := Load(write(t, "nginx:\n  conf_dir: /etc/nginx/liman\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	n := cfg.Nginx
	if !n.Enabled() || n.UpstreamHost != "127.0.0.1" || n.ProbeAddr != "127.0.0.1:80" {
		t.Errorf("Nginx = %+v, want it enabled with upstream 127.0.0.1 and probe 127.0.0.1:80", n)
	}
	if want := []string{"nginx", "-t"}; !slices.Equal(n.TestCommand, want) {
		t.Errorf("TestCommand = %v, want %v", n.TestCommand, want)
	}
	if want := []string{"nginx", "-s", "reload"}; !slices.Equal(n.ReloadCommand, want) {
		t.Errorf("ReloadCommand = %v, want %v", n.ReloadCommand, want)
	}
}

func TestLoadKeepsExplicitValues(t *testing.T) {
	cfg, err := Load(write(t, `
nginx:
  conf_dir: /srv/liman/nginx
  upstream_host: host.docker.internal
  test_command: [docker, exec, liman-nginx, nginx, -t]
  reload_command: [docker, exec, liman-nginx, nginx, -s, reload]
  cert_dir: /srv/liman/certs
  acme_root: /srv/liman/acme
`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	n := cfg.Nginx
	if n.UpstreamHost != "host.docker.internal" || n.CertDir != "/srv/liman/certs" || n.ACMERoot != "/srv/liman/acme" {
		t.Errorf("Nginx = %+v", n)
	}
	if want := []string{"docker", "exec", "liman-nginx", "nginx", "-t"}; !slices.Equal(n.TestCommand, want) {
		t.Errorf("TestCommand = %v, want %v", n.TestCommand, want)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"unknown key", "nginx:\n  confdir: /x\n", "confdir"},
		{"relative conf dir", "nginx:\n  conf_dir: nginx\n", "nginx.conf_dir: must be an absolute path"},
		{"relative cert dir", "nginx:\n  conf_dir: /x\n  cert_dir: certs\n", "nginx.cert_dir: must be an absolute path"},
		{"not yaml", "nginx: [", "parse yaml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(t, tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
