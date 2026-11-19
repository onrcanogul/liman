// Package plan turns an app definition into the ordered list of steps a
// deploy consists of.
//
// For now a plan is only a description. In stage 3 each step will also know
// how to execute and undo itself.
package plan

import (
	"fmt"
	"strings"

	"github.com/onrcanogul/liman/internal/app"
)

// Step is one named unit of work in a deploy.
type Step struct {
	Description string
}

// Build returns the steps needed to deploy a from scratch. It does not look
// at what is currently running; that arrives with the Docker integration in
// stage 2.
func Build(a *app.App) []Step {
	return []Step{
		{Description: fmt.Sprintf("Pull image %s", a.Image)},
		{Description: "Allocate a free host port"},
		{Description: startDescription(a)},
		{Description: healthDescription(a)},
		{Description: fmt.Sprintf("Write Nginx config for %s", a.Domain)},
		{Description: "Reload Nginx"},
	}
}

func startDescription(a *app.App) string {
	var limits []string
	if a.Resources.Memory != "" {
		limits = append(limits, "memory "+a.Resources.Memory)
	}
	if a.Resources.CPUs > 0 {
		limits = append(limits, fmt.Sprintf("cpus %g", a.Resources.CPUs))
	}
	if len(limits) == 0 {
		limits = []string{"no resource limits"}
	}

	return fmt.Sprintf("Start container %s on port %d (%s, %d env vars)",
		a.Name, a.Port, strings.Join(limits, ", "), len(a.Env))
}

func healthDescription(a *app.App) string {
	if a.Healthcheck == nil {
		return "Health check: wait until the container is running"
	}
	return fmt.Sprintf("Health check: GET %s on port %d", a.Healthcheck.Path, a.Port)
}
