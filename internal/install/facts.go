// Package install sets liman up on a server: the liman user, the binary,
// config.yaml, the container engine's and the proxy's side of things, the
// systemd units. "limand install" runs it; deploy/install.sh calls that.
//
// It first looks at the machine (Facts), then settles what to install
// (Choices) from flags, what it found, and, in a terminal, questions; and
// only then changes anything. The deciding is plain functions with tests;
// the doing runs commands, and CI's end-to-end test runs it on a fresh
// Ubuntu.
package install

import (
	"context"
	"regexp"
	"strings"
)

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// Tool is a program the installer looks for.
type Tool struct {
	// Path is where it is installed, "" if it is not.
	Path string
	// Active says its systemd unit is running (for Podman: its API
	// socket).
	Active bool
}

// Installed reports whether the program is there.
func (t Tool) Installed() bool { return t.Path != "" }

// Facts are what the installer found on the machine.
type Facts struct {
	Docker, Podman Tool
	// Proxies are nginx, caddy and traefik, by name.
	Proxies map[string]Tool
	// Port80 is the program listening on port 80, "" if none.
	Port80 string
	// Addresses are the machine's own IPv4 addresses, without loopback
	// and container bridges: candidates for the API to listen on.
	Addresses []Address
	// DockerGroup says the docker group exists.
	DockerGroup bool
}

// Address is an IPv4 address of a network interface.
type Address struct {
	Interface string
	IP        string
}

// ProxyNames are the proxies the installer knows, in the order offered.
var ProxyNames = []string{"nginx", "caddy", "traefik"}

// Gather looks at the machine. look finds a program on PATH ("" if not
// there); run runs a command. Nothing is changed.
func Gather(ctx context.Context, run Runner, look func(string) string) Facts {
	active := func(unit string) bool {
		out, _ := run(ctx, "systemctl", "is-active", unit)
		return strings.TrimSpace(out) == "active"
	}
	f := Facts{Proxies: map[string]Tool{}}
	if p := look("docker"); p != "" {
		f.Docker = Tool{Path: p, Active: active("docker")}
	}
	if p := look("podman"); p != "" {
		f.Podman = Tool{Path: p, Active: active("podman.socket")}
	}
	for _, name := range ProxyNames {
		if p := look(name); p != "" {
			f.Proxies[name] = Tool{Path: p, Active: active(name)}
		}
	}
	if out, err := run(ctx, "ss", "-ltnpH", "sport = :80"); err == nil {
		f.Port80 = portOwner(out)
	}
	if out, err := run(ctx, "ip", "-4", "-o", "addr", "show"); err == nil {
		f.Addresses = addresses(out)
	}
	if _, err := run(ctx, "getent", "group", "docker"); err == nil {
		f.DockerGroup = true
	}
	return f
}

// The process name in ss's users:(("caddy",pid=2600,fd=6)).
var ssUser = regexp.MustCompile(`users:\(\("([^"]+)"`)

// portOwner reads the program out of ss -ltnpH output.
func portOwner(out string) string {
	if m := ssUser.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// bridges are interfaces of container engines and virtual machines: their
// addresses are the host's side of a private network nobody else is on.
var bridges = []string{"docker", "podman", "cni", "veth", "br-", "virbr", "lxc", "lxd", "flannel", "cali"}

// addresses reads ip -4 -o addr show: "2: eth0    inet 10.1.0.168/20 …".
func addresses(out string) []Address {
	var list []Address
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[2] != "inet" {
			continue
		}
		iface := fields[1]
		ip, _, _ := strings.Cut(fields[3], "/")
		if iface == "lo" || strings.HasPrefix(ip, "127.") {
			continue
		}
		bridge := false
		for _, b := range bridges {
			if strings.HasPrefix(iface, b) {
				bridge = true
				break
			}
		}
		if !bridge {
			list = append(list, Address{Interface: iface, IP: ip})
		}
	}
	return list
}
