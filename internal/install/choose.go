package install

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/onrcanogul/liman/internal/config"
)

// Flags are the choices given on the command line; empty means "not
// given". The same choices can be made by answering questions.
type Flags struct {
	Engine      string // docker | podman
	Proxy       string // nginx | caddy | traefik | none
	Listen      string // the API's address
	HooksListen string // the trigger endpoint's address
	NoHooks     bool   // no trigger endpoint
	// Rootful runs Podman as root, as Docker runs, instead of as the
	// liman user (the default).
	Rootful bool
	// Yes takes what the installer found without asking.
	Yes bool
}

// Choices are what will be installed.
type Choices struct {
	Engine string
	// Rootless: Podman runs as the liman user. Only with podman.
	Rootless    bool
	Proxy       string // "none" for none
	Listen      string
	HooksListen string // "" for none
}

// Flags returns the command line that installs the same without questions.
func (c Choices) Flags() string {
	parts := []string{"--engine " + c.Engine}
	if c.Engine == "podman" && !c.Rootless {
		parts = append(parts, "--rootful")
	}
	parts = append(parts, "--proxy "+c.Proxy, "--listen "+c.Listen)
	if c.HooksListen != "" {
		parts = append(parts, "--hooks-listen "+c.HooksListen)
	} else {
		parts = append(parts, "--no-hooks")
	}
	return strings.Join(parts, " ")
}

// EngineLabel is the engine as the installer says it: "podman (rootless)".
func (c Choices) EngineLabel() string {
	switch {
	case c.Engine != "podman":
		return c.Engine
	case c.Rootless:
		return "podman (rootless)"
	}
	return "podman (rootful)"
}

// Option is one answer to a question.
type Option struct {
	Value string
	Label string // what is said about it: "running", "on port 80"
}

// Asker asks the person installing. A nil Asker means nobody can be asked:
// no terminal, or --yes.
type Asker interface {
	// Choose returns the index of the option picked; def is the one
	// offered as the default.
	Choose(question string, options []Option, def int) (int, error)
	// Text returns an answer, def if none is given.
	Text(question, def string) (string, error)
	// Confirm returns a yes or a no, def if none is given.
	Confirm(question string, def bool) (bool, error)
}

// DefaultHooksListen is where the trigger endpoint listens when it is
// turned on without an address: a proxy in front makes it reachable.
const DefaultHooksListen = "127.0.0.1:7443"

// DefaultPort is the API's port when an address is suggested.
const DefaultPort = "7070"

// Choose settles what to install: a flag decides; without one, what was
// found, if that leaves one answer; otherwise the person is asked, or, when
// nobody can be, the installer stops and names the flag to give.
func Choose(f Facts, flags Flags, ask Asker) (Choices, error) {
	if flags.Yes {
		ask = nil
	}
	var c Choices
	var err error
	if c.Engine, err = chooseEngine(f, flags.Engine, ask); err != nil {
		return Choices{}, err
	}
	if c.Rootless, err = chooseRootless(c.Engine, flags, ask); err != nil {
		return Choices{}, err
	}
	if c.Proxy, err = chooseProxy(f, flags.Proxy, ask); err != nil {
		return Choices{}, err
	}
	if c.Listen, err = chooseListen(f, flags.Listen, ask); err != nil {
		return Choices{}, err
	}
	if c.HooksListen, err = chooseHooks(flags, ask); err != nil {
		return Choices{}, err
	}
	return c, nil
}

func chooseEngine(f Facts, flag string, ask Asker) (string, error) {
	tools := map[string]Tool{"docker": f.Docker, "podman": f.Podman}
	if flag != "" {
		t, known := tools[flag]
		switch {
		case !known:
			return "", fmt.Errorf("--engine %q: liman drives docker or podman", flag)
		case !t.Installed():
			return "", fmt.Errorf("--engine %s: %s is not installed", flag, flag)
		}
		return flag, nil
	}
	var options []Option
	active := []bool{}
	for _, name := range []string{"docker", "podman"} {
		t := tools[name]
		if !t.Installed() {
			continue
		}
		label := "installed"
		switch {
		case t.Active && name == "podman":
			label = "API socket on"
		case t.Active:
			label = "running"
		}
		options = append(options, Option{Value: name, Label: label})
		active = append(active, t.Active)
	}
	// The first one running is the default: Docker before Podman.
	def := firstTrue(active)
	switch len(options) {
	case 0:
		return "", errors.New("neither Docker nor Podman is installed; install one of them first")
	case 1:
		return options[0].Value, nil
	}
	if ask == nil {
		return options[def].Value, nil
	}
	i, err := ask.Choose("Container engine", options, def)
	if err != nil {
		return "", err
	}
	return options[i].Value, nil
}

// chooseRootless: Podman runs as the liman user unless --rootful says
// otherwise, or the person answers no.
func chooseRootless(engine string, flags Flags, ask Asker) (bool, error) {
	switch {
	case engine != "podman" && flags.Rootful:
		return false, fmt.Errorf("--rootful is for Podman; %s is the engine", engine)
	case engine != "podman":
		return false, nil
	case flags.Rootful:
		return false, nil
	case ask == nil:
		return true, nil
	}
	return ask.Confirm("Podman rootless: containers run as the liman user, which is then not root's equal?", true)
}

// firstTrue is the index of the first true, or 0.
func firstTrue(list []bool) int {
	for i, v := range list {
		if v {
			return i
		}
	}
	return 0
}

func chooseProxy(f Facts, flag string, ask Asker) (string, error) {
	if flag != "" {
		if flag == "none" {
			return flag, nil
		}
		t, known := f.Proxies[flag]
		switch {
		case !known && !isProxy(flag):
			return "", fmt.Errorf("--proxy %q: liman drives nginx, caddy or traefik, or none", flag)
		case !t.Installed():
			return "", fmt.Errorf("--proxy %s: %s is not installed (or give --proxy none)", flag, flag)
		}
		return flag, nil
	}
	var options []Option
	// The default: the one on port 80, or else the first one running.
	onPort, running := -1, -1
	for _, name := range ProxyNames {
		t, ok := f.Proxies[name]
		if !ok || !t.Installed() {
			continue
		}
		label := "installed, not running"
		switch {
		case f.Port80 == name:
			label = "on port 80"
			onPort = len(options)
		case t.Active:
			label = "running"
			if running < 0 {
				running = len(options)
			}
		}
		options = append(options, Option{Value: name, Label: label})
	}
	def := onPort
	if def < 0 {
		def = running
	}
	if len(options) == 0 {
		// Nothing to route through: apps on fixed ports and workers.
		return "none", nil
	}
	options = append(options, Option{Value: "none", Label: "apps on fixed ports, or workers"})
	if ask == nil {
		if def < 0 {
			return "", errors.New("a proxy is installed but none is running; give --proxy nginx, caddy, traefik or none")
		}
		return options[def].Value, nil
	}
	if def < 0 {
		def = 0
	}
	i, err := ask.Choose("Reverse proxy", options, def)
	if err != nil {
		return "", err
	}
	return options[i].Value, nil
}

func isProxy(name string) bool {
	for _, p := range ProxyNames {
		if p == name {
			return true
		}
	}
	return false
}

// chooseListen picks the API's address. A WireGuard address is suggested;
// another private address only offered: on a cloud's private network
// others may be on it too.
func chooseListen(f Facts, flag string, ask Asker) (string, error) {
	if flag != "" {
		if err := config.CheckListenAddress(flag); err != nil {
			return "", fmt.Errorf("--listen %s: %w", flag, err)
		}
		return flag, nil
	}
	suggested := ""
	for _, a := range f.Addresses {
		if strings.HasPrefix(a.Interface, "wg") {
			suggested = net.JoinHostPort(a.IP, DefaultPort)
			break
		}
	}
	if ask == nil {
		if suggested == "" {
			return "", errors.New("--listen is needed: the API's address on a private network (no WireGuard interface was found to suggest one)")
		}
		return suggested, nil
	}
	question := "API address (private: WireGuard or a private network)"
	if suggested == "" {
		var others []string
		for _, a := range f.Addresses {
			others = append(others, a.IP+" ("+a.Interface+")")
		}
		if len(others) > 0 {
			question += "; addresses here: " + strings.Join(others, ", ")
		}
	}
	for {
		answer, err := ask.Text(question, suggested)
		if err != nil {
			return "", err
		}
		if !strings.Contains(answer, ":") && answer != "" {
			answer = net.JoinHostPort(answer, DefaultPort)
		}
		if err := config.CheckListenAddress(answer); err != nil {
			// Asked again: a typo should not end the installation.
			question = fmt.Sprintf("%s is not usable (%v); API address", answer, err)
			continue
		}
		return answer, nil
	}
}

func chooseHooks(flags Flags, ask Asker) (string, error) {
	switch {
	case flags.NoHooks && flags.HooksListen != "":
		return "", errors.New("--hooks-listen and --no-hooks contradict each other")
	case flags.NoHooks:
		return "", nil
	case flags.HooksListen != "":
		if err := config.CheckListenAddress(flags.HooksListen); err != nil {
			return "", fmt.Errorf("--hooks-listen %s: %w", flags.HooksListen, err)
		}
		return flags.HooksListen, nil
	case ask == nil:
		// Without a word about it, the endpoint stays off, as it always
		// was: it is a door, opened on purpose.
		return "", nil
	}
	yes, err := ask.Confirm("Deploys from CI: open the trigger endpoint on "+DefaultHooksListen+"?", true)
	if err != nil || !yes {
		return "", err
	}
	return DefaultHooksListen, nil
}
