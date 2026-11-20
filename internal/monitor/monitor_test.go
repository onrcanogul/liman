package monitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/applock"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/engine/enginetest"
)

const interval = 10 * time.Second

// fixture is a monitor wired to a fake engine, a fake clock and a fake
// health probe, so every test decides what the world looks like.
type fixture struct {
	*Monitor
	engine *enginetest.Fake

	mu sync.Mutex
	// now is the fake clock.
	now time.Time
	// probeErr is what the health probe returns, per app container port.
	probeErr map[int]error
	probes   int
	events   []Event
	checks   []Check
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	f := &fixture{
		engine:   enginetest.New(),
		now:      time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
		probeErr: map[int]error{},
	}
	f.Monitor = &Monitor{
		Connect:          func(context.Context) (engine.Engine, error) { return f.engine, nil },
		Locks:            &applock.Locks{},
		Interval:         interval,
		FailureThreshold: 3,
		MaxBackoff:       2 * time.Minute,
		Now: func() time.Time {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.now
		},
		Probe: func(_ context.Context, url string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.probes++
			for port, err := range f.probeErr {
				if strings.Contains(url, ":"+itoa(port)+"/") {
					return err
				}
			}
			return nil
		},
		OnRestart: func(ev Event) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.events = append(f.events, ev)
		},
		OnCheck: func(c Check) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.checks = append(f.checks, c)
		},
	}
	return f
}

func itoa(n int) string { return strconv.Itoa(n) }

// advance moves the clock and runs one pass, as one tick of the real loop
// would.
func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
	f.CheckOnce(context.Background())
}

// addApp adds a running container with a health check.
func (f *fixture) addApp(app string, port int) string {
	name := app + "-r1"
	f.engine.Add(engine.Container{Name: name, App: app, HostPort: port, Release: "r1", HealthPath: "/health"})
	return name
}

func (f *fixture) setProbe(port int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeErr[port] = err
}

func (f *fixture) health(t *testing.T, app string) Status {
	t.Helper()
	st, ok := f.Status(app)
	if !ok {
		t.Fatalf("the monitor has no status for %s", app)
	}
	return st
}

func TestHealthyAppIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	name := f.addApp("web", 40001)

	for range 5 {
		f.advance(interval)
	}

	if got := f.engine.Restarts(name); got != 0 {
		t.Errorf("a healthy app was restarted %d times", got)
	}
	if st := f.health(t, "web"); st.Health != Healthy || st.Restarts != 0 || st.Detail != "" {
		t.Errorf("status = %+v, want healthy with no detail", st)
	}
	if f.probes != 5 {
		t.Errorf("%d health probes for 5 passes, want 5", f.probes)
	}
}

func TestStoppedContainerIsRestartedAtOnce(t *testing.T) {
	f := newFixture(t)
	name := f.addApp("web", 40001)
	f.advance(interval)

	// The app crashes.
	f.engine.SetState(name, "exited")
	f.advance(interval)

	if got := f.engine.Restarts(name); got != 1 {
		t.Fatalf("restarts = %d, want 1 on the first pass after the crash", got)
	}
	st := f.health(t, "web")
	if st.Health != Restarting || st.Restarts != 1 || !strings.Contains(st.Detail, "container is exited") {
		t.Errorf("status = %+v, want restarting because the container exited", st)
	}

	if len(f.events) != 1 {
		t.Fatalf("%d restart events, want 1", len(f.events))
	}
	ev := f.events[0]
	if ev.App != "web" || ev.Container != name || ev.Release != "r1" || ev.Reason != "container is exited (exit code 0)" || ev.Err != nil {
		t.Errorf("event = %+v", ev)
	}

	// Once it is back and its grace period is over, it counts as healthy.
	f.advance(interval)
	if st := f.health(t, "web"); st.Health != Healthy {
		t.Errorf("status after recovery = %+v, want healthy", st)
	}
}

func TestFailingHealthCheckNeedsSeveralFailuresInARow(t *testing.T) {
	f := newFixture(t)
	name := f.addApp("web", 40001)
	f.setProbe(40001, errors.New("unexpected status 503"))

	// Two failures: reported, but not acted on.
	for i := 1; i <= 2; i++ {
		f.advance(interval)
		st := f.health(t, "web")
		if st.Health != Unhealthy || !strings.Contains(st.Detail, "unexpected status 503") {
			t.Errorf("after %d failures status = %+v, want unhealthy", i, st)
		}
	}
	if got := f.engine.Restarts(name); got != 0 {
		t.Fatalf("restarted after 2 failures, want the threshold to be 3")
	}

	// The third failure in a row triggers the restart.
	f.advance(interval)
	if got := f.engine.Restarts(name); got != 1 {
		t.Fatalf("restarts after 3 failures = %d, want 1", got)
	}
	if !strings.Contains(f.events[0].Reason, "health check failed: unexpected status 503") {
		t.Errorf("reason = %q", f.events[0].Reason)
	}
}

func TestOneGoodAnswerResetsTheFailureCount(t *testing.T) {
	f := newFixture(t)
	name := f.addApp("web", 40001)

	// Fail, fail, succeed, fail, fail: never three in a row.
	for _, err := range []error{errors.New("timeout"), errors.New("timeout"), nil, errors.New("timeout"), errors.New("timeout")} {
		f.setProbe(40001, err)
		f.advance(interval)
	}

	if got := f.engine.Restarts(name); got != 0 {
		t.Errorf("restarted %d times although the failures were never 3 in a row", got)
	}
}

func TestCrashLoopIsRestartedWithGrowingDelays(t *testing.T) {
	f := newFixture(t)
	name := f.addApp("web", 40001)
	// The app dies again right after every restart.
	f.engine.AfterRestart = "exited"
	f.engine.SetState(name, "exited")

	// Tick once per interval for ten minutes and note when restarts happen.
	var restartedAt []time.Duration
	seen := 0
	for elapsed := interval; elapsed <= 10*time.Minute; elapsed += interval {
		f.advance(interval)
		if n := f.engine.Restarts(name); n > seen {
			seen = n
			restartedAt = append(restartedAt, elapsed)
		}
	}

	// Delays after each restart: 10s, 20s, 40s, 80s, then capped at 120s.
	want := []time.Duration{
		10 * time.Second,  // first pass
		20 * time.Second,  // +10s
		40 * time.Second,  // +20s
		80 * time.Second,  // +40s
		160 * time.Second, // +80s
		280 * time.Second, // +120s (the cap)
		400 * time.Second, // +120s
		520 * time.Second, // +120s
	}
	if len(restartedAt) != len(want) {
		t.Fatalf("restarts at %v, want %v", restartedAt, want)
	}
	for i := range want {
		if restartedAt[i] != want[i] {
			t.Errorf("restart %d at %s, want %s (all: %v)", i+1, restartedAt[i], want[i], restartedAt)
		}
	}

	if st := f.health(t, "web"); st.Restarts != len(want) {
		t.Errorf("Restarts = %d, want %d", st.Restarts, len(want))
	}
}

func TestStableAppIsForgivenItsEarlierRestarts(t *testing.T) {
	f := newFixture(t)
	name := f.addApp("web", 40001)

	// Three crashes in a row push the delay up.
	f.engine.AfterRestart = "exited"
	f.engine.SetState(name, "exited")
	for range 8 {
		f.advance(interval)
	}
	if st := f.health(t, "web"); st.Restarts < 3 {
		t.Fatalf("Restarts = %d, want at least 3 to set the scene", st.Restarts)
	}

	// Then the app recovers and stays up.
	f.engine.AfterRestart = ""
	f.engine.SetState(name, "running")
	for range 30 {
		f.advance(interval)
	}
	if st := f.health(t, "web"); st.Health != Healthy || st.Restarts != 0 {
		t.Fatalf("status after a long healthy stretch = %+v, want healthy with the count reset", st)
	}

	// Its next crash is handled like a first one: restarted on the next
	// pass, with the shortest delay.
	before := f.engine.Restarts(name)
	f.engine.SetState(name, "exited")
	f.advance(interval)
	if got := f.engine.Restarts(name) - before; got != 1 {
		t.Errorf("restarts on the pass after the new crash = %d, want 1", got)
	}
	if st := f.health(t, "web"); !strings.Contains(st.Detail, "next check in 10s") {
		t.Errorf("detail = %q, want the shortest delay again", st.Detail)
	}
}

func TestAppBeingDeployedIsNotTouched(t *testing.T) {
	f := newFixture(t)
	name := f.addApp("web", 40001)
	f.engine.SetState(name, "exited")

	// A deploy holds the lock.
	unlock, ok := f.Locks.TryLock("web")
	if !ok {
		t.Fatal("could not take the lock")
	}
	for range 3 {
		f.advance(interval)
	}
	if got := f.engine.Restarts(name); got != 0 {
		t.Fatalf("the monitor restarted an app %d times while it was locked", got)
	}

	// When the deploy is done, the monitor takes over again.
	unlock()
	f.advance(interval)
	if got := f.engine.Restarts(name); got != 1 {
		t.Errorf("restarts after the lock was released = %d, want 1", got)
	}
	// And it released the lock itself afterwards.
	if release, ok := f.Locks.TryLock("web"); !ok {
		t.Error("the monitor kept the lock after restarting")
	} else {
		release()
	}
}

func TestFailedRestartIsReportedAndRetriedLater(t *testing.T) {
	f := newFixture(t)
	name := f.addApp("web", 40001)
	f.engine.SetState(name, "exited")
	f.engine.RestartErr = errors.New("driver failed programming external connectivity")

	f.advance(interval)

	st := f.health(t, "web")
	if st.Health != Down || !strings.Contains(st.Detail, "restart failed: driver failed") {
		t.Errorf("status = %+v, want down with the restart error", st)
	}
	if len(f.events) != 1 || f.events[0].Err == nil {
		t.Fatalf("events = %+v, want one failed restart", f.events)
	}

	// The failure does not stop the monitor from trying again, later.
	f.engine.RestartErr = nil
	f.advance(interval) // still waiting out the delay
	f.advance(interval)
	if got := f.engine.Restarts(name); got != 1 {
		t.Errorf("successful restarts = %d, want 1 once the engine works again", got)
	}
}

func TestAppWithoutHealthCheckIsOnlyCheckedForRunning(t *testing.T) {
	f := newFixture(t)
	f.engine.Add(engine.Container{Name: "worker-r1", App: "worker", Release: "r1"})

	f.advance(interval)

	if f.probes != 0 {
		t.Errorf("%d health probes for an app without a health check", f.probes)
	}
	if st := f.health(t, "worker"); st.Health != Healthy {
		t.Errorf("status = %+v, want healthy because it is running", st)
	}

	f.engine.SetState("worker-r1", "exited")
	f.advance(interval)
	if got := f.engine.Restarts("worker-r1"); got != 1 {
		t.Errorf("restarts = %d, want the stopped container to be restarted", got)
	}
}

func TestOnlyTheNewestContainerOfAnAppIsWatched(t *testing.T) {
	f := newFixture(t)
	// An old container left behind (stopped) next to the current one.
	f.engine.Add(engine.Container{Name: "web-r1", App: "web", State: "exited", Release: "r1", HealthPath: "/health"})
	f.engine.Add(engine.Container{Name: "web-r2", App: "web", HostPort: 40002, Release: "r2", HealthPath: "/health"})

	f.advance(interval)

	if got := f.engine.Restarts("web-r1"); got != 0 {
		t.Errorf("the old container was restarted %d times; only the newest serves", got)
	}
	if st := f.health(t, "web"); st.Health != Healthy {
		t.Errorf("status = %+v, want healthy", st)
	}
}

func TestAppsDoNotAffectEachOther(t *testing.T) {
	f := newFixture(t)
	good := f.addApp("good", 40001)
	bad := f.addApp("bad", 40002)
	f.setProbe(40002, errors.New("connection refused"))

	for range 3 {
		f.advance(interval)
	}

	if got := f.engine.Restarts(bad); got != 1 {
		t.Errorf("the failing app was restarted %d times, want 1", got)
	}
	if got := f.engine.Restarts(good); got != 0 {
		t.Errorf("the healthy app was restarted %d times", got)
	}
	if st := f.health(t, "good"); st.Health != Healthy {
		t.Errorf("status of the healthy app = %+v", st)
	}
}

func TestStoppedAppIsForgotten(t *testing.T) {
	f := newFixture(t)
	name := f.addApp("web", 40001)
	f.advance(interval)
	if _, ok := f.Status("web"); !ok {
		t.Fatal("no status for a running app")
	}

	// "liman stop" removes the container.
	if err := f.engine.Remove(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	f.advance(interval)

	if _, ok := f.Status("web"); ok {
		t.Error("the monitor still reports an app that no longer exists")
	}
}

func TestEngineOutageIsSurvived(t *testing.T) {
	f := newFixture(t)
	name := f.addApp("web", 40001)

	down := true
	f.Connect = func(context.Context) (engine.Engine, error) {
		if down {
			return nil, errors.New("cannot connect to the Docker daemon")
		}
		return f.engine, nil
	}

	// Passes during the outage do nothing and do not panic.
	for range 3 {
		f.advance(interval)
	}

	down = false
	f.engine.SetState(name, "exited")
	f.advance(interval)
	if got := f.engine.Restarts(name); got != 1 {
		t.Errorf("restarts after the engine came back = %d, want 1", got)
	}
}

func TestBackoff(t *testing.T) {
	m := &Monitor{Interval: 10 * time.Second, MaxBackoff: 5 * time.Minute}

	tests := []struct {
		restarts int
		want     time.Duration
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{5, 160 * time.Second},
		{6, 5 * time.Minute}, // 320s would exceed the cap
		{7, 5 * time.Minute},
		{1000, 5 * time.Minute}, // must not overflow into a negative delay
	}
	for _, tt := range tests {
		if got := m.backoff(tt.restarts); got != tt.want {
			t.Errorf("backoff(%d) = %s, want %s", tt.restarts, got, tt.want)
		}
	}
}

func TestRunChecksPeriodicallyAndStopsWhenCancelled(t *testing.T) {
	fake := enginetest.New()
	fake.Add(engine.Container{Name: "web-r1", App: "web", State: "exited", Release: "r1"})

	m := &Monitor{
		Connect:  func(context.Context) (engine.Engine, error) { return fake, nil },
		Locks:    &applock.Locks{},
		Interval: 10 * time.Millisecond, // the real clock, but fast
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// The loop must find and restart the stopped container on its own.
	deadline := time.Now().Add(5 * time.Second)
	for fake.Restarts("web-r1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run() did not restart the stopped container")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after its context was cancelled")
	}
}

func TestManyAppsAreCheckedConcurrentlyButBounded(t *testing.T) {
	f := newFixture(t)
	for i := range 20 {
		f.addApp("app-"+itoa(i+1), 41000+i)
	}

	// Each probe takes a moment; count how many run at the same time.
	var mu sync.Mutex
	running, peak := 0, 0
	f.Probe = func(context.Context, string) error {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()

		time.Sleep(5 * time.Millisecond)

		mu.Lock()
		running--
		mu.Unlock()
		return nil
	}

	started := time.Now()
	f.advance(interval)
	elapsed := time.Since(started)

	if peak > maxParallelChecks {
		t.Errorf("%d probes ran at once, want at most %d", peak, maxParallelChecks)
	}
	if peak < 2 {
		t.Errorf("only %d probe ran at a time, want the checks to overlap", peak)
	}
	// Twenty sequential probes would take 100ms.
	if elapsed > 80*time.Millisecond {
		t.Errorf("the pass took %s, want the checks to run in parallel", elapsed)
	}
}

func TestStoppedReasonIncludesExitCodeAndOOM(t *testing.T) {
	tests := []struct {
		name      string
		container engine.Container
		want      string
	}{
		{"crash", engine.Container{State: "exited", ExitCode: 1}, "container is exited (exit code 1)"},
		{"killed", engine.Container{State: "exited", ExitCode: 137}, "container is exited (exit code 137)"},
		{"out of memory", engine.Container{State: "exited", ExitCode: 137, OOMKilled: true}, "container is exited (killed for exceeding its memory limit)"},
		{"other state", engine.Container{State: "dead"}, "container is dead"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			c := tt.container
			c.Name, c.App = "web-r1", "web"
			f.engine.Add(c)
			// Add defaults an empty state to running; set it explicitly.
			f.engine.SetState("web-r1", tt.container.State)

			f.advance(interval)

			if len(f.events) != 1 || f.events[0].Reason != tt.want {
				t.Errorf("events = %+v, want one restart with reason %q", f.events, tt.want)
			}
		})
	}
}

func TestNewReleaseStartsWithACleanRecord(t *testing.T) {
	f := newFixture(t)
	old := f.addApp("web", 40001)

	// The old release keeps crashing and has built up a long delay.
	f.engine.AfterRestart = "exited"
	f.engine.SetState(old, "exited")
	for range 8 {
		f.advance(interval)
	}
	if st := f.health(t, "web"); st.Restarts < 3 {
		t.Fatalf("Restarts = %d, want at least 3 to set the scene", st.Restarts)
	}

	// A deploy replaces it with a working release.
	f.engine.AfterRestart = ""
	if err := f.engine.Remove(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	f.engine.Add(engine.Container{Name: "web-r2", App: "web", HostPort: 40002, Release: "r2", HealthPath: "/health"})

	// The very next pass looks at it (no leftover delay) and reports it as
	// healthy, without the old release's restarts.
	f.advance(interval)
	st := f.health(t, "web")
	if st.Health != Healthy || st.Restarts != 0 || !st.LastRestart.IsZero() {
		t.Errorf("status of the new release = %+v, want a clean healthy record", st)
	}
}

func TestEveryCheckIsReported(t *testing.T) {
	f := newFixture(t)
	f.addApp("web", 40001)
	// An app without a health check: running is all there is to see.
	f.engine.Add(engine.Container{Name: "plain-r1", App: "plain", HostPort: 40002, Release: "r1"})

	f.advance(interval)
	f.setProbe(40001, errors.New("503"))
	f.advance(interval)

	// The two apps are checked concurrently, so the order between them is
	// not fixed; look at each app's checks on their own.
	f.mu.Lock()
	byApp := map[string][]Check{}
	for _, c := range f.checks {
		byApp[c.App] = append(byApp[c.App], c)
	}
	f.mu.Unlock()

	web := byApp["web"]
	if len(web) != 2 || !web[0].OK || web[1].OK || !web[0].Probed || !web[1].Probed {
		t.Errorf("checks of web = %+v, want a probed success and then a probed failure", web)
	}
	if len(web) == 2 && !web[1].Time.Equal(web[0].Time.Add(interval)) {
		t.Errorf("check times = %s and %s, want them one interval apart", web[0].Time, web[1].Time)
	}
	plain := byApp["plain"]
	if len(plain) != 2 || !plain[0].OK || plain[0].Probed {
		t.Errorf("checks of plain = %+v, want two successes without a probe", plain)
	}
}

func TestNoCheckIsReportedWhileAnAppIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	name := f.addApp("web", 40001)

	f.engine.SetState(name, "exited")
	f.advance(interval) // restarted; the grace period starts

	f.mu.Lock()
	before := len(f.checks)
	f.mu.Unlock()

	// Half an interval later the app is still inside its grace period: it
	// is not looked at, so there is nothing to report. A gap in the history
	// is the honest record of that.
	f.advance(interval / 2)

	f.mu.Lock()
	defer f.mu.Unlock()
	if before != 1 || len(f.checks) != before {
		t.Errorf("checks reported = %d, then %d; want 1 and no more during the grace period", before, len(f.checks))
	}
	if f.checks[0].OK || f.checks[0].Probed {
		t.Errorf("check of a stopped container = %+v, want a failure without a probe", f.checks[0])
	}
}
