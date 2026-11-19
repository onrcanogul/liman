// Package deploy turns an app definition into the pipeline steps that put
// it on a container engine.
//
// It knows which steps a deploy consists of, how each is undone and how to
// describe each as a manual command. It does not know Docker (everything
// goes through engine.Engine) and it does not run the steps itself (that is
// the pipeline package's job).
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/health"
	"github.com/onrcanogul/liman/internal/pipeline"
	"github.com/onrcanogul/liman/internal/textdiff"
)

// Proxy is what a deploy needs from the reverse proxy. It is declared here,
// by the package that uses it, and lists only the operations a deploy
// performs; *nginx.Manager happens to satisfy it.
type Proxy interface {
	// Path returns the config file of an app, for display.
	Path(app string) string
	// Render returns the config that routes domain to hostPort and reports
	// release. A hostPort of 0 renders a placeholder.
	Render(app, domain string, hostPort int, release string) ([]byte, error)
	// Current returns the config on disk, or nil if there is none.
	Current(app string) ([]byte, error)
	// Write installs a new config.
	Write(app string, content []byte) error
	// Restore puts back an earlier config; nil content removes the file.
	Restore(app string, content []byte) error
	// Test validates the configuration on disk without applying it.
	Test(ctx context.Context) error
	// Reload asks the proxy to apply the configuration on disk. It may
	// return before the proxy has actually switched.
	Reload(ctx context.Context) error
	// WaitRelease blocks until the proxy answers for domain with release.
	// An empty release means no config of ours serves the domain.
	WaitRelease(ctx context.Context, domain, release string) error
	// ReleaseOf returns the release recorded in a config, or "".
	ReleaseOf(content []byte) string
	// TestCommand and ReloadCommand describe the commands for display.
	TestCommand() string
	ReloadCommand() string
}

// Deployer deploys apps. The zero value is not usable; create one with New.
type Deployer struct {
	engine engine.Engine
	out    io.Writer

	// Proxy routes the app's domain to its container. Nil means no proxy
	// is managed: the proxy steps are left out of the pipeline.
	Proxy Proxy

	// The fields below are the deployer's dependencies on the outside
	// world. They are fields rather than direct calls so tests can replace
	// them and run instantly, without a network or a clock.

	// AllocatePort returns a free port on 127.0.0.1.
	AllocatePort func() (int, error)
	// Probe performs one health request and returns nil when healthy.
	Probe func(ctx context.Context, url string) error
	// NewRelease returns an identifier for this deploy. It becomes part of
	// the container name, so it must be unique per app.
	NewRelease func() string
	// HealthTimeout is how long the app gets to become healthy.
	HealthTimeout time.Duration
	// HealthInterval is the pause between health attempts.
	HealthInterval time.Duration
}

// Options selects how a deploy is executed.
type Options struct {
	// DryRun shows the steps without executing them. The engine is not
	// used, so it may be nil.
	DryRun bool
	// Confirm, when set, is asked before every step.
	Confirm func(step pipeline.Step) (bool, error)
	// OnEvent, when set, receives every executed run and undo.
	OnEvent func(release string, ev pipeline.Event)
}

// Result describes a deploy. Release and Container are always set; HostPort
// is only set after a real (not dry) run got far enough to allocate it.
type Result struct {
	Release   string
	Container string
	HostPort  int
}

// New returns a Deployer with production defaults. Progress is written to
// out.
func New(e engine.Engine, out io.Writer) *Deployer {
	return &Deployer{
		engine:         e,
		out:            out,
		AllocatePort:   freePort,
		Probe:          health.Probe,
		NewRelease:     timeRelease,
		HealthTimeout:  30 * time.Second,
		HealthInterval: 500 * time.Millisecond,
	}
}

// timeRelease returns a release identifier derived from the current time,
// to the millisecond: YYYYMMDD-HHMMSS-mmm. Two deploys of one app never run
// at the same time, so they cannot land in the same millisecond.
func timeRelease() string {
	// The reference time in Go layouts is Mon Jan 2 15:04:05 2006. The
	// fractional part must follow a dot in the layout; the dot is then
	// replaced so the result is safe in container names and file names.
	return strings.Replace(time.Now().UTC().Format("20060102-150405.000"), ".", "-", 1)
}

// Deploy runs the deploy pipeline for a.
//
// The new release starts next to the previous one, under its own name and
// port. The previous containers are only removed once the new one is
// healthy, so a failed deploy leaves the running version untouched.
func (d *Deployer) Deploy(ctx context.Context, a *app.App, opts Options) (Result, error) {
	memory, err := a.Resources.MemoryBytes()
	if err != nil {
		return Result{}, fmt.Errorf("resources.memory: %w", err)
	}

	release := d.NewRelease()
	res := Result{Release: release, Container: a.Name + "-" + release}

	// state is shared by the step closures below: one step fills a field
	// in, a later step reads it.
	st := &state{app: a, memory: memory, container: res.Container, release: release}

	popts := pipeline.Options{
		Out:     d.out,
		DryRun:  opts.DryRun,
		Confirm: opts.Confirm,
	}
	if opts.OnEvent != nil {
		// Adapt the pipeline's callback to ours by adding the release.
		popts.OnEvent = func(ev pipeline.Event) { opts.OnEvent(release, ev) }
	}

	err = pipeline.Run(ctx, d.steps(st), popts)
	res.HostPort = st.hostPort
	return res, err
}

// state is the data that flows between the steps of one deploy.
type state struct {
	app       *app.App
	memory    int64
	container string
	release   string

	// hostPort is set by the port step.
	hostPort int
	// previous is set by the start step: the app's containers that existed
	// before this deploy.
	previous []engine.Container

	// previousConf is set by the proxy config step: the config that was on
	// disk before this deploy, or nil if there was none.
	previousConf []byte
}

// steps builds the pipeline. Each Run, Undo and Manual is a closure over d
// and st; nothing is executed here.
func (d *Deployer) steps(st *state) []pipeline.Step {
	a := st.app

	steps := []pipeline.Step{
		{
			Name:   "Pull image " + a.Image,
			Manual: func() string { return "docker pull " + a.Image },
			Run: func(ctx context.Context) error {
				return d.engine.Pull(ctx, a.Image, indent(d.out))
			},
			// No Undo: a pulled image is harmless and speeds up the retry.
		},
		{
			Name: "Allocate a free host port",
			Run: func(context.Context) error {
				port, err := d.AllocatePort()
				if err != nil {
					return err
				}
				st.hostPort = port
				d.detail("127.0.0.1:%d", port)
				return nil
			},
			// No Undo: the port is only reserved once a container binds it.
		},
		{
			Name:   "Start container " + st.container,
			Manual: func() string { return runCommand(st) },
			Run: func(ctx context.Context) error {
				// Remember what was running before, so the last step knows
				// what to remove. This must happen before Start, otherwise
				// the new container would be in the list too.
				all, err := d.engine.List(ctx)
				if err != nil {
					return err
				}
				st.previous = engine.ForApp(all, a.Name)

				id, err := d.engine.Start(ctx, engine.Spec{
					App:           a.Name,
					Name:          st.container,
					Image:         a.Image,
					ContainerPort: a.Port,
					HostPort:      st.hostPort,
					Env:           a.Env,
					MemoryBytes:   st.memory,
					CPUs:          a.Resources.CPUs,
					Release:       st.release,
					HealthPath:    healthPath(a),
				})
				if err != nil {
					return err
				}
				d.detail("%s", shortID(id))
				return nil
			},
			Undo: func(ctx context.Context) error {
				return d.engine.Remove(ctx, st.container)
			},
		},
		{
			Name:   "Health check",
			Manual: func() string { return healthCommand(st) },
			Run: func(ctx context.Context) error {
				err := d.waitHealthy(ctx, st)
				if err != nil {
					// The undo of the start step is about to remove the
					// container, and its logs with it. Show them now.
					d.showLogs(ctx, st.container)
				}
				return err
			},
		},
	}

	// With a proxy, traffic is switched to the new container after it has
	// proven healthy and before the old containers are removed.
	if d.Proxy != nil {
		steps = append(steps, d.proxySteps(st)...)
	}

	// The removal is appended last on purpose: it is the one step that
	// cannot be undone.
	return append(steps, pipeline.Step{
		Name:   "Remove previous containers of " + a.Name,
		Manual: func() string { return removeCommand(st) },
		Run: func(ctx context.Context) error {
			if len(st.previous) == 0 {
				d.detail("none (first deploy)")
				return nil
			}
			for _, prev := range st.previous {
				if err := d.engine.Remove(ctx, prev.Name); err != nil && !errors.Is(err, engine.ErrNotFound) {
					return err
				}
				d.detail("removed %s", prev.Name)
			}
			return nil
		},
		// No Undo: a removed container cannot be brought back. This is
		// the point of no return, which is why it is the last step.
	})
}

// proxySteps returns the two steps that point the domain at the new
// container.
func (d *Deployer) proxySteps(st *state) []pipeline.Step {
	a := st.app

	return []pipeline.Step{
		{
			Name:   "Write Nginx config for " + a.Domain,
			Manual: func() string { return "$EDITOR " + d.Proxy.Path(a.Name) },
			// The preview is computed when the step is about to run, so in
			// a real deploy it shows the allocated port and in a dry run a
			// placeholder.
			Preview: func() string { return d.previewConf(st) },
			Run: func(context.Context) error {
				// Keep the old content before overwriting it: it is what
				// Undo puts back.
				old, err := d.Proxy.Current(a.Name)
				if err != nil {
					return err
				}
				st.previousConf = old

				content, err := d.Proxy.Render(a.Name, a.Domain, st.hostPort, st.release)
				if err != nil {
					return err
				}
				return d.Proxy.Write(a.Name, content)
			},
			Undo: func(context.Context) error {
				return d.Proxy.Restore(a.Name, st.previousConf)
			},
		},
		{
			Name:   "Reload Nginx",
			Manual: func() string { return d.Proxy.TestCommand() + " && " + d.Proxy.ReloadCommand() },
			Run: func(ctx context.Context) error {
				// Test first. A reload with a broken config is refused by
				// Nginx anyway, but testing separately gives a clear error
				// and never touches the running process.
				if err := d.Proxy.Test(ctx); err != nil {
					return err
				}
				if err := d.Proxy.Reload(ctx); err != nil {
					return err
				}
				// The reload command returns before Nginx has switched.
				// The next step removes the old container, so wait until
				// Nginx itself reports the new release.
				return d.Proxy.WaitRelease(ctx, a.Domain, st.release)
			},
			// Undo runs before the undo of the previous step (reverse
			// order), so it cannot rely on the old file being back yet. It
			// restores the file itself and then reloads; the later restore
			// by the previous step is then a harmless repeat.
			Undo: func(ctx context.Context) error {
				if err := d.Proxy.Restore(a.Name, st.previousConf); err != nil {
					return err
				}
				if err := d.Proxy.Test(ctx); err != nil {
					return err
				}
				if err := d.Proxy.Reload(ctx); err != nil {
					return err
				}
				// The new container is removed right after this, by the
				// undo of the start step. Wait until Nginx is back on the
				// release it served before.
				return d.Proxy.WaitRelease(ctx, a.Domain, d.Proxy.ReleaseOf(st.previousConf))
			},
		},
	}
}

// previewConf returns the diff between the config on disk and the one that
// is about to be written.
func (d *Deployer) previewConf(st *state) string {
	old, err := d.Proxy.Current(st.app.Name)
	if err != nil {
		return fmt.Sprintf("(cannot read the current config: %v)", err)
	}
	updated, err := d.Proxy.Render(st.app.Name, st.app.Domain, st.hostPort, st.release)
	if err != nil {
		return fmt.Sprintf("(cannot render the new config: %v)", err)
	}

	// Two lines of context are enough to see where in the file a change is.
	diff := textdiff.Lines(old, updated, 2)
	if diff == "" {
		return "(no changes)"
	}
	return diff
}

// waitHealthy blocks until the app is healthy, the container dies, the
// timeout expires or ctx is cancelled.
func (d *Deployer) waitHealthy(ctx context.Context, st *state) error {
	// A derived context carries a deadline. Everything that receives it,
	// including the HTTP request in Probe, stops when the deadline passes.
	ctx, cancel := context.WithTimeout(ctx, d.HealthTimeout)
	defer cancel()

	url := ""
	if st.app.Healthcheck != nil {
		url = health.URL(st.hostPort, st.app.Healthcheck.Path)
		d.detail("GET %s", url)
	} else {
		d.detail("no healthcheck defined; checking that the container stays up")
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		// A container that has exited will never become healthy. Failing
		// immediately gives a precise error instead of a timeout.
		ctr, err := d.engine.Inspect(ctx, st.container)
		if err != nil {
			return err
		}
		if !ctr.Running() {
			if ctr.OOMKilled {
				return fmt.Errorf("container was killed for exceeding its memory limit (%s)", st.app.Resources.Memory)
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
			// context.Cause tells a deadline apart from a cancellation.
			if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
				return fmt.Errorf("not healthy after %s: %w", d.HealthTimeout, lastErr)
			}
			return ctx.Err()
		case <-time.After(d.HealthInterval):
		}
	}
}

// showLogs prints the container's recent output. Failures are ignored: this
// is a diagnostic aid on an error path that already has an error to report.
func (d *Deployer) showLogs(ctx context.Context, container string) {
	// The context may be cancelled or past its deadline; logs are still
	// worth a short attempt.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	d.detail("container output:")
	w := indent(indent(d.out))
	_ = d.engine.Logs(ctx, container, false, w, w)
}

func (d *Deployer) detail(format string, args ...any) {
	fmt.Fprintf(d.out, "    "+format+"\n", args...)
}

// runCommand returns the docker run command equivalent to the start step.
func runCommand(st *state) string {
	a := st.app

	port := "<free port>"
	if st.hostPort != 0 {
		port = fmt.Sprint(st.hostPort)
	}

	// strings.Builder assembles a string piece by piece without creating a
	// new string for every addition.
	var b strings.Builder
	fmt.Fprintf(&b, "docker run -d --name %s --label %s=%s -p 127.0.0.1:%s:%d",
		st.container, engine.LabelApp, a.Name, port, a.Port)
	if a.Resources.Memory != "" {
		fmt.Fprintf(&b, " --memory %s", a.Resources.Memory)
	}
	if a.Resources.CPUs > 0 {
		fmt.Fprintf(&b, " --cpus %g", a.Resources.CPUs)
	}

	keys := make([]string, 0, len(a.Env))
	for k := range a.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Values may be secrets. This output ends up in terminals, logs
		// and screenshots, so only the names are shown.
		fmt.Fprintf(&b, " -e %s=***", k)
	}

	fmt.Fprintf(&b, " %s", a.Image)
	return b.String()
}

func healthCommand(st *state) string {
	if st.app.Healthcheck == nil {
		return "docker inspect --format '{{.State.Status}}' " + st.container
	}
	port := "<free port>"
	if st.hostPort != 0 {
		port = fmt.Sprint(st.hostPort)
	}
	return fmt.Sprintf("curl --fail http://127.0.0.1:%s%s", port, st.app.Healthcheck.Path)
}

func removeCommand(st *state) string {
	if len(st.previous) == 0 {
		// In a dry run nothing has been listed yet, so the names are not
		// known; describe how they would be found instead.
		return fmt.Sprintf("docker rm -f $(docker ps -aq --filter label=%s=%s)   # except %s",
			engine.LabelApp, st.app.Name, st.container)
	}
	names := make([]string, len(st.previous))
	for i, prev := range st.previous {
		names[i] = prev.Name
	}
	return "docker rm -f " + strings.Join(names, " ")
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

// healthPath returns the app's health check path, or "" if it has none.
func healthPath(a *app.App) string {
	if a.Healthcheck == nil {
		return ""
	}
	return a.Healthcheck.Path
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
