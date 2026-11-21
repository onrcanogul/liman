package daemon

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/onrcanogul/liman/internal/install"
)

func (d *Daemon) runInstall(ctx context.Context, args []string) int {
	const synopsis = "limand install [--binary <limand>] [--engine docker|podman] [--proxy nginx|caddy|traefik|none]\n" +
		"                      [--listen <private address:port>] [--hooks-listen <address:port> | --no-hooks] [--yes]"

	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var f install.Flags
	binary := fs.String("binary", "", "the limand to install (default: this one)")
	fs.StringVar(&f.Engine, "engine", "", "docker or podman")
	fs.StringVar(&f.Proxy, "proxy", "", "nginx, caddy, traefik or none")
	fs.StringVar(&f.Listen, "listen", "", "the API's address, on a private network")
	fs.StringVar(&f.HooksListen, "hooks-listen", "", "turn on the trigger endpoint, at this address")
	fs.BoolVar(&f.NoHooks, "no-hooks", false, "no trigger endpoint")
	noNginx := fs.Bool("no-nginx", false, "the same as --proxy none (kept for install.sh)")
	fs.BoolVar(&f.Yes, "yes", false, "take what is found, without questions")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return d.usageError(synopsis)
	}
	if *noNginx {
		if f.Proxy != "" && f.Proxy != "none" {
			fmt.Fprintln(d.Stderr, "limand: --no-nginx and --proxy "+f.Proxy+" contradict each other")
			return exitUsage
		}
		f.Proxy = "none"
	}
	if *binary == "" {
		self, err := os.Executable()
		if err != nil {
			return d.fail(err)
		}
		*binary = self
	}

	opts := install.Options{
		Binary: *binary,
		Flags:  f,
		Stdout: d.Stdout,
		Stderr: d.Stderr,
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return string(out), err
		},
		Look: func(name string) string {
			p, err := exec.LookPath(name)
			if err != nil {
				return ""
			}
			return p
		},
	}
	// Questions only in a terminal: a script, cloud-init or Ansible gets
	// the flags' answers, or an error that names the flag to give.
	if in, ok := d.Stdin.(*os.File); ok && term.IsTerminal(int(in.Fd())) && !f.Yes {
		opts.Ask = &termAsker{in: bufio.NewReader(in), out: d.Stdout}
	}
	if err := install.Run(ctx, opts); err != nil {
		fmt.Fprintf(d.Stderr, "install: %v\n", err)
		return exitError
	}
	return exitOK
}

// termAsker asks in a terminal. An empty answer takes the default.
type termAsker struct {
	in  *bufio.Reader
	out io.Writer
}

func (a *termAsker) line() (string, error) {
	s, err := a.in.ReadString('\n')
	if err != nil && s == "" {
		return "", fmt.Errorf("no answer: %w", err)
	}
	return strings.TrimSpace(s), nil
}

func (a *termAsker) Choose(question string, options []install.Option, def int) (int, error) {
	for {
		fmt.Fprintf(a.out, "%s\n", question)
		for i, o := range options {
			mark := " "
			if i == def {
				mark = "*"
			}
			fmt.Fprintf(a.out, "  %s[%d] %-8s %s\n", mark, i+1, o.Value, o.Label)
		}
		fmt.Fprintf(a.out, "> [%d] ", def+1)
		s, err := a.line()
		if err != nil {
			return 0, err
		}
		if s == "" {
			return def, nil
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		for i, o := range options {
			if strings.EqualFold(s, o.Value) {
				return i, nil
			}
		}
		fmt.Fprintf(a.out, "%q is none of them.\n", s)
	}
}

func (a *termAsker) Text(question, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(a.out, "%s\n> [%s] ", question, def)
	} else {
		fmt.Fprintf(a.out, "%s\n> ", question)
	}
	s, err := a.line()
	if err != nil {
		return "", err
	}
	if s == "" {
		return def, nil
	}
	return s, nil
}

func (a *termAsker) Confirm(question string, def bool) (bool, error) {
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	for {
		fmt.Fprintf(a.out, "%s %s ", question, hint)
		s, err := a.line()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(s) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
	}
}
