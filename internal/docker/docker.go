// Package docker implements engine.Engine on top of the Docker Engine API.
//
// The SDK used here is a thin wrapper around the HTTP API that dockerd
// serves on its Unix socket. Each method below corresponds to one or two
// HTTP requests, noted in the comments.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/netip"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/engine"
)

// loopback is the only host address container ports are published on. The
// app must be reachable by the local reverse proxy, not from the internet.
var loopback = netip.MustParseAddr("127.0.0.1")

// Client talks to a Docker daemon.
type Client struct {
	api *client.Client

	// creds finds registry credentials for pulling private images.
	creds *credentialStore
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

	return &Client{api: api, creds: newCredentialStore()}, nil
}

// Pull implements engine.Engine. POST /images/create.
func (c *Client) Pull(ctx context.Context, image string, progress io.Writer) error {
	opts := client.ImagePullOptions{}

	// The daemon does not read the user's docker config; that is the
	// client's job. Without this, a private image fails with "denied" even
	// though "docker pull" of the same image works.
	creds, loggedIn, err := c.creds.lookup(ctx, image)
	if err != nil {
		return fmt.Errorf("pull %s: look up registry credentials: %w", image, err)
	}
	if loggedIn {
		if opts.RegistryAuth, err = encodeAuth(creds, image); err != nil {
			return fmt.Errorf("pull %s: encode registry credentials: %w", image, err)
		}
		// The user name is not a secret; the password is never printed.
		fmt.Fprintf(progress, "Authenticating to %s as %s\n", displayHost(image), creds.Username)
	}

	if err := c.pull(ctx, image, opts, progress); err != nil {
		return explainPullError(err, image, loggedIn)
	}
	return nil
}

func (c *Client) pull(ctx context.Context, image string, opts client.ImagePullOptions, progress io.Writer) error {
	resp, err := c.api.ImagePull(ctx, image, opts)
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

// explainPullError adds a hint to errors that are about access.
//
// Registries deliberately answer "denied" or "not found" for a private image
// regardless of whether it exists, so the message alone does not tell the
// user what to do.
func explainPullError(err error, image string, loggedIn bool) error {
	msg := strings.ToLower(err.Error())
	accessProblem := strings.Contains(msg, "denied") || strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "authentication required") || strings.Contains(msg, "no basic auth credentials")
	if !accessProblem {
		return err
	}

	host := registryHost(image)
	if host == dockerHub {
		host = "" // "docker login" without an argument means Docker Hub
	}
	login := strings.TrimSpace("docker login " + host)

	if loggedIn {
		return fmt.Errorf("%w\nthe stored credentials for %s were rejected or lack access to this image; log in again with: %s",
			err, displayHost(image), login)
	}
	return fmt.Errorf("%w\nno credentials for %s were found; if the image is private, log in with: %s",
		err, displayHost(image), login)
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
			Labels:       labels(spec),
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

	// Created but not started yet: the files go in now, so that they are
	// there before the app's first instruction runs.
	if len(spec.Files) > 0 {
		archive, err := filesArchive(spec.Files, time.Now())
		if err == nil {
			// Unpacked at the root, the archive's own directory entries
			// create whatever directories the image lacks (not every
			// image has /run). Docker refuses to replace a directory with
			// a file or the other way round.
			_, err = c.api.CopyToContainer(ctx, created.ID, client.CopyToContainerOptions{DestinationPath: "/", Content: bytes.NewReader(archive)})
		}
		if err != nil {
			_, _ = c.api.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
			return "", fmt.Errorf("put files into container %s: %w", spec.Name, err)
		}
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

// filesArchive returns a tar archive of files, to be unpacked at the root
// of a container: every directory on the way to each file gets an entry of
// its own (mode 0755), then the file. Everything belongs to root; the
// files' modes say who may read them.
//
// The archive is built in memory: secrets never touch the host's disk on
// their way into the container.
func filesArchive(files []engine.File, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for _, f := range files {
		clean := path.Clean(f.Path)
		if !path.IsAbs(clean) || clean != f.Path || clean == "/" {
			return nil, fmt.Errorf("file path %q must be absolute and clean", f.Path)
		}
		rel := strings.TrimPrefix(clean, "/")
		// Parents first, outermost first: "run", then "run/secrets".
		parts := strings.Split(rel, "/")
		for i := 1; i < len(parts); i++ {
			dir := strings.Join(parts[:i], "/")
			if dirs[dir] {
				continue
			}
			dirs[dir] = true
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755, ModTime: now, Format: tar.FormatPAX}); err != nil {
				return nil, err
			}
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: int64(f.Mode.Perm()), Size: int64(len(f.Content)), ModTime: now, Format: tar.FormatPAX}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Content); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ImageExists implements engine.Engine. GET /distribution/{ref}/json: the
// daemon asks the registry for the manifest, with the registry's own
// authentication flow and the manifest types it accepts, and downloads
// nothing. The credentials are the user's, as for Pull.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	opts := client.DistributionInspectOptions{}
	creds, loggedIn, err := c.creds.lookup(ctx, ref)
	if err != nil {
		return false, fmt.Errorf("look up registry credentials for %s: %w", ref, err)
	}
	if loggedIn {
		if opts.EncodedRegistryAuth, err = encodeAuth(creds, ref); err != nil {
			return false, fmt.Errorf("encode registry credentials for %s: %w", ref, err)
		}
	}
	_, err = c.api.DistributionInspect(ctx, ref, opts)
	switch {
	case err == nil:
		return true, nil
	case errdefs.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("ask the registry for %s: %w", ref, err)
	}
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

		Release:    trusted(info.Config.Labels[engine.LabelRelease], app.ValidRelease),
		HealthPath: trusted(info.Config.Labels[engine.LabelHealthPath], app.ValidHealthPath),
		Domain:     trusted(info.Config.Labels[engine.LabelDomain], app.ValidDomain),
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

// Restart implements engine.Engine. POST /containers/{id}/restart.
func (c *Client) Restart(ctx context.Context, ref string) error {
	// Inspect first: it verifies that the container is ours.
	info, err := c.Inspect(ctx, ref)
	if err != nil {
		return err
	}

	// Restart is "stop, then start" in one call. On a container that has
	// already exited the stop is a no-op, so the same call revives it.
	if _, err := c.api.ContainerRestart(ctx, info.ID, client.ContainerRestartOptions{}); err != nil {
		return fmt.Errorf("restart container %s: %w", info.Name, err)
	}
	return nil
}

// Stop implements engine.Engine. POST /containers/{id}/stop.
func (c *Client) Stop(ctx context.Context, ref string) error {
	// Inspect first: it verifies that the container is ours.
	info, err := c.Inspect(ctx, ref)
	if err != nil {
		return err
	}

	// SIGTERM first, so the app can finish what it is doing; the daemon
	// falls back to SIGKILL after the container's stop timeout.
	if _, err := c.api.ContainerStop(ctx, info.ID, client.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("stop container %s: %w", info.Name, err)
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

			Release:    trusted(item.Labels[engine.LabelRelease], app.ValidRelease),
			HealthPath: trusted(item.Labels[engine.LabelHealthPath], app.ValidHealthPath),
			Domain:     trusted(item.Labels[engine.LabelDomain], app.ValidDomain),
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

// labels returns the labels a container of spec carries.
func labels(spec engine.Spec) map[string]string {
	// All four are always set, empty if need be. A label the image itself
	// declares is merged into the container unless the container sets the
	// same key; leaving one out would let an image supply it, and these
	// labels are read back later and acted on (the health path is requested
	// every few seconds, the domain is written into the proxy config).
	return map[string]string{
		engine.LabelApp:        spec.App,
		engine.LabelRelease:    spec.Release,
		engine.LabelHealthPath: spec.HealthPath,
		engine.LabelDomain:     spec.Domain,
	}
}

// trusted returns a label's value if it has the shape liman itself would
// have written, and "" otherwise.
//
// Labels come back from Docker, which is outside this program: anyone with
// access to Docker can start a container with any labels. What is read is
// therefore checked again with the rules that apply to an app definition,
// exactly as if it had arrived over the network.
func trusted(value string, valid func(string) bool) string {
	if valid(value) {
		return value
	}
	return ""
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
