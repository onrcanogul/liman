// Package deploy runs an app definition on a container engine.
//
// It knows the order of the steps and what to do when one fails. It does not
// know Docker: everything goes through the engine.Engine interface.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/engine"
)

// Deployer deploys apps. The zero value is not usable; create one with New.
type Deployer struct {
	engine engine.Engine
	out    io.Writer

	// The fields below are the deployer's dependencies on the outside
	// world. They are fields rather than direct calls so tests can replace
	// them and run instantly, without a network or a clock.

	// AllocatePort returns a free port on 127.0.0.1.
	AllocatePort func() (int, error)
	// Probe performs one health request and returns nil when healthy.
	Probe func(ctx context.Context, url string) error
	// HealthTimeout is how long the app gets to become healthy.
	HealthTimeout time.Duration
	// HealthInterval is the pause between health attempts.
	HealthInterval time.Duration
}

// Result describes a successful deploy.
type Result struct {
	ContainerID string
	HostPort    int
}

// New returns a Deployer with production defaults. Progress is written to
// out.
func New(e engine.Engine, out io.Writer) *Deployer {
	return &Deployer{
		engine:         e,
		out:            out,
		AllocatePort:   freePort,
		Probe:          httpProbe,
		HealthTimeout:  30 * time.Second,
		HealthInterval: 500 * time.Millisecond,
	}
}

// Deploy pulls the image, replaces any previous container of the same app
// and waits until the new one is healthy.
func (d *Deployer) Deploy(ctx context.Context, a *app.App) (Result, error) {
	memory, err := a.Resources.MemoryBytes()
	if err != nil {
		return Result{}, fmt.Errorf("resources.memory: %w", err)
	}

	// Pull first. If the image does not exist we fail here, while the
	// previous version is still running.
	d.step("Pull image %s", a.Image)
	if err := d.engine.Pull(ctx, a.Image, indent(d.out)); err != nil {
		return Result{}, err
	}

	d.step("Allocate a free host port")
	hostPort, err := d.AllocatePort()
	if err != nil {
		return Result{}, fmt.Errorf("allocate host port: %w", err)
	}
	d.detail("127.0.0.1:%d", hostPort)

	// The container name must be unique, so the old container has to go
	// before the new one can start. This causes a short downtime; stage 7
	// replaces it with a zero-downtime switch.
	err = d.engine.Remove(ctx, a.Name)
	switch {
	case err == nil:
		d.step("Removed previous container %s", a.Name)
	case errors.Is(err, engine.ErrNotFound):
		// First deploy of this app: nothing to remove.
	default:
		return Result{}, fmt.Errorf("remove previous container: %w", err)
	}

	d.step("Start container %s", a.Name)
	id, err := d.engine.Start(ctx, engine.Spec{
		Name:          a.Name,
		Image:         a.Image,
		ContainerPort: a.Port,
		HostPort:      hostPort,
		Env:           a.Env,
		MemoryBytes:   memory,
		CPUs:          a.Resources.CPUs,
	})
	if err != nil {
		return Result{}, err
	}
	d.detail("%s", shortID(id))

	d.step("Health check")
	if err := d.waitHealthy(ctx, a, hostPort); err != nil {
		// The container is left in place on purpose so its logs can be
		// inspected. Automatic undo arrives in stage 3.
		return Result{}, fmt.Errorf("health check failed (inspect with: liman logs %s): %w", a.Name, err)
	}

	return Result{ContainerID: id, HostPort: hostPort}, nil
}

// waitHealthy blocks until the app is healthy, the container dies, the
// timeout expires or ctx is cancelled.
func (d *Deployer) waitHealthy(ctx context.Context, a *app.App, hostPort int) error {
	// A derived context carries a deadline. Everything that receives it,
	// including the HTTP request in Probe, stops when the deadline passes.
	ctx, cancel := context.WithTimeout(ctx, d.HealthTimeout)
	defer cancel()

	url := ""
	if a.Healthcheck != nil {
		url = fmt.Sprintf("http://127.0.0.1:%d%s", hostPort, a.Healthcheck.Path)
		d.detail("GET %s", url)
	} else {
		d.detail("no healthcheck defined; checking that the container stays up")
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		// A container that has exited will never become healthy. Failing
		// immediately gives a precise error instead of a timeout.
		ctr, err := d.engine.Inspect(ctx, a.Name)
		if err != nil {
			return err
		}
		if !ctr.Running() {
			if ctr.OOMKilled {
				return fmt.Errorf("container was killed for exceeding its memory limit (%s)", a.Resources.Memory)
			}
			return fmt.Errorf("container is %s (exit code %d)", ctr.State, ctr.ExitCode)
		}

		if url == "" {
			// Without a healthcheck, "still running after one interval" is
			// the best signal available.
			if attempt > 1 {
				return nil
			}
			lastErr = errors.New("container has not been up for a full interval yet")
		} else if lastErr = d.Probe(ctx, url); lastErr == nil {
			return nil
		}

		// select waits for whichever happens first: the deadline or the
		// end of the pause. A plain time.Sleep could not be interrupted.
		select {
		case <-ctx.Done():
			return fmt.Errorf("not healthy after %s: %w", d.HealthTimeout, lastErr)
		case <-time.After(d.HealthInterval):
		}
	}
}

func (d *Deployer) step(format string, args ...any) {
	fmt.Fprintf(d.out, "==> "+format+"\n", args...)
}

func (d *Deployer) detail(format string, args ...any) {
	fmt.Fprintf(d.out, "    "+format+"\n", args...)
}

// freePort asks the operating system for an unused port by listening on
// port 0, then releases it.
//
// There is a small window between closing the listener and Docker binding
// the port in which another process could take it. That is acceptable for a
// single-operator tool; the start step fails cleanly if it happens.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()

	// l.Addr() returns the net.Addr interface. The type assertion gets the
	// concrete TCP address back so its Port field can be read.
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address type %T", l.Addr())
	}
	return addr.Port, nil
}

// httpProbe treats any 2xx or 3xx response as healthy.
func httpProbe(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// indent returns a writer that prefixes every line with four spaces, used
// to nest engine output under a step.
func indent(w io.Writer) io.Writer {
	return &indentWriter{w: w, atLineStart: true}
}

// indentWriter implements io.Writer. Any type with a matching Write method
// can be passed wherever an io.Writer is expected.
type indentWriter struct {
	w           io.Writer
	atLineStart bool
}

func (iw *indentWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if iw.atLineStart {
			if _, err := io.WriteString(iw.w, "    "); err != nil {
				return 0, err
			}
			iw.atLineStart = false
		}
		if _, err := iw.w.Write([]byte{b}); err != nil {
			return 0, err
		}
		if b == '\n' {
			iw.atLineStart = true
		}
	}
	// Report the caller's byte count, not the count including indentation;
	// io.Writer requires n <= len(p).
	return len(p), nil
}
