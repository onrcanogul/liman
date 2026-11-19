package nginx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/config"
)

// Runner executes an external command and returns its combined output.
// It is a type so tests can substitute a function that runs nothing.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Manager writes site files and drives the Nginx process.
type Manager struct {
	cfg config.Nginx
	run Runner

	// These control WaitRelease. They are fields so tests can shorten them.
	client       *http.Client
	waitTimeout  time.Duration
	waitInterval time.Duration
}

// New returns a Manager that runs real commands.
func New(cfg config.Nginx) *Manager {
	return &Manager{
		cfg: cfg,
		run: execRunner,
		client: &http.Client{
			Timeout: 2 * time.Second,
			// The probe wants Nginx's own answer. Following an HTTP to
			// HTTPS redirect would leave the listener being probed.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		waitTimeout:  10 * time.Second,
		waitInterval: 100 * time.Millisecond,
	}
}

// execRunner runs the command directly, without a shell. The arguments are
// passed to the program as they are, so nothing in them can be interpreted
// as shell syntax.
func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	// CommandContext kills the process if ctx is cancelled.
	// CombinedOutput captures stdout and stderr together; nginx writes its
	// diagnostics to stderr.
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Path returns the file that holds the configuration of app.
func (m *Manager) Path(app string) string {
	return filepath.Join(m.cfg.ConfDir, app+".conf")
}

// Site builds the template input for an app. upstreamPort is the host port
// of the app's container; 0 means "not known yet" and renders a placeholder,
// which is what a dry run shows.
func (m *Manager) Site(app, domain string, upstreamPort int, release string) Site {
	port := "<free port>"
	if upstreamPort != 0 {
		port = fmt.Sprint(upstreamPort)
	}

	site := Site{
		App:      app,
		Domain:   domain,
		Upstream: m.cfg.UpstreamHost + ":" + port,
		ACMERoot: m.cfg.ACMERoot,
		Release:  release,
	}

	// HTTPS is enabled by the presence of the certificate files. liman
	// does not obtain certificates yet; it uses what it finds.
	if m.cfg.CertDir != "" {
		cert := filepath.Join(m.cfg.CertDir, domain, "fullchain.pem")
		key := filepath.Join(m.cfg.CertDir, domain, "privkey.pem")
		if fileExists(cert) && fileExists(key) {
			site.CertFile, site.KeyFile = cert, key
		}
	}
	return site
}

// Render returns the configuration for an app whose container listens on
// upstreamPort (0 renders a placeholder).
func (m *Manager) Render(app, domain string, upstreamPort int, release string) ([]byte, error) {
	return Render(m.Site(app, domain, upstreamPort, release))
}

// The quoted value of the add_header directive written by the template.
var releaseRe = regexp.MustCompile(`add_header X-Liman-Release "([^"]*)"`)

// ReleaseOf returns the release recorded in a config written by Render, or
// "" when content is empty or carries none.
func (m *Manager) ReleaseOf(content []byte) string {
	return ReleaseOf(content)
}

// ReleaseOf is the package-level form of Manager.ReleaseOf.
func ReleaseOf(content []byte) string {
	// FindSubmatch returns the whole match followed by the parenthesised
	// groups, or nil when nothing matches.
	if m := releaseRe.FindSubmatch(content); m != nil {
		return string(m[1])
	}
	return ""
}

// WaitRelease blocks until Nginx answers requests for domain with the given
// release, or until the wait times out. An empty release means "no liman
// config is serving this domain".
//
// It exists because a reload is asynchronous: the reload command returns as
// soon as the signal is delivered, while Nginx starts its new workers a
// moment later. Removing the old container in that gap makes Nginx answer
// with 502.
func (m *Manager) WaitRelease(ctx context.Context, domain, release string) error {
	ctx, cancel := context.WithTimeout(ctx, m.waitTimeout)
	defer cancel()

	var last string
	for {
		got, err := m.servedRelease(ctx, domain)
		if err == nil && got == release {
			return nil
		}
		// Remember why this attempt did not succeed, for the timeout
		// message. An attempt cut short by our own deadline says nothing
		// about Nginx, so it must not overwrite a real observation.
		switch {
		case ctx.Err() != nil:
		case err != nil:
			last = err.Error()
		default:
			last = fmt.Sprintf("it serves release %q", got)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("nginx did not switch %s to release %q within %s: %s", domain, release, m.waitTimeout, last)
		case <-time.After(m.waitInterval):
		}
	}
}

// servedRelease asks Nginx which release it serves for domain.
func (m *Manager) servedRelease(ctx context.Context, domain string) (string, error) {
	// HEAD asks for the headers only; the body is of no interest.
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://"+m.cfg.ProbeAddr+"/", nil)
	if err != nil {
		return "", err
	}
	// The connection goes to ProbeAddr, but the Host header carries the
	// app's domain. That header is what Nginx matches server_name against.
	req.Host = domain

	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.Header.Get(ReleaseHeader), nil
}

// checkName refuses names that are not valid app names.
//
// The name becomes part of a file path. A name such as "../../nginx" would
// make the methods below read, overwrite or delete files outside liman's
// directory. Callers validate names too; this check is here so that the
// package that touches the file system does not depend on every caller
// remembering to.
func checkName(name string) error {
	if !app.ValidName(name) {
		return fmt.Errorf("invalid app name %q", name)
	}
	return nil
}

// Current returns the configuration currently on disk for app, or nil if
// there is none.
func (m *Manager) Current(app string) ([]byte, error) {
	if err := checkName(app); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(m.Path(app))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read nginx config: %w", err)
	}
	return data, nil
}

// Write replaces the configuration of app atomically: Nginx either sees the
// complete old file or the complete new one, never a half-written file.
func (m *Manager) Write(app string, content []byte) error {
	if err := checkName(app); err != nil {
		return err
	}
	if err := os.MkdirAll(m.cfg.ConfDir, 0o755); err != nil {
		return fmt.Errorf("create nginx config directory: %w", err)
	}

	// The temporary file must be in the same directory as the target:
	// rename is only atomic within one filesystem. Its name starts with a
	// dot and does not end in .conf, so Nginx's "include *.conf" skips it.
	tmp, err := os.CreateTemp(m.cfg.ConfDir, "."+app+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary nginx config: %w", err)
	}
	// If anything below fails, do not leave the temporary file behind.
	// After a successful rename this Remove fails harmlessly.
	defer func() { _ = os.Remove(tmp.Name()) }()

	_, writeErr := tmp.Write(content)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write nginx config: %w", err)
	}

	// CreateTemp creates files readable only by the owner; Nginx usually
	// runs as a different user and must be able to read the config.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("set nginx config permissions: %w", err)
	}

	if err := os.Rename(tmp.Name(), m.Path(app)); err != nil {
		return fmt.Errorf("install nginx config: %w", err)
	}
	return nil
}

// Remove deletes the configuration of app. Removing a file that does not
// exist is not an error, which makes Remove safe to repeat.
func (m *Manager) Remove(app string) error {
	if err := checkName(app); err != nil {
		return err
	}
	err := os.Remove(m.Path(app))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove nginx config: %w", err)
	}
	return nil
}

// Restore puts back a previous state: content is written, or the file is
// removed when content is nil (meaning there was no file before).
func (m *Manager) Restore(app string, content []byte) error {
	if content == nil {
		return m.Remove(app)
	}
	return m.Write(app, content)
}

// Test asks Nginx to validate the configuration on disk without applying
// it.
func (m *Manager) Test(ctx context.Context) error {
	return m.command(ctx, "nginx config test", m.cfg.TestCommand)
}

// Reload makes Nginx apply the configuration on disk. Running requests are
// finished by the old worker processes; new ones go to workers with the new
// configuration.
func (m *Manager) Reload(ctx context.Context) error {
	return m.command(ctx, "nginx reload", m.cfg.ReloadCommand)
}

// TestCommand returns the configured test command as text, for showing the
// manual equivalent of a step.
func (m *Manager) TestCommand() string { return strings.Join(m.cfg.TestCommand, " ") }

// ReloadCommand returns the configured reload command as text.
func (m *Manager) ReloadCommand() string { return strings.Join(m.cfg.ReloadCommand, " ") }

func (m *Manager) command(ctx context.Context, what string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("%s: no command configured", what)
	}

	// argv[1:]... spreads the slice into the variadic parameter.
	out, err := m.run(ctx, argv[0], argv[1:]...)
	if err != nil {
		// The command's own output explains the failure far better than
		// "exit status 1" does, so it becomes part of the error.
		return fmt.Errorf("%s failed: %w\n%s", what, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
