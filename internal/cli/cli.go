// Package cli implements the liman command-line interface.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/buildinfo"
	"github.com/onrcanogul/liman/internal/deploy"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/plan"
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
  plan <file>        Validate an app definition and list the deploy steps
  deploy <file>      Pull the image and start the app
  ps                 List apps managed by liman
  logs [-f] <app>    Show the logs of an app (-f keeps following)
  stop <app>         Stop and remove an app
  version            Print version information
  help               Show this help
`

// CLI holds everything a command needs from the outside world. main fills
// it with the real streams and the real Docker connection; tests fill it
// with buffers and a fake engine.
type CLI struct {
	Stdout io.Writer
	Stderr io.Writer

	// Connect returns the container engine. It is a function rather than a
	// ready-made engine so that commands which do not need Docker (plan,
	// version) work even when Docker is not running.
	Connect func(ctx context.Context) (engine.Engine, error)
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
	case "plan":
		return c.runPlan(rest)
	case "deploy":
		return c.runDeploy(ctx, rest)
	case "ps":
		return c.runPs(ctx, rest)
	case "logs":
		return c.runLogs(ctx, rest)
	case "stop":
		return c.runStop(ctx, rest)
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

func (c *CLI) runPlan(args []string) int {
	if len(args) != 1 {
		return c.usageError("liman plan <file>")
	}
	path := args[0]

	a, err := app.Load(path)
	if err != nil {
		return c.fail(err)
	}

	fmt.Fprintf(c.Stdout, "Plan for %s (%s):\n", a.Name, path)
	for i, step := range plan.Build(a) {
		fmt.Fprintf(c.Stdout, "  %d. %s\n", i+1, step.Description)
	}
	return exitOK
}

func (c *CLI) runDeploy(ctx context.Context, args []string) int {
	if len(args) != 1 {
		return c.usageError("liman deploy <file>")
	}

	// Validate the file before connecting to Docker: a typo in app.yaml
	// should be reported even when Docker is down.
	a, err := app.Load(args[0])
	if err != nil {
		return c.fail(err)
	}

	eng, err := c.Connect(ctx)
	if err != nil {
		return c.fail(err)
	}

	res, err := deploy.New(eng, c.Stdout).Deploy(ctx, a)
	if err != nil {
		return c.fail(err)
	}

	fmt.Fprintf(c.Stdout, "\n%s is running at http://127.0.0.1:%d\n", a.Name, res.HostPort)
	return exitOK
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
	fmt.Fprintln(w, "NAME\tIMAGE\tSTATE\tADDRESS\tSTATUS")
	for _, ctr := range containers {
		address := "-"
		if ctr.HostPort != 0 {
			address = fmt.Sprintf("127.0.0.1:%d", ctr.HostPort)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ctr.Name, ctr.Image, ctr.State, address, ctr.Status)
	}
	if err := w.Flush(); err != nil {
		return c.fail(err)
	}
	return exitOK
}

func (c *CLI) runLogs(ctx context.Context, args []string) int {
	// Each command gets its own FlagSet, so flags of one command do not
	// leak into another. ContinueOnError makes Parse return the error
	// instead of calling os.Exit, which would bypass our exit codes.
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	follow := fs.Bool("f", false, "keep following the log output")

	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return c.usageError("liman logs [-f] <app>")
	}
	// fs.Arg(0) is the first argument that is not a flag.
	name := fs.Arg(0)

	eng, err := c.Connect(ctx)
	if err != nil {
		return c.fail(err)
	}

	err = eng.Logs(ctx, name, *follow, c.Stdout, c.Stderr)
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
	name := args[0]

	eng, err := c.Connect(ctx)
	if err != nil {
		return c.fail(err)
	}

	if err := eng.Remove(ctx, name); err != nil {
		return c.fail(err)
	}

	fmt.Fprintf(c.Stdout, "Stopped and removed %s\n", name)
	return exitOK
}

// fail prints err and returns the failure exit code.
func (c *CLI) fail(err error) int {
	// errors.Is walks the chain of wrapped errors, so this matches no
	// matter how much context was added on the way up.
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(c.Stderr, "liman: interrupted")
		return exitError
	}
	fmt.Fprintf(c.Stderr, "liman: %v\n", err)
	return exitError
}

func (c *CLI) usageError(synopsis string) int {
	fmt.Fprintf(c.Stderr, "usage: %s\n", synopsis)
	return exitUsage
}
