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

	// Drain is how long the previous release keeps running after the proxy
	// has been switched away from it, so that requests it had already
	// accepted can finish. Zero skips the wait.
	Drain time.Duration
	// Sleep waits for d or until ctx is cancelled. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DefaultDrain is how long connections to the previous release are given to
// finish when nothing else is configured.
const DefaultDrain = 5 * time.Second

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
		Drain:          DefaultDrain,
		Sleep:          sleep,
	}
}

// sleep waits for d, returning early with the context's error if ctx is
// cancelled first.
func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
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
// port. The previous one is only retired once the new one is healthy and
// receiving the traffic, so a failed deploy leaves the running version
// untouched. Retiring means stopping, not deleting: the container is kept
// so that Rollback can bring it back.
func (d *Deployer) Deploy(ctx context.Context, a *app.App, opts Options) (Result, error) {
	memory, err := a.Resources.MemoryBytes()
	if err != nil {
		return Result{}, fmt.Errorf("resources.memory: %w", err)
	}

	release := d.NewRelease()
	res := Result{Release: release, Container: a.Name + "-" + release}

	// state is shared by the step closures below: one step fills a field
	// in, a later step reads it.
	st := &state{
		app:         a,
		name:        a.Name,
		domain:      a.Domain,
		healthPath:  healthPath(a),
		memoryLimit: a.Resources.Memory,
		memory:      memory,
		container:   res.Container,
		release:     release,
	}

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

// state is the data that flows between the steps of one deploy or rollback.
type state struct {
	// app is the definition being deployed. It is nil in a rollback, which
	// has no definition to work from, only what the container's labels say.
	app *app.App

	// The fields the steps shared by deploy and rollback need.
	name        string // app name
	domain      string
	healthPath  string // "" if the app has no health check
	memoryLimit string // as written in the definition, for messages

	memory int64
	// container is the container being brought into service: the new one
	// in a deploy, the previous one in a rollback.
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
					HealthPath:    st.healthPath,
					Domain:        a.Domain,
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
	// proven healthy, and the previous one is given time to finish the
	// requests it had already accepted.
	if d.Proxy != nil {
		steps = append(steps, d.proxySteps(st)...)
		if d.Drain > 0 {
			steps = append(steps, d.drainStep(st))
		}
	}

	return append(steps, pipeline.Step{
		Name:   "Retire previous containers of " + a.Name,
		Manual: func() string { return retireCommand(st) },
		Run: func(ctx context.Context) error {
			if len(st.previous) == 0 {
				d.detail("none (first deploy)")
				return nil
			}

			// The list is ordered oldest first, so the last entry is the
			// release that was serving until a moment ago. It is stopped
			// but kept: Rollback starts it again. Anything older has been
			// superseded twice and is removed.
			last := len(st.previous) - 1
			for i, prev := range st.previous {
				if i == last {
					if err := d.engine.Stop(ctx, prev.Name); err != nil {
						return err
					}
					d.detail("stopped %s (kept for rollback)", prev.Name)
					continue
				}
				if err := d.engine.Remove(ctx, prev.Name); err != nil && !errors.Is(err, engine.ErrNotFound) {
					return err
				}
				d.detail("removed %s", prev.Name)
			}
			return nil
		},
		// No Undo. Nothing follows this step, so there is never anything to
		// roll back from here; and removing the older containers cannot be
		// undone in any case.
	})
}

// drainStep waits while the previous release finishes the requests it had
// accepted before the proxy was switched.
//
// After a reload Nginx sends new requests to the new release, but lets its
// old worker processes finish the requests they were already handling,
// against the old release. Stopping that release at once would cut those
// requests off halfway.
func (d *Deployer) drainStep(st *state) pipeline.Step {
	return pipeline.Step{
		Name:   "Drain connections to the previous release",
		Manual: func() string { return fmt.Sprintf("sleep %g", d.Drain.Seconds()) },
		Run: func(ctx context.Context) error {
			if len(st.previous) == 0 {
				d.detail("nothing to drain (first deploy)")
				return nil
			}
			d.detail("waiting %s", d.Drain)
			return d.Sleep(ctx, d.Drain)
		},
		// No Undo: waiting changes nothing.
	}
}

// proxySteps returns the two steps that point the domain at the container
// being brought into service. Deploy and rollback share them.
func (d *Deployer) proxySteps(st *state) []pipeline.Step {
	return []pipeline.Step{
		{
			Name:   "Write Nginx config for " + st.domain,
			Manual: func() string { return "$EDITOR " + d.Proxy.Path(st.name) },
			// The preview is computed when the step is about to run, so in
			// a real deploy it shows the allocated port and in a dry run a
			// placeholder.
			Preview: func() string { return d.previewConf(st) },
			Run: func(context.Context) error {
				// Keep the old content before overwriting it: it is what
				// Undo puts back.
				old, err := d.Proxy.Current(st.name)
				if err != nil {
					return err
				}
				st.previousConf = old

				content, err := d.Proxy.Render(st.name, st.domain, st.hostPort, st.release)
				if err != nil {
					return err
				}
				return d.Proxy.Write(st.name, content)
			},
			Undo: func(context.Context) error {
				return d.Proxy.Restore(st.name, st.previousConf)
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
				// The other release is retired next, so wait until Nginx
				// itself reports the release being brought into service.
				return d.Proxy.WaitRelease(ctx, st.domain, st.release)
			},
			// Undo runs before the undo of the previous step (reverse
			// order), so it cannot rely on the old file being back yet. It
			// restores the file itself and then reloads; the later restore
			// by the previous step is then a harmless repeat.
			Undo: func(ctx context.Context) error {
				if err := d.Proxy.Restore(st.name, st.previousConf); err != nil {
					return err
				}
				if err := d.Proxy.Test(ctx); err != nil {
					return err
				}
				if err := d.Proxy.Reload(ctx); err != nil {
					return err
				}
				// The container that was being brought into service goes
				// away right after this, by the undo of the start step.
				// Wait until Nginx is back on the release it served before.
				return d.Proxy.WaitRelease(ctx, st.domain, d.Proxy.ReleaseOf(st.previousConf))
			},
		},
	}
}

// previewConf returns the diff between the config on disk and the one that
// is about to be written.
func (d *Deployer) previewConf(st *state) string {
	old, err := d.Proxy.Current(st.name)
	if err != nil {
		return fmt.Sprintf("(cannot read the current config: %v)", err)
	}
	updated, err := d.Proxy.Render(st.name, st.domain, st.hostPort, st.release)
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
	if st.healthPath != "" {
		url = health.URL(st.hostPort, st.healthPath)
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
				limit := ""
				if st.memoryLimit != "" {
					limit = " (" + st.memoryLimit + ")"
				}
				return fmt.Errorf("container was killed for exceeding its memory limit%s", limit)
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
	if st.healthPath == "" {
		return "docker inspect --format '{{.State.Status}}' " + st.container
	}
	port := "<free port>"
	if st.hostPort != 0 {
		port = fmt.Sprint(st.hostPort)
	}
	return fmt.Sprintf("curl --fail http://127.0.0.1:%s%s", port, st.healthPath)
}

// retireCommand returns the commands equivalent to the retire step.
func retireCommand(st *state) string {
	if len(st.previous) == 0 {
		// In a dry run nothing has been listed yet, so the names are not
		// known; describe what would happen instead.
		return fmt.Sprintf("docker stop <current container of %s>   # kept for rollback; older ones are removed", st.name)
	}

	last := len(st.previous) - 1
	cmd := "docker stop " + st.previous[last].Name
	if last > 0 {
		names := make([]string, last)
		for i, prev := range st.previous[:last] {
			names[i] = prev.Name
		}
		cmd += " && docker rm -f " + strings.Join(names, " ")
	}
	return cmd
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
	// The indented text is assembled first and passed on in one write. The
	// first version wrote byte by byte; behind it sits a stream that turns
	// every write into an event of its own, so a line of log output became
	// eighty JSON objects and a multi-byte character was cut in pieces.
	out := make([]byte, 0, len(p)+16)
	for _, b := range p {
		if iw.atLineStart {
			out = append(out, "    "...)
			iw.atLineStart = false
		}
		out = append(out, b)
		if b == '\n' {
			iw.atLineStart = true
		}
	}
	if _, err := iw.w.Write(out); err != nil {
		return 0, err
	}
	// Report the caller's byte count, not the count including indentation;
	// io.Writer requires n <= len(p).
	return len(p), nil
}
