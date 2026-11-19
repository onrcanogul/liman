// Package cli implements the liman command-line interface.
package cli

import (
	"fmt"
	"io"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/buildinfo"
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
  plan <file>   Validate an app definition and list the deploy steps
  version       Print version information
  help          Show this help
`

// Run executes the CLI with the given arguments (without the program name)
// and returns the process exit code. Output streams are passed in so tests
// can capture them.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}

	// args[0] selects the command; the rest belongs to that command.
	switch args[0] {
	case "plan":
		return runPlan(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, buildinfo.String())
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "liman: unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
}

func runPlan(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: liman plan <file>")
		return exitUsage
	}
	path := args[0]

	a, err := app.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "liman: %v\n", err)
		return exitError
	}

	fmt.Fprintf(stdout, "Plan for %s (%s):\n", a.Name, path)
	for i, step := range plan.Build(a) {
		fmt.Fprintf(stdout, "  %d. %s\n", i+1, step.Description)
	}
	return exitOK
}
