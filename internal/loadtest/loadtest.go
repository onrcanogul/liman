// Package loadtest is a small load generator: it calls a function from many
// goroutines for a fixed time and reports throughput and latency.
//
// It is deliberately simple. Each worker sends its next request as soon as
// the previous one has finished (a "closed loop"), so the offered load
// adapts to how fast the system answers. That measures capacity well; it
// does not show what happens when requests keep arriving at a fixed rate
// regardless of how slow the system has become.
package loadtest

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// Config describes one load run.
type Config struct {
	// Name labels the run in the report.
	Name string
	// Concurrency is the number of workers calling Do at the same time.
	Concurrency int
	// Duration is how long the workers keep going.
	Duration time.Duration
	// Do performs one request. It returns a short label describing the
	// outcome ("200", "503", "timeout"); an empty label counts as success.
	// It is called concurrently and must be safe for that.
	Do func(ctx context.Context, worker, iteration int) string
}

// Report is the result of one run.
type Report struct {
	Name        string
	Concurrency int
	Elapsed     time.Duration

	// Requests is the number of calls to Do; Failures those that returned
	// a label.
	Requests int
	Failures int
	// Outcomes counts the calls by label.
	Outcomes map[string]int

	// Latency percentiles: half of the requests were faster than P50, 95%
	// faster than P95, 99% faster than P99.
	P50, P95, P99, Max time.Duration
}

// PerSecond returns the throughput: completed requests per second.
func (r Report) PerSecond() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Requests) / r.Elapsed.Seconds()
}

// Run executes cfg and returns its report.
func Run(ctx context.Context, cfg Config) Report {
	ctx, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()

	// Every worker records into its own slice and map. Sharing one of each
	// would need a lock on every request, and the load generator would end
	// up measuring its own lock instead of the system under test.
	type result struct {
		latencies []time.Duration
		outcomes  map[string]int
	}
	results := make([]result, cfg.Concurrency)

	// A WaitGroup counts running goroutines: Add before starting one, Done
	// when it ends, and Wait blocks until the count is back to zero.
	var wg sync.WaitGroup
	started := time.Now()

	for w := range cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := result{outcomes: map[string]int{}}

			for i := 0; ctx.Err() == nil; i++ {
				begin := time.Now()
				label := cfg.Do(ctx, w, i)

				// A request cut short by the end of the run says nothing
				// about the system; leave it out.
				if ctx.Err() != nil {
					break
				}
				res.latencies = append(res.latencies, time.Since(begin))
				if label == "" {
					label = "ok"
				}
				res.outcomes[label]++
			}
			// Each worker writes only to its own element, so no lock is
			// needed here either.
			results[w] = res
		}()
	}
	wg.Wait()

	report := Report{
		Name:        cfg.Name,
		Concurrency: cfg.Concurrency,
		Elapsed:     time.Since(started),
		Outcomes:    map[string]int{},
	}

	var all []time.Duration
	for _, res := range results {
		all = append(all, res.latencies...)
		for label, n := range res.outcomes {
			report.Outcomes[label] += n
			if label != "ok" {
				report.Failures += n
			}
		}
	}
	report.Requests = len(all)

	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	report.P50 = percentile(all, 50)
	report.P95 = percentile(all, 95)
	report.P99 = percentile(all, 99)
	if len(all) > 0 {
		report.Max = all[len(all)-1]
	}
	return report
}

// percentile returns the value below which p percent of the sorted values
// fall.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	// The index of the last value inside the percentile, rounding up.
	idx := (len(sorted)*p + 99) / 100
	return sorted[min(max(idx, 1), len(sorted))-1]
}

// Print writes the reports as an aligned table.
func Print(w io.Writer, reports []Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "scenario\tworkers\trequests\treq/s\tp50\tp95\tp99\tmax\toutcomes\t")
	for _, r := range reports {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.0f\t%s\t%s\t%s\t%s\t%s\t\n",
			r.Name, r.Concurrency, r.Requests, r.PerSecond(),
			round(r.P50), round(r.P95), round(r.P99), round(r.Max),
			formatOutcomes(r.Outcomes))
	}
	_ = tw.Flush()
}

// round shortens a duration to three significant figures or so, which is
// all a table needs.
func round(d time.Duration) time.Duration {
	switch {
	case d >= time.Second:
		return d.Round(10 * time.Millisecond)
	case d >= time.Millisecond:
		return d.Round(10 * time.Microsecond)
	default:
		return d.Round(time.Microsecond)
	}
}

func formatOutcomes(outcomes map[string]int) string {
	labels := make([]string, 0, len(outcomes))
	for label := range outcomes {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	parts := make([]string, 0, len(labels))
	for _, label := range labels {
		parts = append(parts, fmt.Sprintf("%s=%d", label, outcomes[label]))
	}
	return strings.Join(parts, " ")
}
