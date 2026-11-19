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
