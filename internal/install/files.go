package install

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"
)

// Where things go on the server.
const (
	StateDir  = "/var/lib/liman"
	BackupDir = "/var/backups/liman"
	BinPath   = "/usr/local/bin/limand"
	PrevPath  = "/usr/local/bin/limand.previous"
	UnitDir   = "/etc/systemd/system"
	Sudoers   = "/etc/sudoers.d/liman"
	Caddyfile = "/etc/caddy/Caddyfile"
)

// proxySetup is what each proxy needs, besides its dialect.
type proxySetup struct {
	Name    string // as people write it
	Unit    string // its systemd unit
	ConfDir string // the directory liman writes into
	// Sudo are the commands liman may run as root: the reload, and for
	// Nginx its test (the others test as the liman user, or need none).
	Sudo [][]string
}

func proxyFor(name, binary string) (proxySetup, bool) {
	switch name {
	case "nginx":
		return proxySetup{Name: "Nginx", Unit: "nginx.service", ConfDir: "/etc/nginx/liman.d",
			Sudo: [][]string{{binary, "-t"}, {"/usr/bin/systemctl", "reload", "nginx"}}}, true
	case "caddy":
		return proxySetup{Name: "Caddy", Unit: "caddy.service", ConfDir: "/etc/caddy/liman.d",
			Sudo: [][]string{{"/usr/bin/systemctl", "reload", "caddy"}}}, true
	case "traefik":
		return proxySetup{Name: "Traefik", Unit: "traefik.service", ConfDir: "/etc/traefik/liman.d"}, true
	}
	return proxySetup{}, false
}

// configFile is the part of config.yaml the installer writes: only what
// differs from the defaults, in the order a person reads it.
type configFile struct {
	Daemon struct {
		Listen string `yaml:"listen"`
	} `yaml:"daemon"`
	Hooks *struct {
		Listen string `yaml:"listen"`
	} `yaml:"hooks,omitempty"`
	Engine *struct {
		Driver string `yaml:"driver"`
	} `yaml:"engine,omitempty"`
	Proxy  *proxyFile `yaml:"proxy,omitempty"`
	Backup struct {
		Dir string `yaml:"dir"`
	} `yaml:"backup"`
}

type proxyFile struct {
	Driver        string   `yaml:"driver"`
	ConfDir       string   `yaml:"conf_dir"`
	TestCommand   []string `yaml:"test_command,omitempty,flow"`
	ReloadCommand []string `yaml:"reload_command,omitempty,flow"`
}

// ConfigYAML is config.yaml for c. proxyBinary is the proxy's program, for
// the commands that run it through sudo.
func ConfigYAML(c Choices, proxyBinary string) ([]byte, error) {
	var f configFile
	f.Daemon.Listen = c.Listen
	f.Backup.Dir = BackupDir
	if c.HooksListen != "" {
		f.Hooks = &struct {
			Listen string `yaml:"listen"`
		}{c.HooksListen}
	}
	if c.Engine != "docker" {
		f.Engine = &struct {
			Driver string `yaml:"driver"`
		}{c.Engine}
	}
	if p, ok := proxyFor(c.Proxy, proxyBinary); ok {
		pf := &proxyFile{Driver: c.Proxy, ConfDir: p.ConfDir}
		// Every command that changes the running proxy goes through
		// sudo, with -n: never wait for a password nobody types.
		for _, cmd := range p.Sudo {
			sudo := append([]string{"sudo", "-n"}, cmd...)
			if cmd[0] == "/usr/bin/systemctl" {
				pf.ReloadCommand = sudo
			} else {
				pf.TestCommand = sudo
			}
		}
		f.Proxy = pf
	}
	body, err := yaml.Marshal(f)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Written by limand install. See the README for every setting.\n"), body...), nil
}

// SudoersRule lets the liman user run its proxy's commands as root, with
// exactly these arguments; "" when it needs none.
func SudoersRule(proxy, proxyBinary string) string {
	p, ok := proxyFor(proxy, proxyBinary)
	if !ok || len(p.Sudo) == 0 {
		return ""
	}
	cmds := make([]string, len(p.Sudo))
	for i, c := range p.Sudo {
		cmds[i] = strings.Join(c, " ")
	}
	return fmt.Sprintf("# The liman daemon may run these commands as root, with exactly these\n"+
		"# arguments. Note what that still means: the proxy then reads the files in\n"+
		"# %s, which the liman user writes.\n"+
		"liman ALL=(root) NOPASSWD: %s\n", p.ConfDir, strings.Join(cmds, ", "))
}

// caddyImport is the one line the Caddyfile needs.
func caddyImport(dir string) string { return "import " + dir + "/*.caddy" }

// AddCaddyImport returns the Caddyfile with the line that pulls in liman's
// directory, and whether it had to be added.
func AddCaddyImport(content []byte, dir string) ([]byte, bool) {
	line := caddyImport(dir)
	for _, l := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(l) == line {
			return content, false
		}
	}
	out := bytes.TrimRight(content, "\n")
	out = append(out, []byte("\n\n# Apps deployed by liman. Added by limand install.\n"+line+"\n")...)
	return out, true
}

// TraefikStaticFiles are where Traefik's static configuration usually is.
var TraefikStaticFiles = []string{"/etc/traefik/traefik.yml", "/etc/traefik/traefik.yaml", "/etc/traefik/traefik.toml"}

// TraefikWatches reports whether a static configuration has a file
// provider on dir. Its format is the person's (YAML, TOML, flags in a
// unit); the installer does not edit it, it only looks for the directory.
func TraefikWatches(content []byte, dir string) bool {
	return bytes.Contains(content, []byte(dir))
}

// TraefikInstructions says what to add when Traefik does not watch dir.
func TraefikInstructions(dir string) string {
	return "Traefik must watch " + dir + " for the files liman writes. Add to its static configuration:\n\n" +
		"  # traefik.yml\n  providers:\n    file:\n      directory: " + dir + "\n      watch: true\n\n" +
		"  # or traefik.toml\n  [providers.file]\n    directory = \"" + dir + "\"\n    watch = true\n\n" +
		"  # or as flags\n  --providers.file.directory=" + dir + " --providers.file.watch=true\n\n" +
		"restart Traefik, and run this again."
}

// Podman's API socket is in /run/podman, which Podman's own tmpfiles rule
// creates as 0700 root:root at every boot: a group on the socket alone is
// not enough (a trial). liman's rule, sorted before Podman's, is the one
// systemd keeps for that path.
const (
	PodmanSocketDropIn = "/etc/systemd/system/podman.socket.d/liman.conf"
	PodmanTmpfiles     = "/etc/tmpfiles.d/liman-podman.conf"
)

// PodmanSocketConf gives the socket to the liman group.
func PodmanSocketConf() string {
	return "# Written by limand install: the liman daemon talks to Podman here.\n" +
		"[Socket]\nSocketGroup=liman\nSocketMode=0660\n"
}

// PodmanTmpfilesConf lets the liman group into /run/podman.
func PodmanTmpfilesConf() string {
	return "# Written by limand install: the liman group may reach Podman's socket.\n" +
		"d /run/podman 0750 root liman -\n"
}

//go:embed units
var units embed.FS

// unitData fills in limand.service.
type unitData struct {
	EngineUnit         string // docker.service, podman.socket
	SupplementaryGroup string // docker, or none
	ProxyName          string // Nginx, Caddy, Traefik, or ""
	ProxyUnit          string
	ConfDir            string
}

// DaemonUnit is limand.service for an engine and a proxy.
func DaemonUnit(engine, proxy string) ([]byte, error) {
	d := unitData{EngineUnit: "docker.service", SupplementaryGroup: "docker"}
	if engine == "podman" {
		// The socket is the liman group's (PodmanSocketConf).
		d = unitData{EngineUnit: "podman.socket"}
	}
	if p, ok := proxyFor(proxy, ""); ok {
		d.ProxyName, d.ProxyUnit, d.ConfDir = p.Name, p.Unit, p.ConfDir
	}
	text, err := units.ReadFile("units/limand.service.tmpl")
	if err != nil {
		return nil, err
	}
	t, err := template.New("unit").Parse(string(text))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// BackupUnits are the backup service and its timer, as they are.
func BackupUnits() (service, timer []byte, err error) {
	if service, err = units.ReadFile("units/limand-backup.service"); err != nil {
		return nil, nil, err
	}
	timer, err = units.ReadFile("units/limand-backup.timer")
	return service, timer, err
}
