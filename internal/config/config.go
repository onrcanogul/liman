// Package config loads liman's own configuration: settings that describe
// the machine liman runs on, as opposed to app.yaml, which describes an app.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Config is the content of config.yaml in liman's home directory.
type Config struct {
	Nginx Nginx `yaml:"nginx"`
}

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
		return Config{}, nil
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
	n := c.Nginx
	if !n.Enabled() {
		return nil
	}

	var errs []error
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
