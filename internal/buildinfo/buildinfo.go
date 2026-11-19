// Package buildinfo holds version metadata that is injected at build time.
package buildinfo

import "fmt"

// These variables are overwritten by the linker, see the build target in the
// Makefile:
//
//	go build -ldflags "-X github.com/onrcanogul/liman/internal/buildinfo.Version=v0.1.0"
//
// They must be package-level string variables (not constants) for -X to work.
// The defaults below are what a plain `go run` or `go build` produces.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns the one-line version description printed by `liman version`.
func String() string {
	return fmt.Sprintf("liman %s (commit %s, built %s)", Version, Commit, Date)
}
