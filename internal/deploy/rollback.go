package deploy

import (
	"context"
	"errors"
	"fmt"

	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/pipeline"
)

// ErrNoPrevious is returned by Rollback when the app has no earlier release
// to go back to.
var ErrNoPrevious = errors.New("no previous release is kept")

// Rollback puts the previous release of an app back into service and removes
// the current one.
//
// It needs no app definition and pulls no image: the previous container was
// stopped, not deleted, by the deploy that replaced it, and is simply started
// again with everything it had (image, environment, limits, port). What a
// rollback needs to know beyond that (the health check path and the domain)
// is read from the container's labels.
//
// Only one step back is possible. Afterwards the release that was rolled
// back from is gone, and there is no previous release until the next deploy.
func (d *Deployer) Rollback(ctx context.Context, appName string, opts Options) (Result, error) {
	all, err := d.engine.List(ctx)
	if err != nil {
		return Result{}, err
	}
	containers := engine.ForApp(all, appName)

	switch len(containers) {
	case 0:
		return Result{}, fmt.Errorf("%s: %w", appName, engine.ErrNotFound)
	case 1:
		return Result{}, fmt.Errorf("%s: %w; deploy the version you want instead", appName, ErrNoPrevious)
	}

	// Ordered oldest first: the last one is in service, the one before it
	// is what the last deploy replaced.
	current := containers[len(containers)-1]
	listed := containers[len(containers)-2]

	// The listing does not report the port of a stopped container. Inspect
	// does, because it reads the container's configuration rather than its
	// live network state.
	target, err := d.engine.Inspect(ctx, listed.Name)
	if err != nil {
		return Result{}, err
	}
	if d.Proxy != nil && target.Domain == "" && !target.Worker() {
		return Result{}, fmt.Errorf("%s was started by a version of liman that did not record its domain, so the proxy cannot be pointed back at it; deploy the version you want instead", target.Name)
	}

	st := &state{
		name:       appName,
		domain:     target.Domain,
		healthPath: target.HealthPath,
		container:  target.Name,
		release:    target.Release,
		hostPort:   target.HostPort,
		worker:     target.Worker(),
		// Either release asking for it is enough: the one running may
		// have been deployed with replace, the one coming back without.
		replace: target.Replace() || current.Replace(),
		// From the point of view of the shared steps, the release being
		// replaced is "previous": it is what gets drained.
		previous: []engine.Container{current},
		listed:   true,
	}
	// What healthcheck.settle said is not on the container; a worker gets
	// the default.
	if st.worker {
		st.settle = d.WorkerSettle
	}
	res := Result{Release: target.Release, Container: target.Name, HostPort: target.HostPort, Worker: target.Worker()}

	var steps []pipeline.Step
	// Releases that must never run side by side are swapped one after the
	// other here too.
	if st.replace {
		steps = append(steps, d.stopRunningStep(st))
	}
	steps = append(steps, []pipeline.Step{
		{
			Name:   "Start previous container " + target.Name,
			Manual: func() string { return "docker start " + target.Name },
			Run: func(ctx context.Context) error {
				// Restart also starts a stopped container. Docker binds the
				// same host port again; if something else has taken it in
				// the meantime this fails, and nothing has been changed.
				return d.engine.Restart(ctx, target.Name)
			},
			// Undo returns it to how it was found: stopped, but kept.
			Undo: func(ctx context.Context) error {
				return d.engine.Stop(ctx, target.Name)
			},
		},
		{
			Name:   "Health check",
			Manual: func() string { return healthCommand(st) },
			Run: func(ctx context.Context) error {
				err := d.waitHealthy(ctx, st)
				if err != nil {
					d.showLogs(ctx, target.Name)
				}
				return err
			},
		},
	}...)

	if d.Proxy != nil && !st.worker {
		steps = append(steps, d.proxySteps(st)...)
		if d.Drain > 0 && !st.replace {
			steps = append(steps, d.drainStep(st))
		}
	}

	steps = append(steps, pipeline.Step{
		Name:   "Remove container " + current.Name,
		Manual: func() string { return "docker rm -f " + current.Name },
		Run: func(ctx context.Context) error {
			if err := d.engine.Remove(ctx, current.Name); err != nil && !errors.Is(err, engine.ErrNotFound) {
				return err
			}
			return nil
		},
		// No Undo: this is the point of no return, and it is last. Removing
		// the release that was rolled back from keeps a simple rule true:
		// the newest container of an app is always the one in service,
		// which is what the health monitor and the next deploy rely on.
	})

	popts := pipeline.Options{
		Out:     d.out,
		DryRun:  opts.DryRun,
		Confirm: opts.Confirm,
	}
	if opts.OnEvent != nil {
		popts.OnEvent = func(ev pipeline.Event) { opts.OnEvent(target.Release, ev) }
	}

	return res, pipeline.Run(ctx, steps, popts)
}
