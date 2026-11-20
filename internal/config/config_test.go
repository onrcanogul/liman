package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestLoadSecretsKeyFile(t *testing.T) {
	cfg, err := Load(write(t, "secrets:\n  key_file: /etc/liman/master.key\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Secrets.KeyFile != "/etc/liman/master.key" {
		t.Errorf("Secrets.KeyFile = %q, want the configured path", cfg.Secrets.KeyFile)
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
	if err := SaveClient(path, Client{URL: "http://10.1.1.1:7070", Token: "liman_file"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIMAN_URL", "http://10.9.9.9:7070")
	t.Setenv("LIMAN_TOKEN", "liman_env")

	got, err := LoadClient(path)

	want := Client{URL: "http://10.9.9.9:7070", Token: "liman_env"}
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
		{"acme over plain http", acmeConfig("directory: http://ca.example/dir"), "acme.directory: must be an https:// URL"},
		{"acme not a url", acmeConfig("directory: ca.example"), "acme.directory: must be an https:// URL"},
		{"acme bad email", acmeConfig("directory: https://ca.example/dir\n  email: someone"), "acme.email"},
		{"acme email with a newline", acmeConfig("directory: https://ca.example/dir\n  email: \"a@b\\nc\""), "acme.email"},
		{"acme relative ca file", acmeConfig("directory: https://ca.example/dir\n  ca_file: ca.pem"), "acme.ca_file: must be an absolute path"},
		{"acme without nginx", "acme:\n  directory: https://ca.example/dir\n", "acme: needs nginx.conf_dir"},
		{"acme without acme root", "nginx:\n  conf_dir: /x\n  cert_dir: /c\nacme:\n  directory: https://ca.example/dir\n", "acme: needs nginx.cert_dir"},
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

func TestCheckListenAddress(t *testing.T) {
	tests := []struct {
		address string
		wantErr string // empty means allowed
	}{
		{"127.0.0.1:7070", ""},
		{"[::1]:7070", ""},
		{"10.8.0.1:7070", ""},      // typical WireGuard address
		{"192.168.1.20:7070", ""},  // home or office network
		{"172.16.5.4:7070", ""},    // private
		{"100.101.102.103:80", ""}, // Tailscale (shared address space)
		{"[fd00::1]:7070", ""},     // IPv6 unique local

		{"0.0.0.0:7070", "every interface"},
		{"[::]:7070", "every interface"},
		{":7070", "every interface"},
		{"8.8.8.8:7070", "public address"},
		{"203.0.113.10:7070", "public address"},
		{"[2001:db8::1]:7070", "public address"},
		{"[::ffff:8.8.8.8]:7070", "public address"}, // IPv4 hidden in IPv6 form
		{"example.com:7070", "must be an IP address"},
		{"localhost:7070", "must be an IP address"},
		{"127.0.0.1", "host:port"},
	}

	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			err := CheckListenAddress(tt.address)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("CheckListenAddress() error = %v, want it allowed", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("CheckListenAddress() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsPublicListenAddress(t *testing.T) {
	_, err := Load(write(t, "daemon:\n  listen: 0.0.0.0:7070\n"))

	if err == nil || !strings.Contains(err.Error(), "daemon.listen") {
		t.Errorf("Load() error = %v, want daemon.listen to be rejected", err)
	}
}

func TestCheckClientURL(t *testing.T) {
	tests := []struct {
		url     string
		wantErr string // empty means allowed
	}{
		{"http://127.0.0.1:7070", ""},
		{"http://localhost:7070", ""},
		{"http://10.8.0.1:7070", ""},
		{"http://[fd00::1]:7070", ""},
		{"https://liman.example.com", ""}, // TLS protects the token
		{"https://8.8.8.8:7070", ""},

		{"http://8.8.8.8:7070", "public address"},
		{"http://liman.example.com:7070", "host name"},
		{"ftp://10.8.0.1", "must start with http"},
		{"10.8.0.1:7070", "must start with http"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			err := CheckClientURL(tt.url)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("CheckClientURL() error = %v, want it allowed", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("CheckClientURL() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadClientRejectsClearTextToPublicAddress(t *testing.T) {
	t.Setenv("LIMAN_URL", "http://203.0.113.10:7070")
	t.Setenv("LIMAN_TOKEN", "liman_secret")

	_, err := LoadClient(filepath.Join(t.TempDir(), "missing.yaml"))

	if err == nil || !strings.Contains(err.Error(), "refusing to send the token") {
		t.Fatalf("LoadClient() error = %v, want a refusal", err)
	}
	if strings.Contains(err.Error(), "liman_secret") {
		t.Error("the error message contains the token")
	}
}

func TestLoadMonitorSettings(t *testing.T) {
	cfg, err := Load(write(t, "monitor:\n  interval: 1m30s\n  failure_threshold: 5\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got := time.Duration(cfg.Monitor.Interval); got != 90*time.Second {
		t.Errorf("Interval = %s, want 1m30s", got)
	}
	if cfg.Monitor.FailureThreshold != 5 || cfg.Monitor.Disabled {
		t.Errorf("Monitor = %+v", cfg.Monitor)
	}
}

func TestLoadRejectsBadMonitorSettings(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"number without unit", "monitor:\n  interval: 10\n", "is not a duration"},
		{"not a duration", "monitor:\n  interval: soon\n", "is not a duration"},
		{"too short", "monitor:\n  interval: 100ms\n", "at least 1s"},
		{"negative threshold", "monitor:\n  failure_threshold: -1\n", "must not be negative"},
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

func TestLoadDeployDrain(t *testing.T) {
	// Left out: nil, meaning "use the default".
	cfg, err := Load(write(t, "monitor:\n  interval: 10s\n"))
	if err != nil || cfg.Deploy.Drain != nil {
		t.Errorf("Drain without a setting = %v, %v; want nil", cfg.Deploy.Drain, err)
	}

	// Zero is a real value: do not wait.
	cfg, err = Load(write(t, "deploy:\n  drain: 0s\n"))
	if err != nil || cfg.Deploy.Drain == nil || time.Duration(*cfg.Deploy.Drain) != 0 {
		t.Errorf("Drain 0s = %v, %v; want a pointer to zero", cfg.Deploy.Drain, err)
	}

	cfg, err = Load(write(t, "deploy:\n  drain: 30s\n"))
	if err != nil || cfg.Deploy.Drain == nil || time.Duration(*cfg.Deploy.Drain) != 30*time.Second {
		t.Errorf("Drain 30s = %v, %v", cfg.Deploy.Drain, err)
	}

	if _, err := Load(write(t, "deploy:\n  drain: 1h\n")); err == nil || !strings.Contains(err.Error(), "deploy.drain") {
		t.Errorf("Drain 1h error = %v, want it rejected", err)
	}
}

// acmeConfig returns a config with Nginx set up for ACME and the given
// lines (indented by two spaces) under acme:.
func acmeConfig(lines string) string {
	return "nginx:\n  conf_dir: /x\n  cert_dir: /c\n  acme_root: /a\nacme:\n  " + lines + "\n"
}

func TestLoadACME(t *testing.T) {
	cfg, err := Load(write(t, acmeConfig("directory: https://ca.example/dir\n  email: ops@example.com\n  ca_file: /etc/liman/ca.pem")))
	if err != nil {
		t.Fatal(err)
	}
	want := ACME{Directory: "https://ca.example/dir", Email: "ops@example.com", CAFile: "/etc/liman/ca.pem"}
	if cfg.ACME != want || !cfg.ACME.Enabled() {
		t.Errorf("ACME = %+v, want %+v (enabled)", cfg.ACME, want)
	}

	cfg, err = Load(write(t, "nginx:\n  conf_dir: /x\n"))
	if err != nil || cfg.ACME.Enabled() {
		t.Errorf("without an acme section: %+v, %v; want ACME off", cfg.ACME, err)
	}
}
