// Package config loads liman's own configuration: settings that describe
// the machine liman runs on, as opposed to app.yaml, which describes an app.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the content of config.yaml in liman's home directory.
type Config struct {
	Daemon  Daemon  `yaml:"daemon"`
	Deploy  Deploy  `yaml:"deploy"`
	Monitor Monitor `yaml:"monitor"`
	Nginx   Nginx   `yaml:"nginx"`
	Panel   Panel   `yaml:"panel"`
	Backup  Backup  `yaml:"backup"`
	Secrets Secrets `yaml:"secrets"`
	ACME    ACME    `yaml:"acme"`
}

// ACME configures obtaining and renewing certificates automatically, from
// Let's Encrypt or any other CA that speaks ACME. Setting Directory turns
// it on; it needs nginx.cert_dir, where the certificates go, and
// nginx.acme_root, from which Nginx serves the CA's challenges.
type ACME struct {
	// Directory is the CA's ACME directory URL, for example
	// https://acme-v02.api.letsencrypt.org/directory, or the staging one
	// while trying things out.
	Directory string `yaml:"directory"`
	// Email is given to the CA as a contact, for expiry warnings and
	// problems with the account. Optional.
	Email string `yaml:"email"`
	// CAFile is a PEM file of further certificate authorities to trust
	// when talking to Directory. Let's Encrypt does not need it; a test CA
	// such as Pebble, whose own HTTPS certificate nobody trusts, does.
	CAFile string `yaml:"ca_file"`
	// AccountKey is where the key of the ACME account is kept. It defaults
	// to acme-account.key in liman's home directory.
	AccountKey string `yaml:"account_key"`
}

// Enabled reports whether liman obtains certificates itself.
func (a ACME) Enabled() bool {
	return a.Directory != ""
}

// Backup configures the backups the daemon makes when asked to over the
// API (the panel's "Back up now"). "limand backup create" takes its
// directory as an argument and does not read this.
type Backup struct {
	// Dir is where backups are written and listed from. It defaults to the
	// directory "backups" in liman's home.
	Dir string `yaml:"dir"`
	// Keep is how many backups stay after a new one is made. It defaults
	// to 14; a pointer so that 0 ("keep them all") can be written.
	Keep *int `yaml:"keep"`
}

// Panel configures the web panel the daemon serves under /ui/.
type Panel struct {
	// Disabled turns the panel off. The API is not affected.
	Disabled bool `yaml:"disabled"`
}

// Secrets configures where the key that encrypts app secrets is kept.
type Secrets struct {
	// KeyFile is the path of the master key. It defaults to master.key in
	// liman's home directory. Keeping it somewhere else (another disk, a
	// mount that is not part of the backups) means that a copy of the
	// database alone reveals nothing.
	KeyFile string `yaml:"key_file"`
}

// Deploy configures how releases are switched.
type Deploy struct {
	// Drain is how long the previous release keeps running after the proxy
	// has been switched away from it, written like "5s". It is a pointer so
	// that leaving it out (use the default) can be told apart from "0s"
	// (do not wait at all).
	Drain *Duration `yaml:"drain"`
}

// Monitor configures the health monitor, which restarts apps that have
// stopped working.
type Monitor struct {
	// Disabled turns the monitor off.
	Disabled bool `yaml:"disabled"`
	// Interval is the pause between checks, written like "10s" or "1m".
	Interval Duration `yaml:"interval"`
	// FailureThreshold is how many health checks in a row must fail before
	// a running app is restarted.
	FailureThreshold int `yaml:"failure_threshold"`
}

// Duration is a time.Duration that can be written in YAML as "10s".
//
// YAML has no duration type, and a bare number would leave the unit to
// guesswork. Defining a named type lets it carry its own decoding.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler. The YAML decoder calls it
// whenever it has to fill a Duration.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration; write it like 10s, 2m or 1m30s", node.Line, node.Value)
	}
	*d = Duration(parsed)
	return nil
}

// Daemon configures limand itself.
type Daemon struct {
	// Listen is the address the API is served on. It defaults to the
	// loopback interface. On a server it is set to the WireGuard address;
	// it must never be a public one, because the API is plain HTTP and
	// controls Docker.
	Listen string `yaml:"listen"`
}

// DefaultListen is used when daemon.listen is not set.
const DefaultListen = "127.0.0.1:7070"

// Nginx describes how liman manages the reverse proxy. The zero value
// means "no proxy": apps are then only reachable on their loopback port.
type Nginx struct {
	// ConfDir is the directory liman writes one <app>.conf file into. The
	// main Nginx config must include it. Setting it enables the proxy.
	ConfDir string `yaml:"conf_dir"`

	// UpstreamHost is the address Nginx uses to reach app containers.
	// It is 127.0.0.1 when Nginx runs on the host, and
	// host.docker.internal when Nginx itself runs in a container.
	UpstreamHost string `yaml:"upstream_host"`

	// TestCommand validates the configuration without applying it.
	TestCommand []string `yaml:"test_command"`
	// ReloadCommand makes Nginx pick up the configuration.
	ReloadCommand []string `yaml:"reload_command"`

	// ProbeAddr is the host:port on which liman can reach Nginx's HTTP
	// listener. After a reload liman asks Nginx itself which release it is
	// serving, and only then removes the previous one.
	ProbeAddr string `yaml:"probe_addr"`

	// CertDir holds one directory per domain containing fullchain.pem and
	// privkey.pem. A domain with both files is served over HTTPS. The path
	// must be the same for liman and for Nginx.
	CertDir string `yaml:"cert_dir"`

	// ACMERoot, when set, is served under /.well-known/acme-challenge/ so
	// an ACME client can prove ownership of the domain.
	ACMERoot string `yaml:"acme_root"`
}

// Enabled reports whether liman should manage Nginx at all.
func (n Nginx) Enabled() bool {
	return n.ConfDir != ""
}

// Load reads the configuration at path. A missing file is not an error: it
// yields the zero Config, which disables every optional feature.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		var cfg Config
		cfg.applyDefaults()
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg Config
	// An empty file makes Decode return io.EOF; that is an empty config,
	// not a problem.
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%s: parse yaml: %w", path, err)
	}

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// applyDefaults fills in the values that suit an Nginx installed directly
// on the host, which is the production setup.
func (c *Config) applyDefaults() {
	if c.Daemon.Listen == "" {
		c.Daemon.Listen = DefaultListen
	}

	n := &c.Nginx
	if !n.Enabled() {
		return
	}
	if n.UpstreamHost == "" {
		n.UpstreamHost = "127.0.0.1"
	}
	if n.ProbeAddr == "" {
		n.ProbeAddr = "127.0.0.1:80"
	}
	if len(n.TestCommand) == 0 {
		n.TestCommand = []string{"nginx", "-t"}
	}
	if len(n.ReloadCommand) == 0 {
		n.ReloadCommand = []string{"nginx", "-s", "reload"}
	}
}

func (c *Config) validate() error {
	var errs []error

	if err := CheckListenAddress(c.Daemon.Listen); err != nil {
		errs = append(errs, fmt.Errorf("daemon.listen: %w", err))
	}

	if c.Backup.Dir != "" && !filepath.IsAbs(c.Backup.Dir) {
		errs = append(errs, fmt.Errorf("backup.dir: must be an absolute path, got %q", c.Backup.Dir))
	}
	if k := c.Backup.Keep; k != nil && *k < 0 {
		errs = append(errs, fmt.Errorf("backup.keep: must not be negative, got %d", *k))
	}
	if c.Secrets.KeyFile != "" && !filepath.IsAbs(c.Secrets.KeyFile) {
		errs = append(errs, fmt.Errorf("secrets.key_file: must be an absolute path, got %q", c.Secrets.KeyFile))
	}

	if d := c.Deploy.Drain; d != nil && (time.Duration(*d) < 0 || time.Duration(*d) > 10*time.Minute) {
		errs = append(errs, fmt.Errorf("deploy.drain: must be between 0s and 10m, got %s", time.Duration(*d)))
	}

	// Zero means "use the default" for both; anything else must make sense.
	// A very short interval would hammer the apps and Docker for nothing.
	if iv := time.Duration(c.Monitor.Interval); iv != 0 && iv < time.Second {
		errs = append(errs, fmt.Errorf("monitor.interval: must be at least 1s, got %s", iv))
	}
	if c.Monitor.FailureThreshold < 0 {
		errs = append(errs, fmt.Errorf("monitor.failure_threshold: must not be negative, got %d", c.Monitor.FailureThreshold))
	}

	errs = append(errs, c.validateACME()...)

	n := c.Nginx
	if !n.Enabled() {
		return errors.Join(errs...)
	}

	// The paths end up inside the generated Nginx config, where a relative
	// path would be resolved against Nginx's own directory, not ours.
	// A slice of anonymous structs keeps the checks in a fixed order; a map
	// would report the errors in a different order on every run.
	paths := []struct{ name, value string }{
		{"conf_dir", n.ConfDir},
		{"cert_dir", n.CertDir},
		{"acme_root", n.ACMERoot},
	}
	for _, p := range paths {
		if p.value != "" && !filepath.IsAbs(p.value) {
			errs = append(errs, fmt.Errorf("nginx.%s: must be an absolute path, got %q", p.name, p.value))
		}
	}
	return errors.Join(errs...)
}

func (c *Config) validateACME() []error {
	a := c.ACME
	if !a.Enabled() {
		return nil
	}
	var errs []error
	// The account key and every request are sent to this address; over
	// plain HTTP anyone in between could answer as the CA.
	if u, err := url.Parse(a.Directory); err != nil || u.Scheme != "https" || u.Host == "" {
		errs = append(errs, fmt.Errorf("acme.directory: must be an https:// URL, got %q", a.Directory))
	}
	if a.Email != "" && (!strings.Contains(a.Email, "@") || strings.ContainsAny(a.Email, " \t\r\n,;<>")) {
		errs = append(errs, fmt.Errorf("acme.email: %q is not an email address", a.Email))
	}
	for _, p := range []struct{ name, value string }{{"ca_file", a.CAFile}, {"account_key", a.AccountKey}} {
		if p.value != "" && !filepath.IsAbs(p.value) {
			errs = append(errs, fmt.Errorf("acme.%s: must be an absolute path, got %q", p.name, p.value))
		}
	}
	// Without these there is nowhere to put the challenge for the CA to
	// fetch, or the certificate it sends back.
	switch {
	case !c.Nginx.Enabled():
		errs = append(errs, errors.New("acme: needs nginx.conf_dir; certificates are only used through Nginx"))
	case c.Nginx.CertDir == "" || c.Nginx.ACMERoot == "":
		errs = append(errs, errors.New("acme: needs nginx.cert_dir (where certificates are written) and nginx.acme_root (where Nginx serves the CA's challenges)"))
	}
	return errs
}

// Home returns the directory liman keeps its files in: $LIMAN_HOME if set,
// otherwise ~/.liman.
func Home() string {
	if dir := os.Getenv("LIMAN_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home directory (unusual, but possible in minimal containers):
		// fall back to the current directory rather than refusing to run.
		return ".liman"
	}
	return filepath.Join(home, ".liman")
}

// Client is how the liman CLI reaches a daemon. It lives in its own file,
// client.yaml, because it belongs to the machine commands are typed on,
// which is usually not the machine the daemon runs on.
type Client struct {
	// URL is the base address of the daemon, e.g. http://127.0.0.1:7070.
	URL string `yaml:"url"`
	// Token authenticates the client.
	Token string `yaml:"token"`
}

// LoadClient reads client.yaml at path and then applies the environment
// variables LIMAN_URL and LIMAN_TOKEN, which take precedence. A missing
// file is not an error.
func LoadClient(path string) (Client, error) {
	var c Client

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Environment variables alone may be enough.
	case err != nil:
		return Client{}, fmt.Errorf("read client config: %w", err)
	default:
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			return Client{}, fmt.Errorf("%s: parse yaml: %w", path, err)
		}
	}

	if v := os.Getenv("LIMAN_URL"); v != "" {
		c.URL = v
	}
	if v := os.Getenv("LIMAN_TOKEN"); v != "" {
		c.Token = v
	}
	if c.URL == "" {
		c.URL = "http://" + DefaultListen
	}
	if err := CheckClientURL(c.URL); err != nil {
		return Client{}, fmt.Errorf("daemon URL: %w", err)
	}
	return c, nil
}

// SaveClient writes c to path, readable by the owner only because it
// contains the token.
func SaveClient(path string, c Client) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create client config directory: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode client config: %w", err)
	}
	// The token is written to a new file that is then renamed into place.
	// Writing into a file that is already there would be writing through
	// whatever someone put there: in a shared directory another user could
	// create the file first and stay its owner, and the token would land
	// in a file they can read. CreateTemp makes a file that did not exist,
	// with mode 0600; Rename replaces the name, not the content.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".client-*.yaml")
	if err != nil {
		return fmt.Errorf("write client config: %w", err)
	}
	_, writeErr := tmp.Write(data)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write client config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write client config: %w", err)
	}
	return nil
}
