// Command liman is the CLI for the liman PaaS.
package main

import (
	"os"

	"github.com/onrcanogul/liman/internal/cli"
)

func main() {
	// All logic lives in internal/cli so it can be tested without spawning a
	// process. main only wires the real OS streams and exit code.
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
