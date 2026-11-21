// Package monitor watches the apps and restarts the ones that have stopped
// working.
//
// It runs inside the daemon as a loop that wakes up at a fixed interval,
// looks at every app and acts on what it finds. Nothing asks it to; that is
// the difference to everything else in liman, which only ever reacts to a
// request.
package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/onrcanogul/liman/internal/applock"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/health"
)

// Health states reported for an app.
const (
	// Healthy: the container runs and, if a health check is defined,
	// answers it.
	Healthy = "healthy"
	// Unhealthy: the health check is failing, but not yet often enough in a
	// row to act on.
	Unhealthy = "unhealthy"
	// Restarting: the app was just restarted and is given time to come up.
	Restarting = "restarting"
	// Down: the app is not working and the last restart attempt failed.
	Down = "down"
)

// Defaults used when the corresponding field is zero.
const (
	DefaultInterval         = 10 * time.Second
	DefaultFailureThreshold = 3
	DefaultMaxBackoff       = 5 * time.Minute

	// healthyChecksToForgive is how many healthy checks in a row it takes
	// before earlier restarts stop counting against an app.
	healthyChecksToForgive = 5
	// maxParallelChecks bounds how many apps are checked at the same time.
	maxParallelChecks = 4
	// checkTimeout bounds the work done for one app in one pass.
	checkTimeout = 30 * time.Second
)

// Status is what the monitor knows about one app.
type Status struct {
	App string
	// Health is one of the constants above.
	Health string
	// Detail explains a state other than Healthy.
	Detail string
	// Restarts counts the restarts since the app was last stable.
	Restarts int
	// LastRestart is when the monitor last restarted the app, or the zero
	// time if it never did.
	LastRestart time.Time
	// CheckedAt is when the app was last looked at.
	CheckedAt time.Time
}

// Event describes one restart attempt, for the history.
type Event struct {
	App       string
	Release   string
	Container string
	// Reason is why the app was restarted.
	Reason string
	// Err is nil if the restart command succeeded.
	Err      error
	Started  time.Time
	Duration time.Duration
}

// Check is the outcome of one look at an app, for the health history.
type Check struct {
	App  string
	Time time.Time
	// OK is false when something was wrong with the app.
	OK bool
	// Probed says whether an HTTP health request was made; Duration is how
	// long it took and means nothing without one.
	Probed   bool
	Duration time.Duration
}

// Monitor checks apps periodically. Set the exported fields before calling
// Run; they must not be changed afterwards.
type Monitor struct {
	// Connect returns the container engine.
	Connect func(ctx context.Context) (engine.Engine, error)
	// Locks is shared with the API server, so the monitor never restarts an
	// app that is being deployed or stopped.
	Locks *applock.Locks

	// Interval is the pause between passes over all apps.
	Interval time.Duration
	// FailureThreshold is how many health checks in a row must fail before
	// a running app is restarted. One slow answer must not cause a restart.
	FailureThreshold int
	// MaxBackoff caps the waiting time between restarts of an app that
	// keeps failing.
	MaxBackoff time.Duration

	// OnRestart, when set, is called after every restart attempt.
	OnRestart func(Event)
	// OnCheck, when set, is called after every check of an app. Checks of
	// different apps run concurrently, so it must be safe to call from
	// several goroutines at once.
	OnCheck func(Check)
	// Log receives what the monitor does. Nil discards it.
	Log *slog.Logger

	// Probe performs one health request. Nil means health.Probe.
	Probe func(ctx context.Context, url string) error
	// Now is the clock. Nil means time.Now; tests move time by hand.
	Now func() time.Time

	// mu guards apps. Checks of different apps run concurrently, and the
	// API reads the statuses from yet another goroutine.
	mu   sync.Mutex
	apps map[string]*appState
}

// appState is the monitor's memory of one app between passes.
type appState struct {
	// container is the container the rest of this state is about.
	container string

	status Status
	// failures counts consecutive failed health checks.
	failures int
	// healthyStreak counts consecutive healthy checks.
	healthyStreak int
	// notBefore is the earliest time of the next check. It implements both
	// the grace period after a restart and the growing delay between
	// restarts of an app that keeps failing.
	notBefore time.Time
}

// Run checks all apps every Interval until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	// A Ticker delivers a value on its channel at regular intervals. It
	// must be stopped, or its timer lives on after this function returns.
	ticker := time.NewTicker(m.interval())
	defer ticker.Stop()

	m.logger().Info("health monitor started", "interval", m.interval().String(), "failure_threshold", m.failureThreshold())

	for {
		// select blocks until one of its cases can proceed: either the
		// daemon is shutting down, or it is time for the next pass.
		select {
		case <-ctx.Done():
			m.logger().Info("health monitor stopped")
			return
		case <-ticker.C:
			// The pass runs here, in this goroutine. If it takes longer
			// than the interval, the ticker does not queue up passes: it
			// drops ticks, so passes never overlap or pile up.
			m.CheckOnce(ctx)
		}
	}
}

// CheckOnce looks at every app once and restarts the ones that need it. Run
// calls it on every tick; tests call it directly.
func (m *Monitor) CheckOnce(ctx context.Context) {
	eng, err := m.Connect(ctx)
	if err != nil {
		// Docker being down is not something the monitor can fix. Say so
		// and try again on the next tick.
		m.logger().Warn("health monitor cannot reach the container engine", "error", err)
		return
	}

	containers, err := eng.List(ctx)
	if err != nil {
		m.logger().Warn("health monitor cannot list containers", "error", err)
		return
	}

	// Group by app. The list is ordered oldest first within an app, so the
	// last container seen is the newest one, which is the one that serves.
	newest := map[string]engine.Container{}
	for _, c := range containers {
		newest[c.App] = c
	}
	m.forget(newest)

	// Check the apps concurrently, but only a few at a time: a slow health
	// check of one app must not delay the others, and a hundred apps must
	// not mean a hundred simultaneous probes.
	slots := make(chan struct{}, maxParallelChecks)
	var wg sync.WaitGroup

	for _, c := range newest {
		wg.Add(1)
		slots <- struct{}{} // blocks while maxParallelChecks are running
		go func() {
			defer wg.Done()
			defer func() { <-slots }()

			checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
			defer cancel()
			m.checkApp(checkCtx, eng, c)
		}()
	}
	// Wait for all checks before returning, so two passes never overlap.
	wg.Wait()
}

// checkApp decides what state one app is in and restarts it if needed.
func (m *Monitor) checkApp(ctx context.Context, eng engine.Engine, c engine.Container) {
	now := m.now()
	st := m.state(c.App, c.Name)

	// Still inside the grace period after a restart, or waiting out the
	// delay before the next attempt: leave the app alone.
	if now.Before(st.notBefore) {
		return
	}

	problem, probed, took := m.diagnose(ctx, eng, c)
	if m.OnCheck != nil {
		// Called before the lock below is taken: the callback writes to the
		// database, and a slow disk must not hold up everyone who wants to
		// read a status.
		m.OnCheck(Check{App: c.App, Time: now, OK: problem == "", Probed: probed, Duration: took})
	}

	m.mu.Lock()
	st.status.CheckedAt = now
	if problem == "" {
		st.failures = 0
		st.healthyStreak++
		// An app that has been fine for a while has earned a clean record:
		// its next failure is treated as a first one again.
		if st.healthyStreak >= healthyChecksToForgive {
			st.status.Restarts = 0
		}
		st.status.Health, st.status.Detail = Healthy, ""
		m.mu.Unlock()
		return
	}

	st.healthyStreak = 0
	// A stopped container is restarted at once. A running one that fails
	// its health check gets the benefit of the doubt until the threshold.
	if c.Running() {
		st.failures++
		if st.failures < m.failureThreshold() {
			st.status.Health = Unhealthy
			st.status.Detail = fmt.Sprintf("%s (%d of %d failures before a restart)", problem, st.failures, m.failureThreshold())
			m.mu.Unlock()
			return
		}
	}
	m.mu.Unlock()

	m.restart(ctx, eng, c, st, problem)
}

// diagnose returns a description of what is wrong with the app, or "" if
// it is fine. It also reports whether it made a health request and, if so,
// how long the answer took.
func (m *Monitor) diagnose(ctx context.Context, eng engine.Engine, c engine.Container) (problem string, probed bool, took time.Duration) {
	if !c.Running() {
		return stoppedReason(ctx, eng, c), false, 0
	}
	// Without a health path (a worker has no port to ask), the image's own
	// HEALTHCHECK is what there is, if it has one; the listing does not
	// carry its state.
	if c.HealthPath == "" || c.HostPort == 0 {
		if ins, err := eng.Inspect(ctx, c.Name); err == nil && ins.Health == engine.HealthUnhealthy {
			return "the image's HEALTHCHECK reports it unhealthy", false, 0
		}
		return "", false, 0
	}

	probe := m.Probe
	if probe == nil {
		probe = health.Probe
	}
	started := m.now()
	err := probe(ctx, health.URL(c.HostIP, c.HostPort, c.HealthPath))
	took = m.now().Sub(started)
	if err != nil {
		return "health check failed: " + err.Error(), true, took
	}
	return "", true, took
}

// stoppedReason describes why a container is not running. The listing only
// says that it stopped; the exit code and whether the kernel killed it for
// using too much memory need a closer look, and they are what tells a crash
// in the app from a limit that is set too low.
func stoppedReason(ctx context.Context, eng engine.Engine, c engine.Container) string {
	reason := "container is " + c.State

	info, err := eng.Inspect(ctx, c.Name)
	if err != nil {
		// The basic reason is still true; the details are a bonus.
		return reason
	}
	switch {
	case info.OOMKilled:
		return reason + " (killed for exceeding its memory limit)"
	case info.State == "exited":
		return fmt.Sprintf("%s (exit code %d)", reason, info.ExitCode)
	default:
		return reason
	}
}

// restart restarts the container of an app and schedules the next check.
func (m *Monitor) restart(ctx context.Context, eng engine.Engine, c engine.Container, st *appState, reason string) {
	// A deploy or a stop is working on this app right now. Whatever is
	// wrong is either being fixed or being caused by it; a restart from
	// here would only interfere. The next pass looks again.
	unlock, ok := m.Locks.TryLock(c.App)
	if !ok {
		return
	}
	defer unlock()

	started := m.now()
	m.logger().Warn("restarting app", "app", c.App, "container", c.Name, "reason", reason)
	err := eng.Restart(ctx, c.Name)
	finished := m.now()

	m.mu.Lock()
	st.failures = 0
	st.status.Restarts++
	st.status.LastRestart = finished
	// Wait before looking again: the app needs time to start, and an app
	// that fails again and again must not be restarted in a tight loop.
	delay := m.backoff(st.status.Restarts)
	st.notBefore = finished.Add(delay)
	if err != nil {
		st.status.Health = Down
		st.status.Detail = fmt.Sprintf("%s; restart failed: %v; next attempt in %s", reason, err, delay)
	} else {
		st.status.Health = Restarting
		st.status.Detail = fmt.Sprintf("%s; restarted, next check in %s", reason, delay)
	}
	m.mu.Unlock()

	if err != nil {
		m.logger().Error("restart failed", "app", c.App, "container", c.Name, "error", err)
	}
	if m.OnRestart != nil {
		m.OnRestart(Event{
			App:       c.App,
			Release:   c.Release,
			Container: c.Name,
			Reason:    reason,
			Err:       err,
			Started:   started,
			Duration:  finished.Sub(started),
		})
	}
}

// backoff returns how long to wait after the n-th restart in a row: the
// interval, then twice that, four times, and so on up to MaxBackoff.
//
// Doubling is the standard answer to "it failed again": it retries quickly
// when the problem is likely to be brief and slowly when it evidently is
// not, without anyone having to decide which case this is.
func (m *Monitor) backoff(restarts int) time.Duration {
	limit := m.MaxBackoff
	if limit == 0 {
		limit = DefaultMaxBackoff
	}

	delay := m.interval()
	for i := 1; i < restarts; i++ {
		delay *= 2
		// Checked inside the loop: after enough doublings the value would
		// overflow and turn negative.
		if delay >= limit {
			return limit
		}
	}
	return min(delay, limit)
}

// Status returns what is known about app. The second result is false if the
// monitor has not seen the app yet.
func (m *Monitor) Status(app string) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.apps[app]
	if !ok || st.status.CheckedAt.IsZero() && st.status.LastRestart.IsZero() {
		return Status{}, false
	}
	// A copy is returned: the caller must not hold a pointer into data that
	// other goroutines keep changing.
	return st.status, true
}

// state returns the memory for app, creating it on first sight.
//
// The memory belongs to one container. When a deploy has replaced the
// container, what was learned about the old one (its failures, its restart
// count, a pending delay) says nothing about the new one and is discarded:
// a new release starts with a clean record.
func (m *Monitor) state(app, container string) *appState {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.apps == nil {
		m.apps = map[string]*appState{}
	}
	st, ok := m.apps[app]
	if !ok || st.container != container {
		st = &appState{container: container, status: Status{App: app}}
		m.apps[app] = st
	}
	return st
}

// forget drops the memory of apps that no longer exist, so a stopped app
// does not leave its state behind forever.
func (m *Monitor) forget(existing map[string]engine.Container) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for app := range m.apps {
		if _, ok := existing[app]; !ok {
			// Deleting from a map while ranging over it is allowed in Go.
			delete(m.apps, app)
		}
	}
}

func (m *Monitor) interval() time.Duration {
	if m.Interval > 0 {
		return m.Interval
	}
	return DefaultInterval
}

func (m *Monitor) failureThreshold() int {
	if m.FailureThreshold > 0 {
		return m.FailureThreshold
	}
	return DefaultFailureThreshold
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Monitor) logger() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.New(slog.DiscardHandler)
}
