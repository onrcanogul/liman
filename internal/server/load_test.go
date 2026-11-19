//go:build load

// Load tests. They are excluded from normal test runs by the build
// constraint above and take about a minute:
//
//	make load-test
//
// By default they start a complete daemon inside the test process: a real
// HTTP server and a real SQLite database, with Docker replaced by an
// in-memory engine. That measures liman itself, not Docker.
//
// To aim the read-only scenarios at a running daemon instead (with real
// Docker behind it), set LIMAN_LOAD_URL and LIMAN_LOAD_TOKEN.
//
// The numbers depend on the machine and say nothing absolute; what matters
// is how they change between scenarios and between runs on one machine.

package server_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/loadtest"
	"github.com/onrcanogul/liman/internal/server"
	"github.com/onrcanogul/liman/internal/server/servertest"
)

// loadDuration is how long each scenario runs. LIMAN_LOAD_SECONDS overrides
// the default.
func loadDuration() time.Duration {
	if s, err := strconv.Atoi(os.Getenv("LIMAN_LOAD_SECONDS")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return 3 * time.Second
}

// loadTarget is the daemon under test.
type loadTarget struct {
	url   string
	token string
	// harness is nil when the target is an external daemon.
	harness *servertest.Harness
	// http keeps one connection per worker open.
	http *http.Client
}

// newLoadTarget returns the external daemon named by the environment, or
// starts one in this process.
func newLoadTarget(t *testing.T, workers int, options ...func(*server.Server)) *loadTarget {
	t.Helper()

	target := &loadTarget{
		http: &http.Client{
			Transport: &http.Transport{
				// The default transport keeps only two idle connections per
				// host. With more workers than that, most requests would
				// open a fresh connection, and the test would measure TCP
				// handshakes (and soon run out of local ports) rather than
				// the daemon.
				MaxIdleConns:        workers * 2,
				MaxIdleConnsPerHost: workers * 2,
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			},
			Timeout: 30 * time.Second,
		},
	}
	t.Cleanup(target.http.CloseIdleConnections)

	if url := os.Getenv("LIMAN_LOAD_URL"); url != "" {
		target.url, target.token = strings.TrimSuffix(url, "/"), os.Getenv("LIMAN_LOAD_TOKEN")
		return target
	}

	h := servertest.New(t, options...)
	// A growing list of recorded calls would look like a memory leak.
	h.Engine.RecordCalls = false
	target.harness, target.url, target.token = h, h.URL, h.Client.Token
	return target
}

// get performs one GET and classifies the outcome for the report.
func (lt *loadTarget) get(ctx context.Context, path string, authenticated bool) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lt.url+path, nil)
	if err != nil {
		return "bad request"
	}
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+lt.token)
	}

	resp, err := lt.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ""
		}
		return "transport error"
	}
	// The body must be read to the end and closed, or the connection
	// cannot be reused for the next request.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return ""
	}
	return strconv.Itoa(resp.StatusCode)
}

// TestLoadReads measures the read-only endpoints at rising concurrency.
func TestLoadReads(t *testing.T) {
	const maxWorkers = 256
	lt := newLoadTarget(t, maxWorkers)

	if lt.harness != nil {
		// Something for the endpoints to return: a few running apps and a
		// history worth querying.
		for i := range 20 {
			lt.harness.Engine.Add(engine.Container{
				Name: fmt.Sprintf("app-%d-r1", i), App: fmt.Sprintf("app-%d", i),
				Image: "ghcr.io/user/app:1.0", Status: "Up 2 hours", HostPort: 40000 + i,
			})
		}
		client := &api.Client{BaseURL: lt.url, Token: lt.token}
		for i := range 40 {
			def := strings.Replace(definition, "name: orders-api", fmt.Sprintf("name: seed-%d", i), 1)
			if _, err := client.Deploy(context.Background(), []byte(def), api.DeployOptions{}, io.Discard); err != nil {
				t.Fatalf("seed deploy: %v", err)
			}
		}
	}

	scenarios := []struct {
		name          string
		path          string
		authenticated bool
	}{
		// No token, no database, no engine: the cost of HTTP alone.
		{"health", "/v1/health", false},
		// Token check (database) plus a listing from the engine.
		{"apps", "/v1/apps", true},
		// Token check plus a query with a result set.
		{"history", "/v1/history?limit=50", true},
	}

	var reports []loadtest.Report
	for _, sc := range scenarios {
		// 64 is the daemon's concurrency limit; 256 goes well beyond it.
		for _, workers := range []int{1, 8, 64, maxWorkers} {
			reports = append(reports, loadtest.Run(context.Background(), loadtest.Config{
				Name:        sc.name,
				Concurrency: workers,
				Duration:    loadDuration(),
				Do: func(ctx context.Context, _, _ int) string {
					return lt.get(ctx, sc.path, sc.authenticated)
				},
			}))
		}
	}

	var out strings.Builder
	loadtest.Print(&out, reports)
	t.Logf("read endpoints (target %s)\n%s", lt.url, out.String())

	for _, r := range reports {
		// Beyond the limit the daemon is supposed to shed load with 503;
		// anything else, at any concurrency, is a defect.
		for label, n := range r.Outcomes {
			if label != "ok" && label != "503" {
				t.Errorf("%s with %d workers: %d requests ended as %q", r.Name, r.Concurrency, n, label)
			}
		}
		if r.Concurrency <= 64 && r.Outcomes["503"] > 0 {
			t.Errorf("%s with %d workers: %d requests were shed although the limit is 64", r.Name, r.Concurrency, r.Outcomes["503"])
		}
	}
}

// TestLoadDeploys measures deploys of different apps running concurrently.
// Every deploy writes a handful of rows, so this is where the single
// database connection is under the most pressure.
func TestLoadDeploys(t *testing.T) {
	if os.Getenv("LIMAN_LOAD_URL") != "" {
		t.Skip("deploys are only load-tested against the in-process daemon")
	}

	var reports []loadtest.Report
	for _, workers := range []int{1, 8, 32} {
		lt := newLoadTarget(t, workers)
		client := &api.Client{BaseURL: lt.url, Token: lt.token, HTTP: lt.http}

		report := loadtest.Run(context.Background(), loadtest.Config{
			Name:        "deploy",
			Concurrency: workers,
			Duration:    loadDuration(),
			// Each worker redeploys its own app over and over, so the
			// per-app lock never rejects anything.
			Do: func(ctx context.Context, worker, _ int) string {
				def := strings.Replace(definition, "name: orders-api", fmt.Sprintf("name: load-%d", worker), 1)
				_, err := client.Deploy(ctx, []byte(def), api.DeployOptions{}, io.Discard)
				return deployOutcome(ctx, err)
			},
		})
		reports = append(reports, report)

		// Whatever the load, the state must be consistent afterwards: one
		// container per app, and a release recorded for every deploy.
		if !eventually(func() bool { return containerCount(t, client) == workers }) {
			t.Errorf("%d workers left %d containers, want one per app", workers, containerCount(t, client))
		}
		recorded := 0
		for w := range workers {
			releases, err := lt.harness.Store.Releases(context.Background(), fmt.Sprintf("load-%d", w))
			if err != nil {
				t.Fatal(err)
			}
			recorded += len(releases)
		}
		// Deploys cut off by the end of the run are recorded but not
		// counted by the report, hence "at least".
		if recorded < report.Outcomes["ok"] {
			t.Errorf("%d deploys completed but only %d releases were recorded", report.Requests, recorded)
		}
	}

	var out strings.Builder
	loadtest.Print(&out, reports)
	t.Logf("concurrent deploys of different apps (in-memory engine)\n%s", out.String())

	for _, r := range reports {
		if r.Failures > 0 {
			t.Errorf("deploy with %d workers: %d of %d failed: %v", r.Concurrency, r.Failures, r.Requests, r.Outcomes)
		}
	}
}

// TestLoadSameAppContention checks the per-app lock under pressure: many
// clients deploying the same app at once.
func TestLoadSameAppContention(t *testing.T) {
	if os.Getenv("LIMAN_LOAD_URL") != "" {
		t.Skip("contention is only load-tested against the in-process daemon")
	}

	const workers = 16
	lt := newLoadTarget(t, workers)
	client := &api.Client{BaseURL: lt.url, Token: lt.token, HTTP: lt.http}

	report := loadtest.Run(context.Background(), loadtest.Config{
		Name:        "same app",
		Concurrency: workers,
		Duration:    loadDuration(),
		Do: func(ctx context.Context, _, _ int) string {
			_, err := client.Deploy(ctx, []byte(definition), api.DeployOptions{}, io.Discard)
			return deployOutcome(ctx, err)
		},
	})

	var out strings.Builder
	loadtest.Print(&out, []loadtest.Report{report})
	t.Logf("16 clients deploying the same app\n%s", out.String())

	for label, n := range report.Outcomes {
		if label != "ok" && label != "409" {
			t.Errorf("%d deploys ended as %q; only success and 409 are acceptable", n, label)
		}
	}
	if report.Outcomes["ok"] == 0 || report.Outcomes["409"] == 0 {
		t.Errorf("outcomes = %v, want both successful deploys and rejected ones", report.Outcomes)
	}
	// The property the lock exists for: however many clients fought over
	// the app, exactly one container is left.
	if !eventually(func() bool { return containerCount(t, client) == 1 }) {
		t.Errorf("%d containers left after the contention, want exactly 1", containerCount(t, client))
	}
}

// TestLoadLeavesNothingBehind runs a mix of everything and then checks that
// the daemon returns to where it started: no goroutines left running and no
// memory that keeps growing.
func TestLoadLeavesNothingBehind(t *testing.T) {
	if os.Getenv("LIMAN_LOAD_URL") != "" {
		t.Skip("the leak check needs the daemon to run in this process")
	}

	const workers = 32
	lt := newLoadTarget(t, workers)
	client := &api.Client{BaseURL: lt.url, Token: lt.token, HTTP: lt.http}

	run := func() loadtest.Report {
		return loadtest.Run(context.Background(), loadtest.Config{
			Name:        "mixed",
			Concurrency: workers,
			Duration:    loadDuration(),
			Do: func(ctx context.Context, worker, i int) string {
				// Mostly reads, some deploys, some requests that are
				// rejected, and some that are abandoned halfway.
				switch i % 10 {
				case 0:
					def := strings.Replace(definition, "name: orders-api", fmt.Sprintf("name: mix-%d", worker), 1)
					_, err := client.Deploy(ctx, []byte(def), api.DeployOptions{}, io.Discard)
					return deployOutcome(ctx, err)
				case 1:
					// A client that gives up almost immediately.
					short, cancel := context.WithTimeout(ctx, 200*time.Microsecond)
					defer cancel()
					lt.get(short, "/v1/history?limit=100", true)
					return ""
				case 2:
					if got := lt.get(ctx, "/v1/apps/does-not-exist/logs", true); got != "404" {
						return "want 404, got " + got
					}
					return ""
				default:
					return lt.get(ctx, "/v1/apps", true)
				}
			},
		})
	}

	// settle gives finished requests a moment to wind down, then reports
	// the number of goroutines and the bytes of live heap.
	settle := func() (goroutines int, heap uint64) {
		lt.http.CloseIdleConnections()
		for range 20 {
			runtime.GC()
			time.Sleep(50 * time.Millisecond)
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return runtime.NumGoroutine(), m.HeapAlloc
	}

	// The first run warms everything up (connection pools, caches, the
	// database's page cache); growth during it is not a leak.
	first := run()
	goroutinesBefore, heapBefore := settle()

	var reports = []loadtest.Report{first}
	for range 3 {
		reports = append(reports, run())
	}
	goroutinesAfter, heapAfter := settle()

	var out strings.Builder
	loadtest.Print(&out, reports)
	t.Logf("mixed load, four rounds\n%s\ngoroutines: %d after round 1, %d after round 4\nlive heap:  %.1f MiB after round 1, %.1f MiB after round 4",
		out.String(), goroutinesBefore, goroutinesAfter, float64(heapBefore)/(1<<20), float64(heapAfter)/(1<<20))

	for _, r := range reports {
		for label, n := range r.Outcomes {
			// A 409 is legitimate here and only here: a round ends by
			// cancelling its requests, and the next round can find the
			// daemon still finishing a deploy of the same app.
			if label != "ok" && label != "409" {
				t.Errorf("mixed load: %d requests ended as %q", n, label)
			}
		}
	}
	// The daemon must end up with one container per app, however the
	// rounds were cut off.
	if !eventually(func() bool { return containerCount(t, client) == workers }) {
		t.Errorf("%d containers left, want one per worker (%d)", containerCount(t, client), workers)
	}
	// A few goroutines of slack for the runtime's own background work.
	if goroutinesAfter > goroutinesBefore+5 {
		t.Errorf("goroutines grew from %d to %d over three more rounds: something is not being cleaned up", goroutinesBefore, goroutinesAfter)
	}
	// Three more rounds of the same work must not need noticeably more
	// memory. The history does grow (it is data), hence the allowance.
	if heapAfter > heapBefore+32<<20 {
		t.Errorf("live heap grew from %.1f to %.1f MiB over three more rounds", float64(heapBefore)/(1<<20), float64(heapAfter)/(1<<20))
	}
}

// deployOutcome classifies the result of a deploy for the report.
func deployOutcome(ctx context.Context, err error) string {
	switch {
	case err == nil || ctx.Err() != nil:
		return ""
	case strings.Contains(err.Error(), "another operation"):
		return "409"
	default:
		return "failed: " + firstLine(err.Error())
	}
}

// eventually polls check until it returns true or a few seconds pass.
//
// When a load run ends, its last requests are cancelled on the client side,
// but the daemon is still finishing (or rolling back) what they started.
// Assertions about the daemon's state must wait for that to settle.
func eventually(check func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// containerCount asks the daemon how many containers are in service. Going
// through the API (rather than reading the fake engine's map) is safe while
// requests are still in flight. Stopped containers kept for a rollback are
// not counted: after the second deploy every app has one of those too.
func containerCount(t *testing.T, client *api.Client) int {
	t.Helper()
	apps, err := client.Apps(context.Background())
	if err != nil {
		t.Fatalf("list apps: %v", err)
	}
	running := 0
	for _, c := range apps {
		if c.State == "running" {
			running++
		}
	}
	return running
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 80 {
		line = line[:80]
	}
	return line
}
