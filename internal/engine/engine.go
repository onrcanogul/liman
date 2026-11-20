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
	"io/fs"
	"time"
)

// LabelApp is set on every container liman starts. Its value is the app
// name. Containers without this label are never listed or touched.
const LabelApp = "liman.app"

// Further labels carry what liman needs to know about a container later,
// when the app definition it was started from is no longer at hand. Labels
// are the right place for this: they live and die with the container, and
// they survive a restart of the daemon. Nothing secret may go into a label;
// anyone who can inspect the container can read it.
const (
	// LabelRelease is the release the container belongs to.
	LabelRelease = "liman.release"
	// LabelHealthPath is the HTTP path to probe, or absent when the app
	// defines no health check.
	LabelHealthPath = "liman.healthcheck"
	// LabelDomain is the domain the app is served on. A rollback needs it
	// to point the proxy back at an older container.
	LabelDomain = "liman.domain"
)

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
	// App is the app this container belongs to; it becomes the LabelApp
	// label. Several containers can belong to one app while a new release
	// is replacing an old one.
	App string
	// Name is the container name. It must be unique on the host.
	Name  string
	Image string

	// ContainerPort is the port the app listens on inside the container.
	ContainerPort int
	// HostPort is the port on 127.0.0.1 that is forwarded to ContainerPort.
	HostPort int

	Env map[string]string

	// Files are put into the container after it is created and before it
	// starts, with their directories. They are part of the container from
	// then on: a restart keeps them.
	Files []File

	// MemoryBytes and CPUs are limits; zero means unlimited.
	MemoryBytes int64
	CPUs        float64

	// Release, HealthPath and Domain are stored as labels on the container.
	Release    string
	HealthPath string
	Domain     string
}

// File is a file to put into a container.
type File struct {
	// Path is absolute, inside the container, such as /run/secrets/KEY.
	Path    string
	Content []byte
	Mode    fs.FileMode
}

// Container is the observed state of a container.
type Container struct {
	ID string
	// Name is the container name, App the app it belongs to.
	Name  string
	App   string
	Image string

	// State is a machine-readable state such as "running" or "exited".
	State string
	// Status is a human-readable summary such as "Up 5 minutes". It is only
	// filled in by List.
	Status string

	// HostPort is 0 when no port is published.
	HostPort int

	// Created is when the container was created. It is only filled in by
	// List.
	Created time.Time

	// ExitCode and OOMKilled are only meaningful when State is "exited".
	// They are only filled in by Inspect.
	ExitCode  int
	OOMKilled bool

	// Release, HealthPath and Domain come from the container's labels.
	Release    string
	HealthPath string
	Domain     string
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

	// Inspect returns the state of a managed container. ref is a container
	// name or ID.
	Inspect(ctx context.Context, ref string) (Container, error)

	// Remove stops and deletes a managed container.
	Remove(ctx context.Context, ref string) error

	// Restart stops a managed container if it is running and starts it
	// again. It also brings back a container that has exited.
	Restart(ctx context.Context, ref string) error

	// Stop stops a managed container without deleting it, so it can be
	// started again later. Stopping a stopped container is not an error.
	Stop(ctx context.Context, ref string) error

	// List returns all managed containers, running or not, ordered by app
	// and then by creation time, oldest first.
	List(ctx context.Context) ([]Container, error)

	// Logs copies the container's output to stdout and stderr. With follow
	// it keeps streaming until the context is cancelled or the container
	// stops.
	Logs(ctx context.Context, ref string, follow bool, stdout, stderr io.Writer) error
}

// ForApp returns the containers in list that belong to app, keeping their
// order. Because List orders by creation time, the last element is the
// newest container of the app.
func ForApp(list []Container, app string) []Container {
	var out []Container
	for _, c := range list {
		if c.App == app {
			out = append(out, c)
		}
	}
	return out
}
