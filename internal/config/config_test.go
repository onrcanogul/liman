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

func TestLoadMissingFileDisablesTheProxy(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Proxy.Enabled() {
		t.Error("the proxy is enabled without a config file")
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
	if err != nil || cfg.Proxy.Enabled() {
		t.Errorf("Load() = %+v, %v; want an empty config", cfg, err)
	}
}

func TestLoadAppliesHostDefaults(t *testing.T) {
	cfg, err := Load(write(t, "nginx:\n  conf_dir: /etc/nginx/liman\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	n := cfg.Proxy
	if !n.Enabled() || n.Driver != "nginx" || n.UpstreamHost != "127.0.0.1" || n.ProbeAddr != "127.0.0.1:80" {
		t.Errorf("Proxy = %+v, want nginx enabled with upstream 127.0.0.1 and probe 127.0.0.1:80", n)
	}
	// The commands are the driver's, filled in by it, not here.
	if len(n.TestCommand) != 0 || len(n.ReloadCommand) != 0 {
		t.Errorf("commands = %v, %v; want them left to the driver", n.TestCommand, n.ReloadCommand)
	}
}

func TestLoadProxySection(t *testing.T) {
	cfg, err := Load(write(t, "proxy:\n  driver: caddy\n  conf_dir: /etc/caddy/liman.d\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if p := cfg.Proxy; p.Driver != "caddy" || p.ConfDir != "/etc/caddy/liman.d" || p.Section() != "proxy" {
		t.Errorf("Proxy = %+v (section %s)", p, p.Section())
	}
	// The config package knows settings, not drivers: an unknown one is
	// refused where the proxy is built.
	if _, err := Load(write(t, "proxy:\n  driver: nope\n  conf_dir: /x\n")); err != nil {
		t.Errorf("Load() with an unknown driver error = %v, want it left to the daemon", err)
	}
}

func TestLoadNginxSectionIsTheProxyForNginx(t *testing.T) {
	cfg, err := Load(write(t, "nginx:\n  conf_dir: /etc/nginx/liman.d\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if p := cfg.Proxy; p.Driver != "nginx" || p.ConfDir != "/etc/nginx/liman.d" || p.Section() != "nginx" || cfg.LegacyNginx != nil {
		t.Errorf("Proxy = %+v (section %s), LegacyNginx = %v", p, p.Section(), cfg.LegacyNginx)
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

	n := cfg.Proxy
	if n.UpstreamHost != "host.docker.internal" || n.CertDir != "/srv/liman/certs" || n.ACMERoot != "/srv/liman/acme" {
		t.Errorf("Proxy = %+v", n)
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
		{"acme without a proxy", "acme:\n  directory: https://ca.example/dir\n", "acme: needs proxy.conf_dir"},
		{"both sections", "nginx:\n  conf_dir: /x\nproxy:\n  conf_dir: /y\n", "keep only the proxy section"},
		{"nginx section for another proxy", "nginx:\n  driver: caddy\n  conf_dir: /x\n", "write a proxy section"},
		{"relative proxy conf dir", "proxy:\n  conf_dir: liman.d\n", "proxy.conf_dir: must be an absolute path"},
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

func TestLoadHooks(t *testing.T) {
	cfg, err := Load(write(t, "daemon:\n  listen: 127.0.0.1:7070\n"))
	if err != nil || cfg.Hooks.Enabled() {
		t.Errorf("Load() without hooks = %+v, %v; want them off", cfg.Hooks, err)
	}
	cfg, err = Load(write(t, "hooks:\n  listen: 127.0.0.1:7443\n"))
	if err != nil || !cfg.Hooks.Enabled() || cfg.Hooks.Listen != "127.0.0.1:7443" {
		t.Errorf("Load() = %+v, %v", cfg.Hooks, err)
	}
	for content, want := range map[string]string{
		"hooks:\n  listen: 0.0.0.0:7443\n":                                      "hooks.listen",
		"hooks:\n  listen: 203.0.113.7:7443\n":                                  "public address",
		"daemon:\n  listen: 127.0.0.1:7070\nhooks:\n  listen: 127.0.0.1:7070\n": "must differ",
	} {
		if _, err := Load(write(t, content)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(%q) error = %v; want %q", content, err, want)
		}
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
