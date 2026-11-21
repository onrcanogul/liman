//go:build integration

// The build constraint above keeps this file out of normal builds and test
// runs. It needs a running Docker daemon and network access, so it only runs
// when asked for explicitly:
//
//	go test -tags integration ./internal/docker
//	make test-integration

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/onrcanogul/liman/internal/engine"
)

const testImage = "traefik/whoami:v1.10"

func TestContainerLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := Connect(ctx, testEngine())
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
	// Docker ends a pull with a summary ("Status: Downloaded newer image");
	// Podman's compatible API sends only per-layer lines, which are not
	// shown.
	if testEngine().Name == "Docker" && !strings.Contains(progress.String(), "Status:") {
		t.Errorf("pull progress = %q, want a status line", progress.String())
	}

	spec := engine.Spec{
		App:           "liman-integration",
		Name:          name,
		Image:         testImage,
		ContainerPort: 80,
		HostPort:      39999,
		Env:           map[string]string{"WHOAMI_NAME": "integration"},
		MemoryBytes:   64 << 20,
		CPUs:          0.5,
		Release:       "r1",
		HealthPath:    "/health",
		Domain:        "integration.example.com",
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
	if info.Name != name || info.App != "liman-integration" {
		t.Errorf("Inspect() name = %q app = %q, want %q and liman-integration", info.Name, info.App, name)
	}

	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	mine := engine.ForApp(list, "liman-integration")
	if len(mine) != 1 {
		t.Fatalf("List() has %d containers for the app, want 1", len(mine))
	}
	if mine[0].Name != name || mine[0].HostPort != 39999 || mine[0].Created.IsZero() {
		t.Errorf("List() entry = %+v, want %s on port 39999 with a creation time", mine[0], name)
	}

	if mine[0].Release != "r1" || mine[0].HealthPath != "/health" {
		t.Errorf("List() labels = release %q, health path %q; want r1 and /health", mine[0].Release, mine[0].HealthPath)
	}

	// Restart must revive a container that was killed from outside.
	if _, err := c.api.ContainerKill(ctx, id, client.ContainerKillOptions{}); err != nil {
		t.Fatalf("kill the container: %v", err)
	}
	if info, _ := c.Inspect(ctx, name); info.Running() {
		t.Fatal("the container is still running after being killed")
	}
	if err := c.Restart(ctx, name); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if info, err := c.Inspect(ctx, name); err != nil || !info.Running() || info.HostPort != 39999 {
		t.Errorf("after Restart() = %+v, %v; want it running on the same port", info, err)
	}

	// Stop keeps the container, and Restart brings it back on its port:
	// together they are what a rollback relies on.
	if err := c.Stop(ctx, name); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if info, err := c.Inspect(ctx, name); err != nil || info.Running() || info.HostPort != 39999 || info.Domain != "integration.example.com" {
		t.Errorf("after Stop() = %+v, %v; want it kept, stopped, with its port and domain known", info, err)
	}
	if err := c.Stop(ctx, name); err != nil {
		t.Errorf("Stop() of a stopped container error = %v, want none", err)
	}
	if err := c.Restart(ctx, name); err != nil {
		t.Fatalf("Restart() after Stop() error = %v", err)
	}
	if info, _ := c.Inspect(ctx, name); !info.Running() {
		t.Error("the container is not running after being started again")
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

	c, err := Connect(ctx, testEngine())
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	if err := c.Pull(ctx, "traefik/whoami:v0.0.0-does-not-exist", io.Discard); err == nil {
		t.Error("Pull() of an unknown tag succeeded, want an error")
	}
}

func TestFilesAreInTheContainerAndNotInItsConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := Connect(ctx, testEngine())
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	const name = "liman-integration-files"
	_ = c.Remove(ctx, name)
	t.Cleanup(func() { _ = c.Remove(context.Background(), name) })
	if err := c.Pull(ctx, testImage, io.Discard); err != nil {
		t.Fatal(err)
	}

	// whoami is built FROM scratch: it has no /run, so the archive's own
	// directory entries are what make the path.
	const value = "postgres://user:hunter2@db/app"
	if _, err := c.Start(ctx, engine.Spec{
		App: "liman-integration", Name: name, Image: testImage, ContainerPort: 80, HostPort: 39998,
		Env:   map[string]string{"DATABASE_URL_FILE": "/run/secrets/DATABASE_URL"},
		Files: []engine.File{{Path: "/run/secrets/DATABASE_URL", Content: []byte(value), Mode: 0o444}},
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	read := func() (string, *tar.Header) {
		t.Helper()
		res, err := c.api.CopyFromContainer(ctx, name, client.CopyFromContainerOptions{SourcePath: "/run/secrets/DATABASE_URL"})
		if err != nil {
			t.Fatalf("read the file back: %v", err)
		}
		defer func() { _ = res.Content.Close() }()
		tr := tar.NewReader(res.Content)
		h, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		return string(body), h
	}
	body, h := read()
	if body != value || h.Mode&0o777 != 0o444 || h.Uid != 0 {
		t.Errorf("the file = %q, mode %o, uid %d; want the value, 0444, root", body, h.Mode&0o777, h.Uid)
	}

	// Not in the container's configuration, which is what docker inspect
	// shows and what Docker keeps on disk.
	inspected, err := c.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(inspected.Container.Config), "hunter2") {
		t.Error("the value is in the container's configuration")
	}

	// A restart keeps it: it is part of the container.
	if err := c.Restart(ctx, name); err != nil {
		t.Fatal(err)
	}
	if body, _ := read(); body != value {
		t.Errorf("after a restart the file holds %q", body)
	}
}

// testEngine is the engine the tests run against: Docker, or Podman with
// LIMAN_TEST_ENGINE=podman, or podman-rootless (CI runs all three).
func testEngine() Options {
	switch os.Getenv("LIMAN_TEST_ENGINE") {
	case "podman":
		return ForPodman(os.Getenv("LIMAN_TEST_SOCKET"))
	case "podman-rootless":
		return ForPodmanRootless(os.Getenv("LIMAN_TEST_SOCKET"))
	}
	return ForDocker("")
}
