package deploy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/health"
)

// How a release is found healthy, in the order they are tried:
//
//   - its health path, over HTTP, for a web app that has one;
//   - the image's own HEALTHCHECK, which the engine runs, for an app that
//     has one (a worker has no port to ask);
//   - staying up for its settle time, for a worker without either: one
//     that cannot reach its queue gives up within seconds, and a deploy
//     that called it healthy sooner would retire the release that worked;
//   - still running after one check interval, the old rule for a web app
//     without a health path.
type healthMethod int

const (
	byPath healthMethod = iota
	byImage
	bySettle
	byInterval
)

// waitHealthy blocks until the app is healthy, the container dies, the
// timeout expires or ctx is cancelled.
func (d *Deployer) waitHealthy(ctx context.Context, st *state) error {
	first, err := d.engine.Inspect(ctx, st.container)
	if err != nil {
		return err
	}

	// The app's own healthcheck.timeout, or the deployer's.
	base := d.HealthTimeout
	if st.healthTimeout > 0 {
		base = st.healthTimeout
	}
	method, timeout := byInterval, base
	url := ""
	switch {
	case st.healthPath != "" && st.hostPort != 0:
		method = byPath
		url = health.URL(st.hostIP, st.hostPort, st.healthPath)
		d.detail("GET %s (up to %s)", url, timeout)
	case first.Health != "":
		method = byImage
		// Docker checks every 30 seconds unless the image says otherwise:
		// waiting less than the image's own window would give up before
		// its first verdict.
		timeout = max(base, first.HealthWindow+d.HealthInterval)
		d.detail("waiting for the image's HEALTHCHECK (up to %s)", timeout)
	case st.settle > 0:
		method = bySettle
		timeout = st.settle + base
		d.detail("no health check; the container must stay up for %s", st.settle)
	default:
		d.detail("no healthcheck defined; checking that the container stays up")
	}

	// A derived context carries a deadline. Everything that receives it,
	// including the HTTP request in Probe, stops when the deadline passes.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	ctr := first
	var lastErr error
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			if ctr, err = d.engine.Inspect(ctx, st.container); err != nil {
				return err
			}
		}
		// A container that has exited will never become healthy. Failing
		// immediately gives a precise error instead of a timeout.
		if err := notRunning(ctr, st); err != nil {
			return err
		}

		switch method {
		case byPath:
			if lastErr = d.Probe(ctx, url); lastErr == nil {
				return nil
			}
		case byImage:
			switch ctr.Health {
			case engine.HealthHealthy:
				return nil
			case engine.HealthUnhealthy:
				// Docker has already given it the image's retries.
				return errors.New("the image's HEALTHCHECK reports it unhealthy")
			}
			lastErr = fmt.Errorf("the image's HEALTHCHECK still says %q", ctr.Health)
		case bySettle:
			up := time.Since(started)
			if up >= st.settle {
				d.detail("up for %s", st.settle)
				return nil
			}
			lastErr = fmt.Errorf("up for %s of %s", up.Round(100*time.Millisecond), st.settle)
		case byInterval:
			// "Still running after one interval" is the best signal there
			// is without a health check.
			if attempt > 1 {
				return nil
			}
			lastErr = errors.New("container has not been up for a full interval yet")
		}

		// select waits for whichever happens first: the deadline or the
		// end of the pause. A plain time.Sleep could not be interrupted.
		select {
		case <-ctx.Done():
			// context.Cause tells a deadline apart from a cancellation.
			if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
				return fmt.Errorf("not healthy after %s: %w", timeout, lastErr)
			}
			return ctx.Err()
		case <-time.After(d.HealthInterval):
		}
	}
}

// notRunning explains why a container that should be running is not, or
// returns nil.
func notRunning(ctr engine.Container, st *state) error {
	if ctr.Running() {
		return nil
	}
	if ctr.OOMKilled {
		limit := ""
		if st.memoryLimit != "" {
			limit = " (" + st.memoryLimit + ")"
		}
		return fmt.Errorf("container was killed for exceeding its memory limit%s", limit)
	}
	return fmt.Errorf("container is %s (exit code %d)", ctr.State, ctr.ExitCode)
}

// healthCommand is the manual equivalent of the health step.
func healthCommand(st *state) string {
	if st.healthPath != "" && !st.worker {
		port := "<free port>"
		if st.hostPort != 0 {
			port = fmt.Sprint(st.hostPort)
		}
		return fmt.Sprintf("curl --fail http://127.0.0.1:%s%s", port, st.healthPath)
	}
	if st.settle > 0 {
		return fmt.Sprintf("docker inspect --format '{{.State.Health.Status}}' %s   # the image's HEALTHCHECK; without one: up for %s", st.container, st.settle)
	}
	return "docker inspect --format '{{.State.Status}}' " + st.container
}
