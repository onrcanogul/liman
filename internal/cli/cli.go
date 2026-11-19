// Package cli implements the liman command-line interface.
package cli

import (
	"fmt"
	"io"

	"github.com/onrcanogul/liman/internal/buildinfo"
)

// Exit codes returned by Run. Following the common Unix convention, 2 means
// the command line itself was wrong, as opposed to the command failing.
const (
	exitOK    = 0
	exitUsage = 2
)

const usage = `liman is a small PaaS for a single server.

Usage:
  liman <command>

Commands:
  version   Print version information
  help      Show this help
`

// Run executes the CLI with the given arguments (without the program name)
// and returns the process exit code. Output streams are passed in so tests
// can capture them.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}

	switch args[0] {
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
