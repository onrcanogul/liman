// Package enginetest provides an in-memory engine.Engine for tests.
package enginetest

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/onrcanogul/liman/internal/engine"
)

// Fake is an engine that keeps containers in a map and records every call.
// Set the Err fields to make the corresponding method fail.
//
// Its methods may be called from several goroutines at once, as a server
// handling concurrent requests does. Tests that read or change the fields
// directly must do so while no request is in flight.
type Fake struct {
	// mu guards every field below while a method runs.
	mu sync.Mutex

	// RecordCalls can be switched off for load tests, where an ever-growing
	// call list would be measured as a memory leak. New turns it on.
	RecordCalls bool

	// Calls lists the operations in the order they happened, for example
	// "pull nginx:1.27" or "start web-r1".
	Calls []string

	// Containers holds the current state, keyed by container name.
	Containers map[string]engine.Container

	// LogOutput is what Logs writes to stdout.
	LogOutput string

	PullErr    error
	StartErr   error
	RemoveErr  error
	RestartErr error
	StopErr    error

	// AfterRestart is the state a container gets when restarted. It
	// defaults to "running"; "exited" simulates an app that crashes again
	// right away.
	AfterRestart string
	// RestartCount counts the restarts per container name.
	RestartCount map[string]int

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
		RecordCalls:  true,
		RestartCount: map[string]int{},
		Containers:   map[string]engine.Container{},
		clock:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// Add puts an existing container into the fake, as if it had been started
// earlier. Name and App are required; State defaults to "running".
func (f *Fake) Add(c engine.Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.add(c)
}

// record notes a call. The caller holds mu.
func (f *Fake) record(call string) {
	if f.RecordCalls {
		f.Calls = append(f.Calls, call)
	}
}

// add is Add for callers that already hold mu.
func (f *Fake) add(c engine.Container) {
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
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("pull " + image)
	if f.PullErr != nil {
		return f.PullErr
	}
	_, _ = fmt.Fprintf(progress, "Status: Downloaded newer image for %s\n", image)
	return nil
}

// Start implements engine.Engine.
func (f *Fake) Start(_ context.Context, spec engine.Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("start " + spec.Name)
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
	if f.RecordCalls {
		f.Specs = append(f.Specs, spec)
	}
	f.add(engine.Container{
		Name:      spec.Name,
		App:       spec.App,
		Image:     spec.Image,
		State:     state,
		Status:    state,
		HostPort:  spec.HostPort,
		ExitCode:  f.StartExitCode,
		OOMKilled: f.StartOOMKilled,

		Release:    spec.Release,
		HealthPath: spec.HealthPath,
		Domain:     spec.Domain,
	})
	return f.Containers[spec.Name].ID, nil
}

// Inspect implements engine.Engine.
func (f *Fake) Inspect(_ context.Context, ref string) (engine.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ctr, ok := f.Containers[ref]
	if !ok {
		return engine.Container{}, fmt.Errorf("%s: %w", ref, engine.ErrNotFound)
	}
	return ctr, nil
}

// Remove implements engine.Engine.
func (f *Fake) Remove(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.Containers[ref]; !ok {
		return fmt.Errorf("%s: %w", ref, engine.ErrNotFound)
	}
	f.record("remove " + ref)
	if f.RemoveErr != nil {
		return f.RemoveErr
	}
	delete(f.Containers, ref)
	return nil
}

// Restart implements engine.Engine.
func (f *Fake) Restart(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	ctr, ok := f.Containers[ref]
	if !ok {
		return fmt.Errorf("%s: %w", ref, engine.ErrNotFound)
	}
	f.record("restart " + ref)
	if f.RestartErr != nil {
		return f.RestartErr
	}

	ctr.State = f.AfterRestart
	if ctr.State == "" {
		ctr.State = "running"
	}
	f.Containers[ref] = ctr
	f.RestartCount[ref]++
	return nil
}

// Stop implements engine.Engine.
func (f *Fake) Stop(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	ctr, ok := f.Containers[ref]
	if !ok {
		return fmt.Errorf("%s: %w", ref, engine.ErrNotFound)
	}
	f.record("stop " + ref)
	if f.StopErr != nil {
		return f.StopErr
	}
	ctr.State, ctr.Status = "exited", "exited"
	f.Containers[ref] = ctr
	return nil
}

// Running returns the names of the containers that are running, sorted.
func (f *Fake) Running() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var names []string
	for name, ctr := range f.Containers {
		if ctr.Running() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// State returns the state of a container, or "" if it does not exist.
func (f *Fake) State(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Containers[name].State
}

// SetState changes the state of a container, as a crash would.
func (f *Fake) SetState(name, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ctr := f.Containers[name]
	ctr.State = state
	f.Containers[name] = ctr
}

// Restarts returns how often a container has been restarted.
func (f *Fake) Restarts(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.RestartCount[name]
}

// List implements engine.Engine.
func (f *Fake) List(_ context.Context) ([]engine.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

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
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.Containers[ref]; !ok {
		return fmt.Errorf("%s: %w", ref, engine.ErrNotFound)
	}
	f.record("logs " + ref)
	_, err := io.WriteString(stdout, f.LogOutput)
	return err
}
