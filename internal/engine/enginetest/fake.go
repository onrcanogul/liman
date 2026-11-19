// Package enginetest provides an in-memory engine.Engine for tests.
package enginetest

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/onrcanogul/liman/internal/engine"
)

// Fake is an engine that keeps containers in a map and records every call.
// Set the Err fields to make the corresponding method fail.
type Fake struct {
	// Calls lists the operations in the order they happened, for example
	// "pull nginx:1.27" or "start web-r1".
	Calls []string

	// Containers holds the current state, keyed by container name.
	Containers map[string]engine.Container

	// LogOutput is what Logs writes to stdout.
	LogOutput string

	PullErr   error
	StartErr  error
	RemoveErr error

	// StartState is the state a container gets when started. It defaults
	// to "running"; set it to "exited" to simulate an app that crashes.
	StartState string
	// StartExitCode and StartOOMKilled apply when StartState is "exited".
	StartExitCode  int
	StartOOMKilled bool

	// Specs records the spec of every started container.
	Specs []engine.Spec

	// clock gives every added container a later creation time than the
	// previous one, so ordering is deterministic.
	clock time.Time
}

var _ engine.Engine = (*Fake)(nil)

// New returns an empty fake engine.
func New() *Fake {
	return &Fake{
		Containers: map[string]engine.Container{},
		clock:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// Add puts an existing container into the fake, as if it had been started
// earlier. Name and App are required; State defaults to "running".
func (f *Fake) Add(c engine.Container) {
	if c.State == "" {
		c.State = "running"
	}
	if c.ID == "" {
		c.ID = "fake-" + c.Name
	}
	f.clock = f.clock.Add(time.Second)
	c.Created = f.clock
	f.Containers[c.Name] = c
}

// Pull implements engine.Engine.
func (f *Fake) Pull(_ context.Context, image string, progress io.Writer) error {
	f.Calls = append(f.Calls, "pull "+image)
	if f.PullErr != nil {
		return f.PullErr
	}
	_, _ = fmt.Fprintf(progress, "Status: Downloaded newer image for %s\n", image)
	return nil
}

// Start implements engine.Engine.
func (f *Fake) Start(_ context.Context, spec engine.Spec) (string, error) {
	f.Calls = append(f.Calls, "start "+spec.Name)
	if f.StartErr != nil {
		return "", f.StartErr
	}
	if _, exists := f.Containers[spec.Name]; exists {
		return "", fmt.Errorf("container name %q is already in use", spec.Name)
	}

	state := f.StartState
	if state == "" {
		state = "running"
	}
	f.Specs = append(f.Specs, spec)
	f.Add(engine.Container{
		Name:      spec.Name,
		App:       spec.App,
		Image:     spec.Image,
		State:     state,
		Status:    state,
		HostPort:  spec.HostPort,
		ExitCode:  f.StartExitCode,
		OOMKilled: f.StartOOMKilled,
	})
	return f.Containers[spec.Name].ID, nil
}

// Inspect implements engine.Engine.
func (f *Fake) Inspect(_ context.Context, ref string) (engine.Container, error) {
	ctr, ok := f.Containers[ref]
	if !ok {
		return engine.Container{}, fmt.Errorf("%s: %w", ref, engine.ErrNotFound)
	}
	return ctr, nil
}

// Remove implements engine.Engine.
func (f *Fake) Remove(_ context.Context, ref string) error {
	if _, ok := f.Containers[ref]; !ok {
		return fmt.Errorf("%s: %w", ref, engine.ErrNotFound)
	}
	f.Calls = append(f.Calls, "remove "+ref)
	if f.RemoveErr != nil {
		return f.RemoveErr
	}
	delete(f.Containers, ref)
	return nil
}

// List implements engine.Engine.
func (f *Fake) List(_ context.Context) ([]engine.Container, error) {
	out := make([]engine.Container, 0, len(f.Containers))
	for _, ctr := range f.Containers {
		out = append(out, ctr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Created.Before(out[j].Created)
	})
	return out, nil
}

// Logs implements engine.Engine.
func (f *Fake) Logs(_ context.Context, ref string, _ bool, stdout, _ io.Writer) error {
	if _, ok := f.Containers[ref]; !ok {
		return fmt.Errorf("%s: %w", ref, engine.ErrNotFound)
	}
	f.Calls = append(f.Calls, "logs "+ref)
	_, err := io.WriteString(stdout, f.LogOutput)
	return err
}
