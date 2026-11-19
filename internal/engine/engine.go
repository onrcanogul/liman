// Package engine describes what liman needs from a container engine.
//
// It contains only types and an interface, no Docker code. The real
// implementation lives in internal/docker; tests use a fake. Keeping this
// boundary small is what lets the deploy logic be tested without Docker.
package engine

import (
	"context"
	"errors"
	"io"
)

// LabelApp is set on every container liman starts. Its value is the app
// name. Containers without this label are never listed or touched.
const LabelApp = "liman.app"

// Sentinel errors. Callers compare against them with errors.Is.
var (
	// ErrNotFound means no container with the given name exists.
	ErrNotFound = errors.New("container not found")

	// ErrNotManaged means a container with that name exists but was not
	// started by liman.
	ErrNotManaged = errors.New("container is not managed by liman")
)

// Spec describes a container to start. It is deliberately smaller than the
// app definition: it carries only what an engine needs to know.
type Spec struct {
	Name  string
	Image string

	// ContainerPort is the port the app listens on inside the container.
	ContainerPort int
	// HostPort is the port on 127.0.0.1 that is forwarded to ContainerPort.
	HostPort int

	Env map[string]string

	// MemoryBytes and CPUs are limits; zero means unlimited.
	MemoryBytes int64
	CPUs        float64
}

// Container is the observed state of a container.
type Container struct {
	ID    string
	Name  string
	Image string

	// State is a machine-readable state such as "running" or "exited".
	State string
	// Status is a human-readable summary such as "Up 5 minutes". It is only
	// filled in by List.
	Status string

	// HostPort is 0 when no port is published.
	HostPort int

	// ExitCode and OOMKilled are only meaningful when State is "exited".
	// They are only filled in by Inspect.
	ExitCode  int
	OOMKilled bool
}

// Running reports whether the container is currently running.
func (c Container) Running() bool {
	return c.State == "running"
}

// Engine is the set of container operations liman uses.
//
// Every method takes a context as its first argument. Cancelling the context
// (for example with Ctrl+C) aborts the operation.
type Engine interface {
	// Pull downloads image. Human-readable progress is written to progress.
	Pull(ctx context.Context, image string, progress io.Writer) error

	// Start creates and starts a container and returns its ID.
	Start(ctx context.Context, spec Spec) (string, error)

	// Inspect returns the state of the managed container called name.
	Inspect(ctx context.Context, name string) (Container, error)

	// Remove stops and deletes the managed container called name.
	Remove(ctx context.Context, name string) error

	// List returns all managed containers, running or not.
	List(ctx context.Context) ([]Container, error)

	// Logs copies the container's output to stdout and stderr. With follow
	// it keeps streaming until the context is cancelled or the container
	// stops.
	Logs(ctx context.Context, name string, follow bool, stdout, stderr io.Writer) error
}
