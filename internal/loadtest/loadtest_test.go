package loadtest

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunCountsAndClassifies(t *testing.T) {
	var calls atomic.Int64

	report := Run(context.Background(), Config{
		Name:        "demo",
		Concurrency: 4,
		Duration:    100 * time.Millisecond,
		Do: func(_ context.Context, _, iteration int) string {
			calls.Add(1)
			time.Sleep(time.Millisecond)
			if iteration%10 == 9 {
				return "503"
			}
			return ""
		},
	})

	if report.Requests == 0 {
		t.Fatal("no requests were recorded")
	}
	// Requests in flight when the run ended are not recorded, so the
	// report can be short of the call count by at most one per worker.
	if diff := calls.Load() - int64(report.Requests); diff < 0 || diff > 4 {
		t.Errorf("recorded %d of %d calls, want at most one missing per worker", report.Requests, calls.Load())
	}
	if report.Outcomes["ok"]+report.Outcomes["503"] != report.Requests {
		t.Errorf("outcomes %v do not add up to %d requests", report.Outcomes, report.Requests)
	}
	if report.Failures != report.Outcomes["503"] || report.Failures == 0 {
		t.Errorf("Failures = %d, want it to equal the %d labelled outcomes", report.Failures, report.Outcomes["503"])
	}
	if report.P50 < time.Millisecond || report.P50 > report.P95 || report.P95 > report.P99 || report.P99 > report.Max {
		t.Errorf("percentiles are not ordered: p50=%s p95=%s p99=%s max=%s", report.P50, report.P95, report.P99, report.Max)
	}
	if report.PerSecond() <= 0 {
		t.Errorf("PerSecond() = %f, want a positive rate", report.PerSecond())
	}
}

func TestRunStopsAtDuration(t *testing.T) {
	started := time.Now()

	Run(context.Background(), Config{
		Concurrency: 2,
		Duration:    50 * time.Millisecond,
		// A request that only ends when the run's context does.
		Do: func(ctx context.Context, _, _ int) string {
			<-ctx.Done()
			return ""
		},
	})

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("Run() took %s, want it to stop at its duration", elapsed)
	}
}

func TestPercentile(t *testing.T) {
	// 1ms, 2ms, ..., 100ms: the p-th percentile is p milliseconds.
	values := make([]time.Duration, 100)
	for i := range values {
		values[i] = time.Duration(i+1) * time.Millisecond
	}

	for _, p := range []int{50, 95, 99, 100} {
		if got, want := percentile(values, p), time.Duration(p)*time.Millisecond; got != want {
			t.Errorf("percentile(%d) = %s, want %s", p, got, want)
		}
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("percentile of nothing = %s, want 0", got)
	}
	if got := percentile(values[:1], 99); got != time.Millisecond {
		t.Errorf("percentile of one value = %s, want that value", got)
	}
}

func TestPrint(t *testing.T) {
	var out bytes.Buffer

	Print(&out, []Report{{
		Name:        "health",
		Concurrency: 8,
		Elapsed:     2 * time.Second,
		Requests:    1000,
		Outcomes:    map[string]int{"ok": 990, "503": 10},
		P50:         1234 * time.Microsecond,
		Max:         1500 * time.Millisecond,
	}})

	for _, want := range []string{"scenario", "health", "500", "1.23ms", "1.5s", "503=10 ok=990"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table does not contain %q:\n%s", want, out.String())
		}
	}
}
