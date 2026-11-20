// Package cli implements the liman command-line interface.
//
// The CLI is a client: it reads arguments and files, asks the daemon to do
// the work over the HTTP API and prints what comes back. It never talks to
// Docker or Nginx itself.
package cli

import (
	"bufio"
	"bytes"
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

	"golang.org/x/term"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/buildinfo"
	"github.com/onrcanogul/liman/internal/engine"
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
  rollback [--dry-run] [--step] <app>
                       Put the previous release back into service
  trigger --digest <sha256:...> --file <app.yaml> [--app <app>]
                       From a CI pipeline, after pushing an image: deploy a
                       registered app with this definition and the image of
                       its repository that has this digest. ${NAME} in env
                       values is filled from the environment. Needs a deploy
                       key (LIMAN_TOKEN) and the trigger endpoint's address
                       (LIMAN_URL)
  preview --file <app.yaml> [--digest <sha256:...>] [--markdown]
                       From a pull request's pipeline: what a deploy of this
                       definition would change, and what is wrong with it,
                       without deploying. Needs a preview key; no secrets.
                       --markdown writes a summary for a comment. Exits 1 when
                       the deploy would be refused
  ps                   List containers managed by liman
  logs [-f] <app>      Show the logs of an app (-f keeps following)
  stop <app>           Stop and remove an app
  history [-n N] [app] Show the last N executed steps (default 20)
  audit [-n N] [app]   Show who changed what: the last N entries of the audit
                       trail (default 20)
  secret set <app> <NAME>
                       Store a secret; the value is read from standard input
                       (or typed at a prompt that does not show it)
  secret list <app>    List the names of an app's secrets
  secret rm <app> <NAME>
                       Remove a secret
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

	// LookupEnv reads an environment variable, for the values a
	// definition refers to. Nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)

	// Version is this CLI's version, compared with the daemon's by status.
	// Empty means buildinfo.Version.
	Version string
}

// Run executes the CLI with the given arguments (without the program name)
// and returns the process exit code.
func (c *CLI) Run(ctx context.Context, args []string) int {
	// Everything printed from here on passes through a filter that removes
	// terminal control characters; see safeWriter.
	c.Stdout, c.Stderr = safe(c.Stdout), safe(c.Stderr)

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
	case "rollback":
		return c.runRollback(ctx, rest)
	case "trigger":
		return c.runTrigger(ctx, rest)
	case "preview":
		return c.runPreview(ctx, rest)
	case "ps":
		return c.runPs(ctx, rest)
	case "logs":
		return c.runLogs(ctx, rest)
	case "stop":
		return c.runStop(ctx, rest)
	case "history":
		return c.runHistory(ctx, rest)
	case "audit":
		return c.runAudit(ctx, rest)
	case "secret":
		return c.runSecret(ctx, rest)
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

func (c *CLI) runTrigger(ctx context.Context, args []string) int {
	const synopsis = "liman trigger --digest <sha256:...> --file <app.yaml> [--app <app>]"

	fs := flag.NewFlagSet("trigger", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "the app's definition, app.yaml")
	digest := fs.String("digest", "", "the digest of the pushed image")
	name := fs.String("app", "", "the app; checked against the definition's name")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *file == "" || *digest == "" {
		return c.usageError(synopsis)
	}
	// Checked here so a pipeline with an empty or garbled variable fails
	// with a clear message; the daemon checks again.
	if !engine.ValidDigest(*digest) {
		return c.fail(fmt.Errorf("invalid digest %q: want sha256: and 64 hexadecimal digits, as docker push prints it", *digest))
	}
	a, definition, err := readDefinition(*file, *name)
	if err != nil {
		return c.fail(err)
	}

	// The values of the variables the env values refer to, from this
	// process's environment: the pipeline's secrets. Only those: a
	// variable named in a comment is not sent anywhere. One that is not
	// set is left out, and the daemon names it.
	lookup := c.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	values := make([]string, 0, len(a.Env))
	for _, v := range a.Env {
		values = append(values, v)
	}
	vars := map[string]string{}
	for _, v := range app.Variables([]byte(strings.Join(values, "\n"))) {
		if value, ok := lookup(v); ok {
			vars[v] = value
		}
	}

	res, err := c.API.Trigger(ctx, api.TriggerRequest{App: a.Name, Digest: *digest, Definition: string(definition), Vars: vars}, c.Stdout)
	if err != nil {
		if errors.Is(err, api.ErrDeployFailed) && res != nil {
			fmt.Fprintf(c.Stderr, "liman: %s\n", res.Error)
			return exitError
		}
		return c.fail(err)
	}
	if res.Unchanged {
		return exitOK
	}
	fmt.Fprintf(c.Stdout, "\n%s is running release %s, image %s\n", res.App, res.Release, *digest)
	if len(res.Changed) > 0 {
		fmt.Fprintf(c.Stdout, "Changed in the definition: %s\n", strings.Join(res.Changed, ", "))
	}
	return exitOK
}

func (c *CLI) runRollback(ctx context.Context, args []string) int {
	const synopsis = "liman rollback [--dry-run] [--step] <app>"

	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "show the steps without executing them")
	stepMode := fs.Bool("step", false, "ask for confirmation before every step")

	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return c.usageError(synopsis)
	}
	if *dryRun && *stepMode {
		fmt.Fprintln(c.Stderr, "liman: --dry-run and --step cannot be combined")
		return exitUsage
	}
	name := fs.Arg(0)
	// Checked here so a mistyped name is reported without a round trip;
	// the daemon checks again.
	if !app.ValidName(name) {
		return c.fail(fmt.Errorf("invalid app name %q", name))
	}

	opts := api.DeployOptions{DryRun: *dryRun}
	if *stepMode {
		opts.Confirm = c.confirm()
	}
	if *dryRun {
		fmt.Fprintf(c.Stdout, "Dry run of a rollback of %s: nothing will be changed.\n\n", name)
	}

	res, err := c.API.Rollback(ctx, name, opts, c.Stdout)
	if err != nil {
		if errors.Is(err, api.ErrDeployFailed) && res != nil {
			fmt.Fprintf(c.Stderr, "liman: %s\n", res.Error)
			return exitError
		}
		return c.fail(err)
	}

	if res.DryRun {
		return exitOK
	}

	fmt.Fprintf(c.Stdout, "\n%s is back on release %s, running on the server at 127.0.0.1:%d\n", res.App, res.Release, res.HostPort)
	if res.Routed {
		fmt.Fprintln(c.Stdout, "Nginx routes its domain to it")
	}
	fmt.Fprintln(c.Stdout, "The release that was rolled back from has been removed; deploy again to move forward")
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
	fmt.Fprintln(w, "APP\tCONTAINER\tROLE\tIMAGE\tSTATE\tHEALTH\tADDRESS\tSTATUS")
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

		role := ctr.Role
		if role == "" {
			role = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ctr.App, ctr.Container, role, ctr.Image, ctr.State, health, address, ctr.Status)
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
	if len(res.KeptSecrets) > 0 {
		fmt.Fprintf(c.Stdout, "Kept the secrets of %s (%s); deploying it again uses them. Remove one with: liman secret rm %s <name>\n",
			args[0], strings.Join(res.KeptSecrets, ", "), args[0])
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

func (c *CLI) runAudit(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("n", 20, "number of entries to show")
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 || *limit < 1 || *limit > 1000 {
		return c.usageError("liman audit [-n N] [app]")
	}

	entries, err := c.API.Audit(ctx, fs.Arg(0), *limit)
	if err != nil {
		return c.fail(err)
	}
	if len(entries) == 0 {
		fmt.Fprintln(c.Stdout, "Nothing recorded yet.")
		return exitOK
	}
	w := tabwriter.NewWriter(c.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "#\tTIME\tACTOR\tACTION\tAPP\tTARGET\tRESULT")
	for _, e := range entries {
		result := "ok"
		if !e.OK {
			result = "FAILED: " + e.Error
		}
		app := e.App
		if app == "" {
			app = "-"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Seq, e.At.Local().Format("2006-01-02 15:04:05"),
			e.Actor, e.Action, app, e.Target, result)
	}
	if err := w.Flush(); err != nil {
		return c.fail(err)
	}
	return exitOK
}

func (c *CLI) runSecret(ctx context.Context, args []string) int {
	const synopsis = "liman secret set <app> <NAME> | list <app> | rm <app> <NAME>"
	if len(args) == 0 {
		return c.usageError(synopsis)
	}

	switch args[0] {
	case "set":
		// The value is deliberately not an argument. Command lines are
		// saved in the shell history and are visible to every user of the
		// machine in the process list while the command runs.
		if len(args) != 3 {
			return c.usageError("liman secret set <app> <NAME>   (the value is read from standard input)")
		}
		return c.secretSet(ctx, args[1], args[2])

	case "list":
		if len(args) != 2 {
			return c.usageError("liman secret list <app>")
		}
		return c.secretList(ctx, args[1])

	case "rm":
		if len(args) != 3 {
			return c.usageError("liman secret rm <app> <NAME>")
		}
		if err := c.API.DeleteSecret(ctx, args[1], args[2]); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.Stdout, "Removed secret %s of %s; running containers keep it until the next deploy\n", args[2], args[1])
		return exitOK

	default:
		return c.usageError(synopsis)
	}
}

func (c *CLI) secretSet(ctx context.Context, appName, name string) int {
	// Checked before anything is read, so a mistyped name does not cost
	// the user typing the value.
	if !app.ValidName(appName) {
		return c.fail(fmt.Errorf("invalid app name %q", appName))
	}

	value, err := c.readSecret(name)
	if err != nil {
		return c.fail(err)
	}
	if len(value) == 0 {
		return c.fail(errors.New("the secret value is empty"))
	}

	if err := c.API.SetSecret(ctx, appName, name, value); err != nil {
		return c.fail(err)
	}
	// The length is the only thing about the value that is ever shown; it
	// helps to notice a stray newline or a truncated paste.
	fmt.Fprintf(c.Stdout, "Stored secret %s of %s (%d bytes); it takes effect on the next deploy\n", name, appName, len(value))
	return exitOK
}

// readSecret gets a secret value from the user: typed at a prompt that
// does not echo when standard input is a terminal, read to the end
// otherwise (a pipe or a file).
func (c *CLI) readSecret(name string) ([]byte, error) {
	// A type assertion with two results does not panic when the type does
	// not match; ok is simply false. In tests Stdin is a strings.Reader.
	if f, ok := c.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(c.Stdout, "Value for %s (not shown): ", name)
		// ReadPassword switches the terminal's echo off for one line.
		value, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(c.Stdout)
		if err != nil {
			return nil, fmt.Errorf("read secret value: %w", err)
		}
		return value, nil
	}

	// One byte beyond the limit, so an oversized value is rejected by the
	// daemon rather than silently cut off here.
	value, err := io.ReadAll(io.LimitReader(c.Stdin, 64<<10+1))
	if err != nil {
		return nil, fmt.Errorf("read secret value: %w", err)
	}
	// "echo value | liman secret set" and files saved by editors end in a
	// newline that is not part of the secret. Exactly one is removed, so a
	// value that really ends in blank lines can still be stored.
	value = bytes.TrimSuffix(value, []byte("\n"))
	value = bytes.TrimSuffix(value, []byte("\r"))
	return value, nil
}

func (c *CLI) secretList(ctx context.Context, appName string) int {
	list, err := c.API.Secrets(ctx, appName)
	if err != nil {
		return c.fail(err)
	}
	if len(list) == 0 {
		fmt.Fprintf(c.Stdout, "No secrets for %s. Add one with: liman secret set %s <NAME>\n", appName, appName)
		return exitOK
	}

	w := tabwriter.NewWriter(c.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tUPDATED")
	for _, sec := range list {
		fmt.Fprintf(w, "%s\t%s\n", sec.Name, sec.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
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
	// The two are released together. Apart, a command one of them knows
	// and the other does not fails, often only once it is used.
	mine := c.Version
	if mine == "" {
		mine = buildinfo.Version
	}
	if mine != health.Version && mine != "dev" && health.Version != "dev" {
		fmt.Fprintf(c.Stdout, "Note: this liman is %s and the daemon is %s; use the same version on both sides\n", mine, health.Version)
	}

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
