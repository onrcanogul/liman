// Package cli implements the liman command-line interface.
//
// The CLI is a client: it reads arguments and files, asks the daemon to do
// the work over the HTTP API and prints what comes back. It never talks to
// Docker or Nginx itself.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/buildinfo"
)

// Exit codes returned by Run. Following the common Unix convention, 2 means
// the command line itself was wrong, as opposed to the command failing.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `liman is a small PaaS for a single server.

Usage:
  liman <command> [arguments]

Commands:
  deploy [--dry-run] [--step] <file>
                       Deploy an app. --dry-run only shows the steps and the
                       equivalent manual commands; --step asks before each step
  plan <file>          Same as deploy --dry-run
  ps                   List containers managed by liman
  logs [-f] <app>      Show the logs of an app (-f keeps following)
  stop <app>           Stop and remove an app
  history [-n N] [app] Show the last N executed steps (default 20)
  status               Check that the daemon is reachable
  version              Print version information
  help                 Show this help

liman sends these commands to the limand daemon. Its address and token come
from $LIMAN_HOME/client.yaml (default ~/.liman/client.yaml) or from the
LIMAN_URL and LIMAN_TOKEN environment variables.
`

// CLI holds everything a command needs from the outside world. main fills
// it with the real streams and a client for the real daemon; tests point
// the client at a test server.
type CLI struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// API is the connection to the daemon.
	API *api.Client
}

// Run executes the CLI with the given arguments (without the program name)
// and returns the process exit code.
func (c *CLI) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.Stderr, usage)
		return exitUsage
	}

	// args[0] selects the command; the rest belongs to that command.
	name, rest := args[0], args[1:]

	switch name {
	case "deploy":
		return c.runDeploy(ctx, rest, false)
	case "plan":
		return c.runDeploy(ctx, rest, true)
	case "ps":
		return c.runPs(ctx, rest)
	case "logs":
		return c.runLogs(ctx, rest)
	case "stop":
		return c.runStop(ctx, rest)
	case "history":
		return c.runHistory(ctx, rest)
	case "status":
		return c.runStatus(ctx, rest)
	case "version":
		fmt.Fprintln(c.Stdout, buildinfo.String())
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(c.Stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(c.Stderr, "liman: unknown command %q\n\n", name)
		fmt.Fprint(c.Stderr, usage)
		return exitUsage
	}
}

// runDeploy implements both deploy and plan; plan is deploy with the dry
// run forced on, so the two can never describe different steps.
func (c *CLI) runDeploy(ctx context.Context, args []string, planOnly bool) int {
	synopsis := "liman deploy [--dry-run] [--step] <file>"
	if planOnly {
		synopsis = "liman plan <file>"
	}

	// Each command gets its own FlagSet, so flags of one command do not
	// leak into another. ContinueOnError makes Parse return the error
	// instead of calling os.Exit, which would bypass our exit codes.
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "show the steps without executing them")
	stepMode := fs.Bool("step", false, "ask for confirmation before every step")

	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return c.usageError(synopsis)
	}
	if planOnly {
		if *dryRun || *stepMode {
			return c.usageError(synopsis)
		}
		*dryRun = true
	}
	if *dryRun && *stepMode {
		fmt.Fprintln(c.Stderr, "liman: --dry-run and --step cannot be combined")
		return exitUsage
	}

	definition, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return c.fail(fmt.Errorf("read app definition: %w", err))
	}
	// Validate locally before going to the network: a typo in app.yaml is
	// reported at once, even when the daemon is unreachable. The daemon
	// validates again, because it cannot trust its clients.
	a, err := app.Parse(definition)
	if err != nil {
		return c.fail(fmt.Errorf("%s: %w", fs.Arg(0), err))
	}

	opts := api.DeployOptions{DryRun: *dryRun}
	if *stepMode {
		opts.Confirm = c.confirm()
	}
	if *dryRun {
		fmt.Fprintf(c.Stdout, "Dry run for %s: nothing will be changed.\n\n", a.Name)
	}

	// The daemon's progress output is copied to stdout as it arrives.
	res, err := c.API.Deploy(ctx, definition, opts, c.Stdout)
	if err != nil {
		// The details were already streamed; the summary is enough here.
		if errors.Is(err, api.ErrDeployFailed) && res != nil {
			fmt.Fprintf(c.Stderr, "liman: %s\n", res.Error)
			return exitError
		}
		return c.fail(err)
	}

	if res.DryRun {
		return exitOK
	}

	fmt.Fprintf(c.Stdout, "\n%s is running on the server at 127.0.0.1:%d (release %s)\n", res.App, res.HostPort, res.Release)
	if res.Routed {
		fmt.Fprintf(c.Stdout, "Nginx routes %s to it\n", res.Domain)
	} else {
		fmt.Fprintln(c.Stdout, "No Nginx is configured on the server, so it is only reachable on that address")
	}
	return exitOK
}

// confirm returns the callback that asks the user before each step.
func (c *CLI) confirm() func(step string) (bool, error) {
	// One reader for all questions: a bufio.Reader may read ahead, so
	// creating a new one per question could lose input.
	in := bufio.NewReader(c.Stdin)

	return func(string) (bool, error) {
		fmt.Fprint(c.Stdout, "    Run this step? [y/N] ")

		line, err := in.ReadString('\n')
		// ReadString returns what it read together with io.EOF when the
		// input ends without a newline; only fail if nothing was read.
		if err != nil && line == "" {
			fmt.Fprintln(c.Stdout)
			return false, fmt.Errorf("read answer: %w", err)
		}

		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes", nil
	}
}

func (c *CLI) runPs(ctx context.Context, args []string) int {
	if len(args) != 0 {
		return c.usageError("liman ps")
	}

	containers, err := c.API.Apps(ctx)
	if err != nil {
		return c.fail(err)
	}
	if len(containers) == 0 {
		fmt.Fprintln(c.Stdout, "No apps. Deploy one with: liman deploy <file>")
		return exitOK
	}

	// tabwriter aligns tab-separated cells into columns once Flush is
	// called, so the widths adapt to the longest value.
	w := tabwriter.NewWriter(c.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "APP\tCONTAINER\tIMAGE\tSTATE\tHEALTH\tADDRESS\tSTATUS")
	// Explanations of anything that is not healthy are printed below the
	// table: they are sentences, and would wreck the columns.
	var notes []string
	for _, ctr := range containers {
		address := "-"
		if ctr.HostPort != 0 {
			address = fmt.Sprintf("127.0.0.1:%d", ctr.HostPort)
		}

		health := ctr.Health
		switch {
		case health == "":
			// The monitor has not looked yet, or is switched off.
			health = "-"
		case ctr.Restarts == 1:
			health += " (1 restart)"
		case ctr.Restarts > 1:
			health = fmt.Sprintf("%s (%d restarts)", health, ctr.Restarts)
		}
		if ctr.HealthDetail != "" {
			notes = append(notes, fmt.Sprintf("%s: %s", ctr.App, ctr.HealthDetail))
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ctr.App, ctr.Container, ctr.Image, ctr.State, health, address, ctr.Status)
	}
	if err := w.Flush(); err != nil {
		return c.fail(err)
	}

	if len(notes) > 0 {
		fmt.Fprintln(c.Stdout)
		for _, note := range notes {
			fmt.Fprintln(c.Stdout, note)
		}
	}
	return exitOK
}

func (c *CLI) runLogs(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	follow := fs.Bool("f", false, "keep following the log output")

	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return c.usageError("liman logs [-f] <app>")
	}

	// fs.Arg(0) is the first argument that is not a flag.
	err := c.API.Logs(ctx, fs.Arg(0), *follow, c.Stdout)
	// Pressing Ctrl+C while following cancels ctx. That is the normal way
	// to stop following, not a failure.
	if err != nil && ctx.Err() == nil {
		return c.fail(err)
	}
	return exitOK
}

func (c *CLI) runStop(ctx context.Context, args []string) int {
	if len(args) != 1 {
		return c.usageError("liman stop <app>")
	}

	res, err := c.API.Stop(ctx, args[0])
	if err != nil {
		return c.fail(err)
	}

	if res.ProxyConfig != "" {
		fmt.Fprintf(c.Stdout, "Removed Nginx config %s\n", res.ProxyConfig)
	}
	for _, name := range res.Removed {
		fmt.Fprintf(c.Stdout, "Stopped and removed %s\n", name)
	}
	return exitOK
}

func (c *CLI) runHistory(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("n", 20, "number of entries to show")

	if err := fs.Parse(args); err != nil || fs.NArg() > 1 || *limit < 1 {
		return c.usageError("liman history [-n N] [app]")
	}

	// fs.Arg(0) is empty when no app was given, which means "all apps".
	events, err := c.API.History(ctx, fs.Arg(0), *limit)
	if err != nil {
		return c.fail(err)
	}
	if len(events) == 0 {
		fmt.Fprintln(c.Stdout, "No history yet.")
		return exitOK
	}

	w := tabwriter.NewWriter(c.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tAPP\tRELEASE\tACTION\tRESULT\tTOOK\tSTEP")
	for _, ev := range events {
		result := "ok"
		if !ev.OK {
			result = "FAILED"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			ev.Time.Local().Format("2006-01-02 15:04:05"),
			ev.App, ev.Release, ev.Action, result,
			(time.Duration(ev.DurationMS) * time.Millisecond).String(),
			ev.Step)
	}
	if err := w.Flush(); err != nil {
		return c.fail(err)
	}
	return exitOK
}

// runStatus checks the two things that can be wrong on the client side:
// the address and the token.
func (c *CLI) runStatus(ctx context.Context, args []string) int {
	if len(args) != 0 {
		return c.usageError("liman status")
	}

	// The health endpoint needs no token, so this tells "unreachable"
	// apart from "reachable but not authorised".
	health, err := c.API.Health(ctx)
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "Daemon at %s is up (version %s)\n", c.API.BaseURL, health.Version)

	if _, err := c.API.Apps(ctx); err != nil {
		var apiErr *api.Error
		// errors.As looks for an error of this type in the chain and, if
		// found, assigns it so its fields can be read.
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
			fmt.Fprintf(c.Stderr, "liman: the token was rejected: %v\n", err)
			return exitError
		}
		// Reachable and authorised, but something behind the daemon is not
		// working, typically Docker.
		fmt.Fprintf(c.Stderr, "liman: token accepted, but the daemon reports a problem: %v\n", err)
		return exitError
	}
	fmt.Fprintln(c.Stdout, "Token accepted")
	return exitOK
}

// fail prints err and returns the failure exit code.
func (c *CLI) fail(err error) int {
	if errors.Is(err, context.Canceled) {
		// The daemon notices the closed connection and rolls back on its
		// own; say so, because the output stopped before showing it.
		fmt.Fprintln(c.Stderr, "liman: interrupted; if a deploy was running, the daemon is rolling it back (see: liman history)")
		return exitError
	}
	fmt.Fprintf(c.Stderr, "liman: %v\n", err)
	return exitError
}

func (c *CLI) usageError(synopsis string) int {
	fmt.Fprintf(c.Stderr, "usage: %s\n", synopsis)
	return exitUsage
}
