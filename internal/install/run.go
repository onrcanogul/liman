package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onrcanogul/liman/internal/config"
)

// Options are what Run works with.
type Options struct {
	// Binary is the limand to install.
	Binary string
	Flags  Flags
	// Ask asks the person installing; nil when nobody can be asked.
	Ask    Asker
	Stdout io.Writer
	Stderr io.Writer
	// Run runs a command, Look finds a program on PATH ("" if not there).
	Run  Runner
	Look func(string) string
}

// installer carries what one run has found and done.
type installer struct {
	Options
	ctx context.Context

	choices     Choices
	proxyBinary string
	upgrade     bool
	oldVersion  string
	newVersion  string
	wroteConfig bool
}

// Run installs, or upgrades, liman on this machine. Every step can be run
// again: nothing that exists is overwritten, except the binary and the
// units. Nothing is changed before every choice is settled and checked.
func Run(ctx context.Context, o Options) error {
	in := &installer{Options: o, ctx: ctx}
	if os.Geteuid() != 0 {
		return errors.New("run this as root (sudo)")
	}
	if _, err := os.Stat(o.Binary); err != nil {
		return fmt.Errorf("%s: %w", o.Binary, err)
	}
	if o.Look("systemctl") == "" {
		return errors.New("systemd is required")
	}
	// Run the binary before anything is changed: a file built for another
	// architecture fails here, with a clear message, instead of in the
	// unit.
	if _, err := in.Run(ctx, o.Binary, "version"); err != nil {
		return fmt.Errorf("%s does not run on this machine (wrong architecture?)", o.Binary)
	}

	if err := in.settle(); err != nil {
		return err
	}
	if err := in.check(); err != nil {
		return err
	}
	for _, step := range []func() error{in.user, in.binary, in.state, in.engine, in.proxy, in.daemonUnit, in.backups} {
		if err := step(); err != nil {
			return err
		}
	}
	in.next()
	return nil
}

func (in *installer) step(format string, args ...any) {
	fmt.Fprintf(in.Stdout, "==> "+format+"\n", args...)
}

func (in *installer) detail(format string, args ...any) {
	fmt.Fprintf(in.Stdout, "    "+format+"\n", args...)
}

func (in *installer) cmd(name string, args ...string) error {
	out, err := in.Run(in.ctx, name, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

// settle decides what to install. On a machine that has a config.yaml,
// that is what it says: an upgrade asks nothing.
func (in *installer) settle() error {
	path := filepath.Join(StateDir, "config.yaml")
	if _, err := os.Stat(path); err == nil {
		cfg, err := config.Load(path)
		if err != nil {
			return fmt.Errorf("%s exists but cannot be read: %w", path, err)
		}
		in.choices = Choices{Engine: cfg.Engine.Driver, Proxy: cfg.Proxy.Driver, Listen: cfg.Daemon.Listen, HooksListen: cfg.Hooks.Listen}
		if in.choices.Engine == "" {
			in.choices.Engine = "docker"
		}
		if !cfg.Proxy.Enabled() {
			in.choices.Proxy = "none"
		}
		f := in.Flags
		if (f.Engine != "" && f.Engine != in.choices.Engine) || (f.Proxy != "" && f.Proxy != in.choices.Proxy) {
			fmt.Fprintf(in.Stderr, "Note: %s exists, and says engine %s and proxy %s; those stay. To choose again, move it away first.\n", path, in.choices.Engine, in.choices.Proxy)
		}
		if f.HooksListen != "" && cfg.Hooks.Listen == "" {
			fmt.Fprintf(in.Stderr, "Note: --hooks-listen is not applied to the existing %s: add\n  hooks:\n    listen: %s\nand run systemctl restart limand.\n", path, f.HooksListen)
		}
		in.proxyBinary = in.Look(in.choices.Proxy)
		return nil
	}

	facts := Gather(in.ctx, in.Run, in.Look)
	if in.Ask != nil && !in.Flags.Yes {
		in.describe(facts)
	}
	c, err := Choose(facts, in.Flags, in.Ask)
	if err != nil {
		return err
	}
	in.choices = c
	if t, ok := facts.Proxies[c.Proxy]; ok {
		in.proxyBinary = t.Path
	}
	hooks := "no trigger endpoint"
	if c.HooksListen != "" {
		hooks = "trigger endpoint on " + c.HooksListen
	}
	fmt.Fprintf(in.Stdout, "\nInstalling: %s, proxy %s, API on %s, %s.\nThe same without questions:\n  sudo limand install --binary %s %s\n\n",
		c.Engine, c.Proxy, c.Listen, hooks, in.Binary, c.Flags())
	return nil
}

// describe says what was found, before the questions.
func (in *installer) describe(f Facts) {
	state := func(t Tool, on string) string {
		switch {
		case !t.Installed():
			return "not installed"
		case t.Active:
			return on
		}
		return "installed, not running"
	}
	fmt.Fprintln(in.Stdout, "Looking at this server:")
	fmt.Fprintf(in.Stdout, "  Docker  %s\n  Podman  %s\n", state(f.Docker, "running"), state(f.Podman, "API socket on"))
	for _, name := range ProxyNames {
		s := state(f.Proxies[name], "running")
		if f.Port80 == name {
			s = "on port 80"
		}
		fmt.Fprintf(in.Stdout, "  %-7s %s\n", name, s)
	}
	fmt.Fprintln(in.Stdout)
}

// check refuses, before anything is changed, a choice that cannot work.
func (in *installer) check() error {
	c := in.choices
	if c.Engine == "docker" {
		if _, err := in.Run(in.ctx, "getent", "group", "docker"); err != nil {
			return errors.New("there is no docker group; is Docker installed from a package?")
		}
	}
	if c.Proxy == "none" {
		return nil
	}
	if in.proxyBinary == "" {
		return fmt.Errorf("%s is not installed", c.Proxy)
	}
	p, _ := proxyFor(c.Proxy, in.proxyBinary)
	// liman has the proxy load every deploy, and a stopped one cannot:
	// the first deploy would fail and be undone.
	if out, _ := in.Run(in.ctx, "systemctl", "is-active", strings.TrimSuffix(p.Unit, ".service")); strings.TrimSpace(out) != "active" {
		return fmt.Errorf("%s is installed but not running; start it with: systemctl enable --now %s", p.Name, strings.TrimSuffix(p.Unit, ".service"))
	}
	if len(p.Sudo) > 0 && in.Look("sudo") == "" {
		return fmt.Errorf("sudo is required for reloading %s", p.Name)
	}
	switch c.Proxy {
	case "nginx":
		if _, err := os.Stat("/etc/nginx/conf.d"); err != nil {
			return errors.New("/etc/nginx/conf.d does not exist; this Nginx is laid out differently")
		}
	case "caddy":
		if _, err := os.Stat(Caddyfile); err != nil {
			return fmt.Errorf("%s does not exist; this Caddy is set up differently", Caddyfile)
		}
	case "traefik":
		// The person's static configuration is not edited: it is looked
		// at, and what to add is said.
		for _, f := range TraefikStaticFiles {
			if data, err := os.ReadFile(f); err == nil && TraefikWatches(data, p.ConfDir) {
				return nil
			}
		}
		return errors.New(TraefikInstructions(p.ConfDir))
	}
	return nil
}

func (in *installer) user() error {
	in.step("User liman")
	if _, err := user.Lookup("liman"); err == nil {
		in.detail("exists")
	} else {
		// --system: no password, no login, a low uid. Its home is the
		// state directory, so that "docker login" or "podman login" run
		// as this user stores credentials where the daemon looks.
		if err := in.cmd("useradd", "--system", "--home-dir", StateDir, "--shell", "/usr/sbin/nologin", "liman"); err != nil {
			return err
		}
		in.detail("created")
	}
	if in.choices.Engine == "docker" {
		// Also for a user that already existed.
		return in.cmd("usermod", "-aG", "docker", "liman")
	}
	return nil
}

func (in *installer) binary() error {
	in.step("Binary %s", BinPath)
	newData, err := os.ReadFile(in.Binary)
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(BinPath); err == nil && !bytes.Equal(old, newData) {
		in.upgrade = true
		in.oldVersion = in.version(BinPath)
		// One previous binary is kept: the one to go back to.
		if err := writeFile(PrevPath, old, 0o755, ""); err != nil {
			return err
		}
		in.detail("kept the running one as %s (%s)", PrevPath, in.oldVersion)
	}
	// Written to a temporary name and renamed, so a running daemon keeps
	// its old file until it is restarted.
	if err := writeFile(BinPath, newData, 0o755, ""); err != nil {
		return err
	}
	in.newVersion = in.version(BinPath)
	in.detail("%s", in.newVersion)
	return nil
}

// version is the first line of "limand version".
func (in *installer) version(bin string) string {
	out, _ := in.Run(in.ctx, bin, "version")
	first, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(first)
}

func (in *installer) state() error {
	in.step("State directory %s", StateDir)
	if err := mkdir(StateDir, 0o700, "liman"); err != nil {
		return err
	}
	path := filepath.Join(StateDir, "config.yaml")
	if _, err := os.Stat(path); err == nil {
		in.detail("config.yaml exists, left as it is")
		return nil
	}
	data, err := ConfigYAML(in.choices, in.proxyBinary)
	if err != nil {
		return err
	}
	if err := writeFile(path, data, 0o600, "liman"); err != nil {
		return err
	}
	in.wroteConfig = true
	in.detail("wrote config.yaml")
	return nil
}

func (in *installer) engine() error {
	if in.choices.Engine != "podman" {
		return nil
	}
	in.step("Podman's API socket, for the liman group")
	if err := writeFile(PodmanSocketDropIn, []byte(PodmanSocketConf()), 0o644, ""); err != nil {
		return err
	}
	if err := writeFile(PodmanTmpfiles, []byte(PodmanTmpfilesConf()), 0o644, ""); err != nil {
		return err
	}
	for _, c := range [][]string{
		{"systemd-tmpfiles", "--create", PodmanTmpfiles},
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "--now", "podman.socket"},
		{"systemctl", "restart", "podman.socket"},
	} {
		if err := in.cmd(c[0], c[1:]...); err != nil {
			return err
		}
	}
	in.detail("/run/podman/podman.sock, group liman")
	return nil
}

func (in *installer) proxy() error {
	c := in.choices
	p, ok := proxyFor(c.Proxy, in.proxyBinary)
	if !ok {
		return nil
	}
	in.step("%s", p.Name)
	// liman owns one directory and writes one file per app into it.
	if err := mkdir(p.ConfDir, 0o755, "liman"); err != nil {
		return err
	}
	in.detail("%s", p.ConfDir)
	switch c.Proxy {
	case "nginx":
		// The only change to the existing Nginx setup is one line that
		// includes it, in a file of its own.
		incl := "/etc/nginx/conf.d/liman.conf"
		if _, err := os.Stat(incl); err == nil {
			in.detail("%s exists, left as it is", incl)
		} else {
			if err := writeFile(incl, []byte("# Apps deployed by liman. Written by limand install.\ninclude "+p.ConfDir+"/*.conf;\n"), 0o644, ""); err != nil {
				return err
			}
			in.detail("wrote %s", incl)
		}
		if err := in.cmd(in.proxyBinary, "-t"); err != nil {
			return fmt.Errorf("nginx -t fails; fix the Nginx configuration and run this again: %w", err)
		}
	case "caddy":
		if err := in.caddyImport(p.ConfDir); err != nil {
			return err
		}
	}
	if rule := SudoersRule(c.Proxy, in.proxyBinary); rule != "" {
		in.step("sudo rule %s", Sudoers)
		// Checked before it is put in place: a syntax error in a sudoers
		// file locks everybody out of sudo.
		tmp, err := os.CreateTemp("", "liman-sudoers-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(tmp.Name()) }()
		_, werr := tmp.WriteString(rule)
		if err := errors.Join(werr, tmp.Close()); err != nil {
			return err
		}
		if err := in.cmd("visudo", "-cf", tmp.Name()); err != nil {
			return fmt.Errorf("the sudo rule did not pass visudo: %w", err)
		}
		if err := writeFile(Sudoers, []byte(rule), 0o440, ""); err != nil {
			return err
		}
		in.detail("installed")
	}
	return nil
}

// caddyImport adds liman's line to the Caddyfile: kept as it was, the
// line added, checked with caddy validate, and put back if that fails.
func (in *installer) caddyImport(dir string) error {
	before, err := os.ReadFile(Caddyfile)
	if err != nil {
		return err
	}
	after, added := AddCaddyImport(before, dir)
	if !added {
		in.detail("%s imports it already", Caddyfile)
		return nil
	}
	backup := Caddyfile + ".before-liman"
	if err := writeFile(backup, before, 0o644, ""); err != nil {
		return err
	}
	if err := writeFile(Caddyfile, after, 0o644, ""); err != nil {
		return err
	}
	err = in.cmd(in.proxyBinary, "validate", "--config", Caddyfile, "--adapter", "caddyfile")
	if err == nil {
		err = in.cmd("systemctl", "reload", "caddy")
	}
	if err != nil {
		if rerr := writeFile(Caddyfile, before, 0o644, ""); rerr != nil {
			return fmt.Errorf("%w (and putting %s back failed: %v; the old one is %s)", err, Caddyfile, rerr, backup)
		}
		return fmt.Errorf("the Caddyfile with liman's import did not pass; it is as it was: %w", err)
	}
	in.detail("added to %s: %s (the one before is %s)", Caddyfile, caddyImport(dir), backup)
	return nil
}

// The line the daemon logs once it listens, and the one that names the
// backup it took before upgrading the database.
var (
	startedRe = regexp.MustCompile(`msg="limand started".* version=(\S+) `)
	backupRe  = regexp.MustCompile(`before upgrading it.* backup=(\S+)`)
)

func (in *installer) daemonUnit() error {
	in.step("systemd unit")
	unit, err := DaemonUnit(in.choices.Engine, in.choices.Proxy)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(UnitDir, "limand.service"), unit, 0o644, ""); err != nil {
		return err
	}
	if err := in.cmd("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := in.cmd("systemctl", "enable", "limand"); err != nil {
		return err
	}

	before, want := in.schema(BinPath)
	if before > 0 && want > before {
		in.detail("the database has schema %d, this limand uses %d: it is backed up, then upgraded, as the daemon starts", before, want)
	}

	// restart rather than start: on an upgrade the new binary must be
	// picked up.
	started := time.Now().Add(-time.Second)
	if err := in.cmd("systemctl", "restart", "limand"); err != nil {
		return err
	}

	// "active" only says that systemd started a process. The daemon logs
	// "limand started" with its version once it listens; wait for that
	// line from this start, from this version.
	tag := versionTag(in.newVersion)
	up := false
	var log string
	for range 15 {
		active, _ := in.Run(in.ctx, "systemctl", "is-active", "limand")
		log, _ = in.Run(in.ctx, "journalctl", "-u", "limand", "--since", "@"+strconv.FormatInt(started.Unix(), 10), "-o", "cat", "--no-pager")
		if strings.TrimSpace(active) == "active" {
			if m := startedRe.FindStringSubmatch(log); m != nil && m[1] == tag {
				up = true
				break
			}
		}
		time.Sleep(time.Second)
	}
	backup := ""
	if m := backupRe.FindAllStringSubmatch(log, -1); len(m) > 0 {
		backup = m[len(m)-1][1]
	}

	if up {
		in.detail("limand is running (%s)", tag)
		if in.upgrade {
			in.detail("upgraded from: %s", in.oldVersion)
			if backup != "" {
				in.detail("the database was upgraded; backup from before: %s", backup)
				fmt.Fprint(in.Stdout, indent(in.wayBack(backup), "    "))
			} else {
				in.detail("the database did not need upgrading; to go back: install -m 0755 %s %s && systemctl restart limand", PrevPath, BinPath)
			}
		}
		return nil
	}

	fmt.Fprintln(in.Stderr, lastLines(log, 20))
	// Do not leave it restarting every five seconds.
	_ = in.cmd("systemctl", "stop", "limand")
	if in.upgrade {
		if now, _ := in.schema(BinPath); before > 0 && now == before {
			// The database is as the previous version left it, so that
			// version can simply run again.
			prev, err := os.ReadFile(PrevPath)
			if err == nil {
				err = writeFile(BinPath, prev, 0o755, "")
			}
			if err == nil {
				err = in.cmd("systemctl", "start", "limand")
			}
			if err != nil {
				return fmt.Errorf("%s did not start, and putting %s back failed: %w", tag, in.oldVersion, err)
			}
			return fmt.Errorf("%s did not start; its log is above. The database was not changed, and the previous version (%s) is back and running", tag, in.oldVersion)
		}
		return fmt.Errorf("%s upgraded the database but did not start; its log is above.\n  Fix the cause and run: systemctl restart limand\n%s", tag, in.wayBack(backup))
	}
	if in.wroteConfig {
		_ = os.Remove(filepath.Join(StateDir, "config.yaml"))
		return errors.New("limand did not stay up; its last log lines are above. The config.yaml this run wrote was removed: fix the cause and run this again")
	}
	return fmt.Errorf("limand did not stay up; its last log lines are above. Fix %s, then: systemctl restart limand", filepath.Join(StateDir, "config.yaml"))
}

// schema is the database's schema version and the one bin uses, read as
// the liman user; 0 when it cannot be read.
func (in *installer) schema(bin string) (have, want int) {
	out, err := in.Run(in.ctx, "runuser", "-u", "liman", "--", "env", "LIMAN_HOME="+StateDir, bin, "version", "--schema")
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(out)
	if len(fields) >= 2 {
		have, _ = strconv.Atoi(fields[0])
		want, _ = strconv.Atoi(fields[1])
	}
	return have, want
}

// wayBack says how to return to the previous version after the database
// was upgraded: the previous binary, and the backup the daemon took.
func (in *installer) wayBack(backup string) string {
	if backup == "" {
		backup = "<the backup named in: journalctl -u limand | grep 'before upgrading'>"
	}
	return fmt.Sprintf("To go back to %s:\n  systemctl stop limand\n  install -m 0755 %s %s\n  runuser -u liman -- env LIMAN_HOME=%s limand backup restore %s\n  systemctl start limand\n"+
		"Whatever was written after the upgrade (history, secrets, tokens) is lost that way. Settings only the new version knows must leave config.yaml first.\n",
		in.oldVersion, PrevPath, BinPath, StateDir, backup)
}

func (in *installer) backups() error {
	in.step("Daily backup")
	if err := mkdir(BackupDir, 0o700, "liman"); err != nil {
		return err
	}
	service, timer, err := BackupUnits()
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(UnitDir, "limand-backup.service"), service, 0o644, ""); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(UnitDir, "limand-backup.timer"), timer, 0o644, ""); err != nil {
		return err
	}
	if err := in.cmd("systemctl", "daemon-reload"); err != nil {
		return err
	}
	// A timer is enabled and started like a service; the service it
	// triggers is not enabled itself.
	if err := in.cmd("systemctl", "enable", "--now", "limand-backup.timer"); err != nil {
		return err
	}
	in.detail("%s, the newest 14 are kept", BackupDir)
	return nil
}

func (in *installer) next() {
	if in.upgrade {
		fmt.Fprintf(in.Stdout, "\nUpgraded to %s.\n", in.newVersion)
		return
	}
	c := in.choices
	hooks := ""
	if c.HooksListen != "" {
		hooks = fmt.Sprintf("\n  5. Deploys from CI arrive at %s. Make /hooks/ reachable there over\n"+
			"     HTTPS, then register each app (README, Deploying from CI):\n"+
			"       sudo -u liman env LIMAN_HOME=%s limand app create <name> --repo <repository> --domain <domain>\n", c.HooksListen, StateDir)
	}
	fmt.Fprintf(in.Stdout, `
Done. Next:

  1. Create a token for your machine (it is shown once):
       sudo -u liman env LIMAN_HOME=%[1]s limand token create laptop

  2. On your machine, in ~/.liman/client.yaml:
       url: http://%[2]s
       token: <the token>

  3. Copy %[1]s/master.key to somewhere that is not this server, once.
     It is not in the backups, and they are useless without it.

  4. The backups in %[3]s are on this server's disk. Copy them
     elsewhere regularly (rsync, restic, ...): a backup that dies with the
     disk it protects is only half of one.
%[4]s
  Logs:    journalctl -u limand -f
  Status:  systemctl status limand
  Backups: systemctl list-timers limand-backup
`, StateDir, c.Listen, BackupDir, hooks)
}

// versionTag is the version in "liman v1.2.3 (commit …)".
func versionTag(line string) string {
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		return fields[1]
	}
	return ""
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func indent(s, prefix string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(s, "\n") {
		if l != "" {
			b.WriteString(prefix + l)
		}
	}
	return b.String()
}

// writeFile replaces path atomically, with perm, owned by owner ("" for
// root).
func writeFile(path string, data []byte, perm fs.FileMode, owner string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Close()); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	if err := chown(tmp.Name(), owner); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// mkdir makes dir with perm, owned by owner, or sets both on one that
// exists.
func mkdir(dir string, perm fs.FileMode, owner string) error {
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	if err := os.Chmod(dir, perm); err != nil {
		return err
	}
	return chown(dir, owner)
}

func chown(path, owner string) error {
	if owner == "" {
		return nil
	}
	u, err := user.Lookup(owner)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return os.Chown(path, uid, gid)
}
