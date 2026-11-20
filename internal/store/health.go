package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// HealthSample is the outcome of one health check of an app.
type HealthSample struct {
	Time time.Time
	App  string
	OK   bool
	// Probed says whether the check made an HTTP request. Without one (the
	// app defines no health check, or its container was not running)
	// Duration means nothing and is not stored.
	Probed   bool
	Duration time.Duration
}

// AppendHealthSample records one health check.
func (s *Store) AppendHealthSample(ctx context.Context, h HealthSample) error {
	// sql.NullInt64 is how database/sql spells "an integer or NULL". Storing
	// 0 instead would drag the averages down with numbers nobody measured.
	duration := sql.NullInt64{Int64: h.Duration.Milliseconds(), Valid: h.Probed}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO health_samples (at, app, ok, duration_ms) VALUES (?, ?, ?, ?)`,
		h.Time.Unix(), h.App, h.OK, duration)
	if err != nil {
		return fmt.Errorf("record health sample: %w", err)
	}
	return nil
}

// HealthBucket summarises the health checks of one app in one stretch of
// time.
type HealthBucket struct {
	// Start is the beginning of the stretch.
	Start time.Time
	// OK and Failed count the checks by outcome.
	OK     int
	Failed int
	// Probed counts the checks that measured a response time; AvgMS and
	// MaxMS are about those and are zero when there were none.
	Probed int
	AvgMS  float64
	MaxMS  int64
}

// HealthBuckets groups the samples of app from since up to until into
// stretches of the given width, oldest first. Stretches without a single
// sample are left out: "nothing was measured" is not the same as "healthy",
// and the caller must be able to tell.
func (s *Store) HealthBuckets(ctx context.Context, app string, since, until time.Time, width time.Duration) ([]HealthBucket, error) {
	step := int64(width / time.Second)
	if step < 1 {
		return nil, errors.New("health buckets must be at least a second wide")
	}
	from := since.Unix()

	// The database does the grouping. Reading every row into Go and
	// counting there would move a week of samples (60 000 per app at the
	// default interval) across the driver for a result of a few dozen rows.
	//
	// (at - from) / step is integer division: it numbers the stretches 0,
	// 1, 2, ... AVG and MAX skip NULLs, which is exactly the rule wanted
	// for checks without a response time.
	rows, err := s.db.QueryContext(ctx,
		`SELECT (at - ?) / ? AS bucket,
		        SUM(ok), SUM(1 - ok),
		        COUNT(duration_ms), COALESCE(AVG(duration_ms), 0), COALESCE(MAX(duration_ms), 0)
		 FROM health_samples
		 WHERE app = ? AND at >= ? AND at < ?
		 GROUP BY bucket
		 ORDER BY bucket`,
		from, step, app, from, until.Unix())
	if err != nil {
		return nil, fmt.Errorf("read health samples: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var buckets []HealthBucket
	for rows.Next() {
		var b HealthBucket
		var n int64
		if err := rows.Scan(&n, &b.OK, &b.Failed, &b.Probed, &b.AvgMS, &b.MaxMS); err != nil {
			return nil, fmt.Errorf("read health samples: %w", err)
		}
		b.Start = time.Unix(from+n*step, 0).UTC()
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read health samples: %w", err)
	}
	return buckets, nil
}

// PruneHealthSamples deletes the samples older than before and returns how
// many there were. Without it the table would grow for as long as the
// daemon runs.
func (s *Store) PruneHealthSamples(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM health_samples WHERE at < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("prune health samples: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune health samples: %w", err)
	}
	return n, nil
}
