//go:build integration

// The build constraint above keeps this file out of normal builds and test
// runs. It needs a running Docker daemon and network access, so it only runs
// when asked for explicitly:
//
//	go test -tags integration ./internal/docker
//	make test-integration

package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/engine"
)

const testImage = "traefik/whoami:v1.10"

func TestContainerLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := Connect(ctx)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	const name = "liman-integration-test"
	// Leftovers from an aborted earlier run would make Start fail.
	_ = c.Remove(ctx, name)
	// t.Cleanup runs even when the test fails halfway through.
	t.Cleanup(func() { _ = c.Remove(context.Background(), name) })

	var progress bytes.Buffer
	if err := c.Pull(ctx, testImage, &progress); err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if !strings.Contains(progress.String(), "Status:") {
		t.Errorf("pull progress = %q, want a status line", progress.String())
	}

	spec := engine.Spec{
		Name:          name,
		Image:         testImage,
		ContainerPort: 80,
		HostPort:      39999,
		Env:           map[string]string{"WHOAMI_NAME": "integration"},
		MemoryBytes:   64 << 20,
		CPUs:          0.5,
	}
	id, err := c.Start(ctx, spec)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	info, err := c.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if info.ID != id || !info.Running() || info.HostPort != 39999 {
		t.Errorf("Inspect() = %+v, want running container %s on port 39999", info, id)
	}

	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	found := false
	for _, ctr := range list {
		if ctr.Name == name {
			found = true
			if ctr.HostPort != 39999 {
				t.Errorf("List() host port = %d, want 39999", ctr.HostPort)
			}
		}
	}
	if !found {
		t.Errorf("List() does not contain %s", name)
	}

	// A second container with the same name must be rejected.
	if _, err := c.Start(ctx, spec); err == nil {
		t.Error("Start() with a duplicate name succeeded, want an error")
	}

	// whoami writes its startup line to stderr.
	var logs bytes.Buffer
	if err := c.Logs(ctx, name, false, &logs, &logs); err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	if !strings.Contains(logs.String(), "Starting up") {
		t.Errorf("logs = %q, want the startup line", logs.String())
	}

	if err := c.Remove(ctx, name); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := c.Inspect(ctx, name); !errors.Is(err, engine.ErrNotFound) {
		t.Errorf("Inspect() after Remove error = %v, want ErrNotFound", err)
	}
}

func TestPullUnknownImage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	c, err := Connect(ctx)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	if err := c.Pull(ctx, "traefik/whoami:v0.0.0-does-not-exist", io.Discard); err == nil {
		t.Error("Pull() of an unknown tag succeeded, want an error")
	}
}
