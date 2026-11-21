package deploy

import (
	"context"
	"errors"
	"fmt"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/pipeline"
)

// freePortStep finds a free port on 127.0.0.1 for the new release, or for
// its trial.
func (d *Deployer) freePortStep(st *state) pipeline.Step {
	return pipeline.Step{
		Name: "Allocate a free host port",
		Run: func(context.Context) error {
			port, err := d.AllocatePort()
			if err != nil {
				return err
			}
			st.hostPort, st.hostIP = port, ""
			d.detail("127.0.0.1:%d", port)
			return nil
		},
		// No Undo: the port is only reserved once a container binds it.
	}
}

// idlePortStep picks, of an app's two fixed endpoints, the one its running
// release is not on: the new release starts there, next to the old one,
// and whatever balances the two sends traffic to the healthy one.
func (d *Deployer) idlePortStep(st *state) pipeline.Step {
	return pipeline.Step{
		Name:   "Choose the idle one of " + st.publish.String(),
		Manual: func() string { return "# the one the running release of " + st.name + " is not published on" },
		Run: func(ctx context.Context) error {
			if err := d.listPrevious(ctx, st); err != nil {
				return err
			}
			pick := st.publish[0]
			if n := len(st.previous); n > 0 {
				if current := st.previous[n-1]; current.Running() && current.HostPort != 0 {
					d.detail("%s is on %s", current.Name, address(current.HostIP, current.HostPort))
					if current.HostPort == pick.Port && sameHost(current.HostIP, pick.IP()) {
						pick = st.publish[1]
					}
				}
			}
			st.hostPort, st.hostIP = pick.Port, pick.IP()
			d.detail("this release takes %s", pick.Address())
			return nil
		},
	}
}

// trialStep starts the new release on the free port first and checks that
// it is healthy there, before anything happens to the running release: one
// fixed port cannot hold both, and a release that fails here leaves the
// running one untouched. The trial container is removed again; the release
// then starts on the fixed port.
//
// The trial does not shorten the gap: the release starts a second time on
// the fixed port, which takes as long as its first start (a trial measured
// 3.2 s for an app that took 3 s to start, against 3.4 s without a trial).
// What it saves is the outage a broken release would cause.
func (d *Deployer) trialStep(st *state) pipeline.Step {
	trial := st.container + "-try"
	return pipeline.Step{
		Name: "Try the new release on a free port",
		Manual: func() string {
			return fmt.Sprintf("docker run -d --name %s -p 127.0.0.1:<free port>:%d %s   # health check, then docker rm -f %s", trial, st.app.Port, st.app.Image, trial)
		},
		Run: func(ctx context.Context) error {
			a := st.app
			if _, err := d.engine.Start(ctx, engine.Spec{
				App:           a.Name,
				Name:          trial,
				Image:         a.Image,
				ContainerPort: a.Port,
				HostPort:      st.hostPort,
				Command:       a.Command,
				Env:           a.Env,
				Files:         secretFiles(a, st.secretFiles),
				MemoryBytes:   st.memory,
				CPUs:          a.Resources.CPUs,
				HealthPath:    st.healthPath,
				Kind:          a.Kind,
			}); err != nil {
				return err
			}
			tried := *st
			tried.container = trial
			if err := d.waitHealthy(ctx, &tried); err != nil {
				d.showLogs(ctx, trial)
				// A step that fails is not undone itself, only the ones
				// before it: the trial goes now.
				if rerr := d.engine.Remove(context.WithoutCancel(ctx), trial); rerr != nil && !errors.Is(rerr, engine.ErrNotFound) {
					return fmt.Errorf("%w (and removing the trial container failed: %v)", err, rerr)
				}
				return err
			}
			if err := d.engine.Remove(ctx, trial); err != nil {
				return err
			}
			e := st.publish[0]
			st.hostPort, st.hostIP = e.Port, e.IP()
			d.detail("healthy; the release itself goes on %s", e.Address())
			return nil
		},
		Undo: func(ctx context.Context) error {
			if err := d.engine.Remove(ctx, trial); err != nil && !errors.Is(err, engine.ErrNotFound) {
				return err
			}
			return nil
		},
	}
}

// sameHost compares the addresses a container is published on, where ""
// and 127.0.0.1 are the same.
func sameHost(a, b string) bool {
	return displayHost(a) == displayHost(b)
}

func displayHost(ip string) string {
	if ip == "" {
		return app.DefaultPublishHost
	}
	return ip
}

func address(ip string, port int) string {
	return app.Endpoint{Host: ip, Port: port}.Address()
}
