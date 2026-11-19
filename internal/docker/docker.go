// Package docker implements engine.Engine on top of the Docker Engine API.
//
// The SDK used here is a thin wrapper around the HTTP API that dockerd
// serves on its Unix socket. Each method below corresponds to one or two
// HTTP requests, noted in the comments.
package docker

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/onrcanogul/liman/internal/engine"
)

// loopback is the only host address container ports are published on. The
// app must be reachable by the local reverse proxy, not from the internet.
var loopback = netip.MustParseAddr("127.0.0.1")

// Client talks to a Docker daemon.
type Client struct {
	api *client.Client
}

// This line makes the compiler verify that *Client implements engine.Engine.
// Go interfaces are satisfied implicitly, so without it a missing method
// would only be reported where a *Client is first used as an Engine.
var _ engine.Engine = (*Client)(nil)

// Connect creates a client and checks that the daemon is reachable.
//
// The daemon address comes from the environment (DOCKER_HOST) and falls back
// to the default socket, /var/run/docker.sock.
func Connect(ctx context.Context) (*Client, error) {
	api, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	// GET /_ping. Creating the client does not open a connection, so
	// without this the first real call would fail with a confusing error.
	if _, err := api.Ping(ctx, client.PingOptions{}); err != nil {
		return nil, fmt.Errorf("connect to docker (is it running?): %w", err)
	}

	return &Client{api: api}, nil
}

// Pull implements engine.Engine. POST /images/create.
func (c *Client) Pull(ctx context.Context, image string, progress io.Writer) error {
	resp, err := c.api.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}

	// The daemon answers with a stream of JSON messages that arrive while
	// the download is running. Ranging over it reads them one by one; the
	// loop ends when the pull is finished and the stream is closed.
	for msg, err := range resp.JSONMessages(ctx) {
		if err != nil {
			return fmt.Errorf("pull %s: %w", image, err)
		}
		// A failed pull (unknown tag, no access) is still an HTTP 200; the
		// failure is reported inside the stream.
		if msg.Error != nil {
			return fmt.Errorf("pull %s: %s", image, msg.Error.Message)
		}
		// Messages with an ID are per-layer progress updates, dozens per
		// second. Only the summary lines are worth showing.
		if msg.ID == "" && msg.Status != "" {
			fmt.Fprintf(progress, "%s\n", msg.Status)
		}
	}
	return nil
}

// Start implements engine.Engine. POST /containers/create, then
// POST /containers/{id}/start.
func (c *Client) Start(ctx context.Context, spec engine.Spec) (string, error) {
	port, err := network.ParsePort(fmt.Sprintf("%d/tcp", spec.ContainerPort))
	if err != nil {
		return "", fmt.Errorf("container port: %w", err)
	}

	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		// Config describes the container itself: what runs inside it.
		Config: &container.Config{
			Image:        spec.Image,
			Env:          envList(spec.Env),
			Labels:       map[string]string{engine.LabelApp: spec.App},
			ExposedPorts: network.PortSet{port: struct{}{}},
		},
		// HostConfig describes how it is attached to the host: published
		// ports and resource limits.
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{
				port: []network.PortBinding{{
					HostIP:   loopback,
					HostPort: strconv.Itoa(spec.HostPort),
				}},
			},
			Resources: container.Resources{
				Memory: spec.MemoryBytes,
				// The API expresses CPU limits in billionths of a core.
				NanoCPUs: int64(spec.CPUs * 1e9),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", spec.Name, err)
	}

	if _, err := c.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		// Creating succeeded but starting failed, typically because the
		// host port is taken. The created container would keep the name
		// reserved, so clean it up. A fresh context is used because ctx may
		// be the reason we are here (cancelled).
		_, _ = c.api.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
		return "", fmt.Errorf("start container %s: %w", spec.Name, err)
	}

	return created.ID, nil
}

// Inspect implements engine.Engine. GET /containers/{ref}/json.
func (c *Client) Inspect(ctx context.Context, ref string) (engine.Container, error) {
	res, err := c.api.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		// Translate the SDK's error into ours so callers do not need to
		// import anything Docker-specific.
		return engine.Container{}, fmt.Errorf("%s: %w", ref, engine.ErrNotFound)
	}
	if err != nil {
		return engine.Container{}, fmt.Errorf("inspect container %s: %w", ref, err)
	}

	info := res.Container
	// A container without our label was started by someone else. Refusing
	// it here protects every method that inspects first (Remove, Logs).
	if info.Config == nil || info.Config.Labels[engine.LabelApp] == "" {
		return engine.Container{}, fmt.Errorf("%s: %w", ref, engine.ErrNotManaged)
	}

	out := engine.Container{
		ID: info.ID,
		// The API reports names with a leading slash.
		Name:  strings.TrimPrefix(info.Name, "/"),
		App:   info.Config.Labels[engine.LabelApp],
		Image: info.Config.Image,
	}
	if info.State != nil {
		out.State = string(info.State.Status)
		out.ExitCode = info.State.ExitCode
		out.OOMKilled = info.State.OOMKilled
	}
	if info.HostConfig != nil {
		out.HostPort = firstHostPort(info.HostConfig.PortBindings)
	}
	return out, nil
}

// Remove implements engine.Engine. POST /containers/{id}/stop, then
// DELETE /containers/{id}.
func (c *Client) Remove(ctx context.Context, ref string) error {
	// Inspect first: it verifies that the container is ours.
	info, err := c.Inspect(ctx, ref)
	if err != nil {
		return err
	}

	// Stop sends SIGTERM and gives the app time to shut down cleanly before
	// the daemon falls back to SIGKILL. Stopping an already stopped
	// container is not an error.
	if _, err := c.api.ContainerStop(ctx, info.ID, client.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("stop container %s: %w", info.Name, err)
	}
	if _, err := c.api.ContainerRemove(ctx, info.ID, client.ContainerRemoveOptions{}); err != nil {
		return fmt.Errorf("remove container %s: %w", info.Name, err)
	}
	return nil
}

// List implements engine.Engine. GET /containers/json?all=true&filters=...
func (c *Client) List(ctx context.Context) ([]engine.Container, error) {
	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true, // include stopped containers
		Filters: make(client.Filters).Add("label", engine.LabelApp),
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]engine.Container, 0, len(res.Items))
	for _, item := range res.Items {
		ctr := engine.Container{
			ID:      item.ID,
			App:     item.Labels[engine.LabelApp],
			Image:   item.Image,
			State:   string(item.State),
			Status:  item.Status,
			Created: time.Unix(item.Created, 0),
		}
		if len(item.Names) > 0 {
			ctr.Name = strings.TrimPrefix(item.Names[0], "/")
		}
		// The list endpoint only reports ports of running containers.
		for _, p := range item.Ports {
			if p.PublicPort != 0 {
				ctr.HostPort = int(p.PublicPort)
				break
			}
		}
		out = append(out, ctr)
	}

	// The comparison function says whether element i sorts before j: first
	// by app, then oldest first, with the name as a stable tie-breaker for
	// containers created within the same second.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.App != b.App {
			return a.App < b.App
		}
		if !a.Created.Equal(b.Created) {
			return a.Created.Before(b.Created)
		}
		return a.Name < b.Name
	})
	return out, nil
}

// Logs implements engine.Engine. GET /containers/{id}/logs.
func (c *Client) Logs(ctx context.Context, ref string, follow bool, stdout, stderr io.Writer) error {
	info, err := c.Inspect(ctx, ref)
	if err != nil {
		return err
	}

	stream, err := c.api.ContainerLogs(ctx, info.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       "200",
	})
	if err != nil {
		return fmt.Errorf("read logs of %s: %w", info.Name, err)
	}
	// defer runs when the function returns, on every path. It is Go's way
	// of making sure a resource is released.
	defer func() { _ = stream.Close() }()

	// The daemon sends stdout and stderr interleaved on one connection,
	// each chunk prefixed with a small header. StdCopy splits them again.
	// With follow it blocks here until the stream ends.
	if _, err := stdcopy.StdCopy(stdout, stderr, stream); err != nil {
		return fmt.Errorf("read logs of %s: %w", info.Name, err)
	}
	return nil
}

// envList converts a map to the KEY=value list the API expects, sorted so
// the result is deterministic.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// firstHostPort returns the first published host port, or 0.
func firstHostPort(bindings network.PortMap) int {
	for _, list := range bindings {
		for _, b := range list {
			if p, err := strconv.Atoi(b.HostPort); err == nil && p > 0 {
				return p
			}
		}
	}
	return 0
}
