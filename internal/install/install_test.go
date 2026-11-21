package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onrcanogul/liman/internal/config"
)

// What the trial found on GitHub's Ubuntu 26.04, as the commands print it.
func TestGather(t *testing.T) {
	outputs := map[string]string{
		"systemctl is-active docker":        "active\n",
		"systemctl is-active podman.socket": "inactive\n",
		"systemctl is-active nginx":         "inactive\n",
		"systemctl is-active caddy":         "active\n",
		"ss -ltnpH sport = :80":             `LISTEN 0      4096   *:80 *:* users:(("caddy",pid=2600,fd=6))` + "\n",
		"ip -4 -o addr show": "1: lo    inet 127.0.0.1/8 scope host lo\\       valid_lft forever\n" +
			"2: eth0    inet 10.1.0.168/20 metric 100 brd 10.1.15.255 scope global dynamic eth0\n" +
			"3: docker0    inet 172.17.0.1/16 brd 172.17.255.255 scope global docker0\n" +
			"5: wg0    inet 10.8.0.1/24 scope global wg0\n" +
			"6: podman0    inet 10.88.0.1/16 scope global podman0\n",
		"getent group docker": "docker:x:112:runner\n",
	}
	run := func(_ context.Context, name string, args ...string) (string, error) {
		out, ok := outputs[strings.Join(append([]string{name}, args...), " ")]
		if !ok {
			return "", errors.New("exit status 1")
		}
		return out, nil
	}
	paths := map[string]string{"docker": "/usr/bin/docker", "podman": "/usr/bin/podman", "nginx": "/usr/sbin/nginx", "caddy": "/usr/bin/caddy"}
	f := Gather(context.Background(), run, func(n string) string { return paths[n] })

	if !f.Docker.Active || f.Podman.Active || !f.Podman.Installed() || !f.DockerGroup {
		t.Errorf("engines: %+v %+v group %v", f.Docker, f.Podman, f.DockerGroup)
	}
	if f.Proxies["nginx"].Active || !f.Proxies["caddy"].Active || f.Proxies["traefik"].Installed() {
		t.Errorf("proxies: %+v", f.Proxies)
	}
	if f.Port80 != "caddy" {
		t.Errorf("Port80 = %q", f.Port80)
	}
	// Bridges are left out; WireGuard and the network card stay.
	if got := len(f.Addresses); got != 2 || f.Addresses[0].Interface != "eth0" || f.Addresses[1].IP != "10.8.0.1" {
		t.Errorf("Addresses = %+v", f.Addresses)
	}
}

// fakeAsker answers questions from a list, and records them.
type fakeAsker struct {
	choices  []int
	texts    []string
	confirms []bool
	asked    []string
}

func (a *fakeAsker) Choose(q string, _ []Option, def int) (int, error) {
	a.asked = append(a.asked, q)
	if len(a.choices) == 0 {
		return def, nil
	}
	c := a.choices[0]
	a.choices = a.choices[1:]
	return c, nil
}

func (a *fakeAsker) Text(q, def string) (string, error) {
	a.asked = append(a.asked, q)
	if len(a.texts) == 0 {
		return def, nil
	}
	t := a.texts[0]
	a.texts = a.texts[1:]
	return t, nil
}

func (a *fakeAsker) Confirm(q string, def bool) (bool, error) {
	a.asked = append(a.asked, q)
	if len(a.confirms) == 0 {
		return def, nil
	}
	c := a.confirms[0]
	a.confirms = a.confirms[1:]
	return c, nil
}

func server() Facts {
	return Facts{
		Docker:      Tool{Path: "/usr/bin/docker", Active: true},
		Podman:      Tool{Path: "/usr/bin/podman"},
		Proxies:     map[string]Tool{"nginx": {Path: "/usr/sbin/nginx", Active: true}, "caddy": {Path: "/usr/bin/caddy"}},
		Port80:      "nginx",
		Addresses:   []Address{{"eth0", "10.1.0.168"}, {"wg0", "10.8.0.1"}},
		DockerGroup: true,
	}
}

func TestChooseWithoutQuestions(t *testing.T) {
	// The defaults: what runs, what is on port 80, the WireGuard address,
	// and no trigger endpoint unless asked for.
	c, err := Choose(server(), Flags{}, nil)
	want := Choices{Engine: "docker", Proxy: "nginx", Listen: "10.8.0.1:7070"}
	if err != nil || c != want {
		t.Errorf("Choose() = %+v, %v; want %+v", c, err, want)
	}
	if got := c.Flags(); got != "--engine docker --proxy nginx --listen 10.8.0.1:7070 --no-hooks" {
		t.Errorf("Flags() = %s", got)
	}

	// Flags win, and are checked.
	c, err = Choose(server(), Flags{Engine: "podman", Proxy: "caddy", Listen: "10.8.0.1:9000", HooksListen: "127.0.0.1:7443"}, nil)
	if err != nil || c != (Choices{"podman", "caddy", "10.8.0.1:9000", "127.0.0.1:7443"}) {
		t.Errorf("with flags: %+v, %v", c, err)
	}
}

func TestChooseRefuses(t *testing.T) {
	noWG := server()
	noWG.Addresses = []Address{{"eth0", "10.1.0.168"}}
	idle := server()
	idle.Proxies = map[string]Tool{"nginx": {Path: "/usr/sbin/nginx"}, "caddy": {Path: "/usr/bin/caddy"}}
	idle.Port80 = ""
	for _, tt := range []struct {
		name    string
		facts   Facts
		flags   Flags
		wantErr string
	}{
		{"no engine", Facts{}, Flags{}, "neither Docker nor Podman"},
		{"unknown engine", server(), Flags{Engine: "containerd"}, "docker or podman"},
		{"proxy not installed", server(), Flags{Proxy: "traefik"}, "traefik is not installed"},
		{"unknown proxy", server(), Flags{Proxy: "apache"}, "nginx, caddy or traefik"},
		// No terminal, no WireGuard: guessing the API's address would
		// be guessing what is private.
		{"no address to suggest", noWG, Flags{}, "--listen is needed"},
		{"public address", server(), Flags{Listen: "203.0.113.5:7070"}, "--listen 203.0.113.5:7070"},
		{"proxies but none running", idle, Flags{}, "give --proxy"},
		{"hooks both ways", server(), Flags{NoHooks: true, HooksListen: "127.0.0.1:7443"}, "contradict"},
	} {
		if _, err := Choose(tt.facts, tt.flags, nil); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: error = %v, want %q", tt.name, err, tt.wantErr)
		}
	}
}

func TestChooseAsks(t *testing.T) {
	noWG := server()
	noWG.Addresses = []Address{{"eth0", "10.1.0.168"}}
	ask := &fakeAsker{
		choices:  []int{1, 1},                       // Podman, Caddy
		texts:    []string{"8.8.8.8", "10.1.0.168"}, // a public address first: asked again
		confirms: []bool{true},
	}
	c, err := Choose(noWG, Flags{}, ask)
	want := Choices{Engine: "podman", Proxy: "caddy", Listen: "10.1.0.168:7070", HooksListen: DefaultHooksListen}
	if err != nil || c != want {
		t.Fatalf("Choose() = %+v, %v; want %+v", c, err, want)
	}
	if len(ask.asked) != 5 || !strings.Contains(ask.asked[3], "not usable") {
		t.Errorf("asked %q", ask.asked)
	}

	// Only what is there is offered: with one engine nobody is asked.
	one := server()
	one.Podman = Tool{}
	ask = &fakeAsker{}
	if c, _ := Choose(one, Flags{}, ask); c.Engine != "docker" || strings.Contains(strings.Join(ask.asked, "|"), "engine") {
		t.Errorf("asked %q for one engine", ask.asked)
	}

	// --yes: no questions, the defaults.
	ask = &fakeAsker{choices: []int{1, 1}}
	if c, _ := Choose(server(), Flags{Yes: true}, ask); c.Engine != "docker" || len(ask.asked) != 0 {
		t.Errorf("--yes asked %q", ask.asked)
	}
}

// The config.yaml written must be one the daemon loads, with the choices in
// it.
func TestConfigYAMLLoads(t *testing.T) {
	for _, c := range []Choices{
		{Engine: "docker", Proxy: "nginx", Listen: "10.8.0.1:7070", HooksListen: "127.0.0.1:7443"},
		{Engine: "podman", Proxy: "caddy", Listen: "10.8.0.1:7070"},
		{Engine: "docker", Proxy: "traefik", Listen: "10.8.0.1:7070"},
		{Engine: "podman", Proxy: "none", Listen: "10.8.0.1:7070"},
	} {
		data, err := ConfigYAML(c, "/usr/sbin/nginx")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("%+v: %v\n%s", c, err, data)
		}
		wantDriver := c.Engine
		if cfg.Engine.Driver == "" {
			wantDriver = "docker"
		}
		if wantDriver != c.Engine || cfg.Daemon.Listen != c.Listen || cfg.Hooks.Listen != c.HooksListen {
			t.Errorf("%+v loaded as %+v", c, cfg)
		}
		switch c.Proxy {
		case "none":
			if cfg.Proxy.Enabled() {
				t.Errorf("a proxy for none:\n%s", data)
			}
		case "nginx":
			if cfg.Proxy.Driver != "nginx" || strings.Join(cfg.Proxy.TestCommand, " ") != "sudo -n /usr/sbin/nginx -t" || strings.Join(cfg.Proxy.ReloadCommand, " ") != "sudo -n /usr/bin/systemctl reload nginx" {
				t.Errorf("nginx: %+v", cfg.Proxy)
			}
		case "caddy":
			// Caddy validates as the liman user; only the reload is sudo.
			if cfg.Proxy.ConfDir != "/etc/caddy/liman.d" || len(cfg.Proxy.TestCommand) != 0 || strings.Join(cfg.Proxy.ReloadCommand, " ") != "sudo -n /usr/bin/systemctl reload caddy" {
				t.Errorf("caddy: %+v", cfg.Proxy)
			}
		case "traefik":
			if cfg.Proxy.ConfDir != "/etc/traefik/liman.d" || len(cfg.Proxy.ReloadCommand) != 0 {
				t.Errorf("traefik: %+v", cfg.Proxy)
			}
		}
	}
}

func TestSudoersRule(t *testing.T) {
	if r := SudoersRule("nginx", "/usr/sbin/nginx"); !strings.Contains(r, "NOPASSWD: /usr/sbin/nginx -t, /usr/bin/systemctl reload nginx\n") {
		t.Errorf("nginx rule:\n%s", r)
	}
	if r := SudoersRule("caddy", "/usr/bin/caddy"); !strings.Contains(r, "NOPASSWD: /usr/bin/systemctl reload caddy\n") {
		t.Errorf("caddy rule:\n%s", r)
	}
	if SudoersRule("traefik", "") != "" || SudoersRule("none", "") != "" {
		t.Error("a rule for a proxy that needs no commands")
	}
}

func TestAddCaddyImport(t *testing.T) {
	// Ubuntu's Caddyfile, as the trial found it.
	start := []byte(":80 {\n\troot * /usr/share/caddy\n\tfile_server\n}\n")
	once, added := AddCaddyImport(start, "/etc/caddy/liman.d")
	if !added || !strings.HasSuffix(string(once), "\nimport /etc/caddy/liman.d/*.caddy\n") || !strings.HasPrefix(string(once), string(start)) {
		t.Errorf("first time (%v):\n%s", added, once)
	}
	// Run again: not twice.
	if twice, added := AddCaddyImport(once, "/etc/caddy/liman.d"); added || string(twice) != string(once) {
		t.Errorf("second time added it again:\n%s", twice)
	}
}

func TestTraefikWatches(t *testing.T) {
	yamlConf := []byte("providers:\n  file:\n    directory: /etc/traefik/liman.d\n    watch: true\n")
	if !TraefikWatches(yamlConf, "/etc/traefik/liman.d") || TraefikWatches([]byte("entryPoints:\n  web:\n    address: :80\n"), "/etc/traefik/liman.d") {
		t.Error("TraefikWatches is wrong")
	}
	if !strings.Contains(TraefikInstructions("/etc/traefik/liman.d"), "directory: /etc/traefik/liman.d") {
		t.Error("the instructions do not name the directory")
	}
}

func TestDaemonUnit(t *testing.T) {
	docker, err := DaemonUnit("docker", "nginx")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Description=liman daemon (deploys containers behind Nginx)",
		"After=network-online.target docker.service nginx.service wg-quick@wg0.service",
		"Wants=network-online.target docker.service\n",
		"SupplementaryGroups=docker\n",
		"ReadWritePaths=-/etc/nginx/liman.d\n",
	} {
		if !strings.Contains(string(docker), want) {
			t.Errorf("docker+nginx unit lacks %q", want)
		}
	}
	podman, _ := DaemonUnit("podman", "caddy")
	for _, want := range []string{"After=network-online.target podman.socket caddy.service", "ReadWritePaths=-/etc/caddy/liman.d\n"} {
		if !strings.Contains(string(podman), want) {
			t.Errorf("podman+caddy unit lacks %q", want)
		}
	}
	if strings.Contains(string(podman), "SupplementaryGroups") {
		t.Error("podman unit puts liman in a group; the socket is the liman group's")
	}
	none, _ := DaemonUnit("docker", "none")
	if strings.Contains(string(none), "ReadWritePaths") || strings.Contains(string(none), "behind") {
		t.Errorf("unit without a proxy names one:\n%s", none)
	}
}
