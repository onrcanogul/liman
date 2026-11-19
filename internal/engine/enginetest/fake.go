// Package enginetest provides an in-memory engine.Engine for tests.
package enginetest

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/onrcanogul/liman/internal/engine"
)

// Fake is an engine that keeps containers in a map and records every call.
// Set the Err fields to make the corresponding method fail.
type Fake struct {
	// Calls lists the operations in the order they happened, for example
	// "pull nginx:1.27" or "start web".
	Calls []string

	// Containers holds the current state, keyed by name.
	Containers map[string]engine.Container

	// LogOutput is what Logs writes to stdout.
	LogOutput string

	PullErr  error
	StartErr error

	// StartState is the state a container gets when started. It defaults
	// to "running"; set it to "exited" to simulate an app that crashes.
	StartState string
	// StartExitCode and StartOOMKilled apply when StartState is "exited".
	StartExitCode  int
	StartOOMKilled bool

	// Specs records the spec of every started container.
	Specs []engine.Spec
}

var _ engine.Engine = (*Fake)(nil)

// New returns an empty fake engine.
func New() *Fake {
	return &Fake{Containers: map[string]engine.Container{}}
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
	id := "fake-" + spec.Name
	f.Specs = append(f.Specs, spec)
	f.Containers[spec.Name] = engine.Container{
		ID:        id,
		Name:      spec.Name,
		Image:     spec.Image,
		State:     state,
		Status:    state,
		HostPort:  spec.HostPort,
		ExitCode:  f.StartExitCode,
		OOMKilled: f.StartOOMKilled,
	}
	return id, nil
}

// Inspect implements engine.Engine.
func (f *Fake) Inspect(_ context.Context, name string) (engine.Container, error) {
	ctr, ok := f.Containers[name]
	if !ok {
		return engine.Container{}, fmt.Errorf("%s: %w", name, engine.ErrNotFound)
	}
	return ctr, nil
}

// Remove implements engine.Engine.
func (f *Fake) Remove(_ context.Context, name string) error {
	if _, ok := f.Containers[name]; !ok {
		return fmt.Errorf("%s: %w", name, engine.ErrNotFound)
	}
	f.Calls = append(f.Calls, "remove "+name)
	delete(f.Containers, name)
	return nil
}

// List implements engine.Engine.
func (f *Fake) List(_ context.Context) ([]engine.Container, error) {
	out := make([]engine.Container, 0, len(f.Containers))
	for _, ctr := range f.Containers {
		out = append(out, ctr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Logs implements engine.Engine.
func (f *Fake) Logs(_ context.Context, name string, _ bool, stdout, _ io.Writer) error {
	if _, ok := f.Containers[name]; !ok {
		return fmt.Errorf("%s: %w", name, engine.ErrNotFound)
	}
	_, err := io.WriteString(stdout, f.LogOutput)
	return err
}
