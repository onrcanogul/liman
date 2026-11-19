// Package cli implements the liman command-line interface.
package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/buildinfo"
	"github.com/onrcanogul/liman/internal/deploy"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/history"
	"github.com/onrcanogul/liman/internal/pipeline"
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
  version              Print version information
  help                 Show this help
`

// CLI holds everything a command needs from the outside world. main fills
// it with the real streams and the real Docker connection; tests fill it
// with buffers and a fake engine.
type CLI struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Connect returns the container engine. It is a function rather than a
	// ready-made engine so that commands which do not need Docker (plan,
	// history, version) work even when Docker is not running.
	Connect func(ctx context.Context) (engine.Engine, error)

	// History is where executed steps are recorded.
	History history.Store
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
		return c.runHistory(rest)
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

	// Validate the file before connecting to Docker: a typo in app.yaml
	// should be reported even when Docker is down.
	a, err := app.Load(fs.Arg(0))
	if err != nil {
		return c.fail(err)
	}

	var eng engine.Engine
	opts := deploy.Options{DryRun: *dryRun}

	if *dryRun {
		fmt.Fprintf(c.Stdout, "Dry run for %s: nothing will be changed.\n\n", a.Name)
	} else {
		// A dry run needs no engine, so Docker is only contacted here.
		if eng, err = c.Connect(ctx); err != nil {
			return c.fail(err)
		}
		opts.OnEvent = c.recordEvent(a.Name)
		if *stepMode {
			opts.Confirm = c.confirm()
		}
	}

	res, err := deploy.New(eng, c.Stdout).Deploy(ctx, a, opts)
	if err != nil {
		return c.fail(err)
	}

	if !*dryRun {
		fmt.Fprintf(c.Stdout, "\n%s is running at http://127.0.0.1:%d (release %s)\n", a.Name, res.HostPort, res.Release)
	}
	return exitOK
}

// recordEvent returns the callback that writes pipeline events of one app
// to the history.
func (c *CLI) recordEvent(appName string) func(release string, ev pipeline.Event) {
	// warned is captured by the closure below and keeps its value between
	// calls, so the warning is printed once instead of once per step.
	warned := false

	return func(release string, ev pipeline.Event) {
		rec := history.Event{
			Time:       ev.Started,
			App:        appName,
			Release:    release,
			Step:       ev.Step,
			Action:     string(ev.Action),
			OK:         ev.Err == nil,
			DurationMS: ev.Duration.Milliseconds(),
		}
		if ev.Err != nil {
			rec.Error = ev.Err.Error()
		}

		// Failing to write the record must not fail a deploy that is
		// otherwise fine, but it must not go unnoticed either.
		if err := c.History.Append(rec); err != nil && !warned {
			warned = true
			fmt.Fprintf(c.Stderr, "liman: warning: history is not being recorded: %v\n", err)
		}
	}
}

// confirm returns the callback that asks the user before each step.
func (c *CLI) confirm() func(pipeline.Step) (bool, error) {
	// One reader for all questions: a bufio.Reader may read ahead, so
	// creating a new one per question could lose input.
	in := bufio.NewReader(c.Stdin)

	return func(pipeline.Step) (bool, error) {
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

	eng, err := c.Connect(ctx)
	if err != nil {
		return c.fail(err)
	}

	containers, err := eng.List(ctx)
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
	fmt.Fprintln(w, "APP\tCONTAINER\tIMAGE\tSTATE\tADDRESS\tSTATUS")
	for _, ctr := range containers {
		address := "-"
		if ctr.HostPort != 0 {
			address = fmt.Sprintf("127.0.0.1:%d", ctr.HostPort)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", ctr.App, ctr.Name, ctr.Image, ctr.State, address, ctr.Status)
	}
	if err := w.Flush(); err != nil {
		return c.fail(err)
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
	appName := fs.Arg(0)

	eng, containers, err := c.containersOf(ctx, appName)
	if err != nil {
		return c.fail(err)
	}
	// The list is ordered oldest first; the newest container is the one
	// whose logs are interesting.
	newest := containers[len(containers)-1]

	err = eng.Logs(ctx, newest.Name, *follow, c.Stdout, c.Stderr)
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
	appName := args[0]

	eng, containers, err := c.containersOf(ctx, appName)
	if err != nil {
		return c.fail(err)
	}

	for _, ctr := range containers {
		if err := eng.Remove(ctx, ctr.Name); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.Stdout, "Stopped and removed %s\n", ctr.Name)
	}
	return exitOK
}

// containersOf connects to the engine and returns the containers of an app.
// It fails with engine.ErrNotFound when the app has none, so callers can
// rely on the slice being non-empty.
func (c *CLI) containersOf(ctx context.Context, appName string) (engine.Engine, []engine.Container, error) {
	eng, err := c.Connect(ctx)
	if err != nil {
		return nil, nil, err
	}

	all, err := eng.List(ctx)
	if err != nil {
		return nil, nil, err
	}

	containers := engine.ForApp(all, appName)
	if len(containers) == 0 {
		return nil, nil, fmt.Errorf("%s: %w", appName, engine.ErrNotFound)
	}
	return eng, containers, nil
}

func (c *CLI) runHistory(args []string) int {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("n", 20, "number of entries to show")

	if err := fs.Parse(args); err != nil || fs.NArg() > 1 || *limit < 1 {
		return c.usageError("liman history [-n N] [app]")
	}
	appName := fs.Arg(0) // empty when no app was given

	events, err := c.History.Read()
	if err != nil {
		return c.fail(err)
	}

	if appName != "" {
		// Filter in place: kept reuses the backing array of events, which
		// is safe because the write index never overtakes the read index.
		kept := events[:0]
		for _, ev := range events {
			if ev.App == appName {
				kept = append(kept, ev)
			}
		}
		events = kept
	}

	if len(events) == 0 {
		fmt.Fprintln(c.Stdout, "No history yet.")
		return exitOK
	}
	// Keep only the newest entries.
	if len(events) > *limit {
		events = events[len(events)-*limit:]
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

// fail prints err and returns the failure exit code.
func (c *CLI) fail(err error) int {
	fmt.Fprintf(c.Stderr, "liman: %v\n", err)
	return exitError
}

func (c *CLI) usageError(synopsis string) int {
	fmt.Fprintf(c.Stderr, "usage: %s\n", synopsis)
	return exitUsage
}
