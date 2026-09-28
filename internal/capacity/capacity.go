// Package capacity records how full each pool is, once a day, so a forecast
// of when pools run out of space has history to work from. Recording starts
// the day the helper updates; the forecast itself reads this table later.
package capacity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	_ "modernc.org/sqlite"
)

// Retention keeps a little over a year: enough for year-on-year growth,
// small enough that the table never matters (one row per pool per day).
const Retention = 400 * 24 * time.Hour

// DefaultInterval re-samples hourly. Each sample overwrites the day's row,
// so the day ends up holding its last reading, and a helper that was down
// for part of a day still records it.
const DefaultInterval = time.Hour

// Sample is one pool's space on one day. Sizes are in bytes.
type Sample struct {
	Day       string // YYYY-MM-DD, UTC
	Pool      string
	Size      int64
	Allocated int64
	Free      int64
	SampledAt time.Time
}

// Store persists samples in their own SQLite file, separate from the events
// DB, so the two never contend for a write lock.
type Store struct {
	db *sql.DB
}

// Open prepares the SQLite file at path. Pass ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// ":memory:" gives every connection its own database; one connection
	// keeps tests (and this low-traffic table) on a single one.
	db.SetMaxOpenConns(1)

	const schema = `
CREATE TABLE IF NOT EXISTS pool_usage (
    pool       TEXT NOT NULL,
    day        TEXT NOT NULL,
    size       INTEGER NOT NULL,
    allocated  INTEGER NOT NULL,
    free       INTEGER NOT NULL,
    sampled_at INTEGER NOT NULL,
    PRIMARY KEY (pool, day)
);`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying DB.
func (s *Store) Close() error { return s.db.Close() }

// Record stores a sample, replacing any earlier reading for the same pool
// and day.
func (s *Store) Record(ctx context.Context, sample Sample) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO pool_usage (pool, day, size, allocated, free, sampled_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (pool, day) DO UPDATE SET
    size = excluded.size,
    allocated = excluded.allocated,
    free = excluded.free,
    sampled_at = excluded.sampled_at`,
		sample.Pool, sample.Day, sample.Size, sample.Allocated, sample.Free, sample.SampledAt.Unix())
	if err != nil {
		return fmt.Errorf("record %s: %w", sample.Pool, err)
	}
	return nil
}

// History returns a pool's samples from since onwards, oldest first.
func (s *Store) History(ctx context.Context, pool string, since time.Time) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT pool, day, size, allocated, free, sampled_at
FROM pool_usage
WHERE pool = ? AND day >= ?
ORDER BY day`,
		pool, dayOf(since))
	if err != nil {
		return nil, fmt.Errorf("history %s: %w", pool, err)
	}
	defer rows.Close()

	var out []Sample
	for rows.Next() {
		var sample Sample
		var sampledAt int64
		if err := rows.Scan(&sample.Pool, &sample.Day, &sample.Size, &sample.Allocated, &sample.Free, &sampledAt); err != nil {
			return nil, err
		}
		sample.SampledAt = time.Unix(sampledAt, 0).UTC()
		out = append(out, sample)
	}
	return out, rows.Err()
}

// Prune deletes days before cutoff and reports how many rows went.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pool_usage WHERE day < ?`, dayOf(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune: %w", err)
	}
	return res.RowsAffected()
}

// PoolSource is the slice of the TrueNAS client the recorder needs.
type PoolSource interface {
	GetRaw(ctx context.Context, path string) (json.RawMessage, error)
}

// Recorder samples every pool on an interval.
type Recorder struct {
	source   PoolSource
	store    *Store
	interval time.Duration
	now      func() time.Time
}

// NewRecorder builds a recorder; call Run to start it.
func NewRecorder(source PoolSource, store *Store, interval time.Duration) *Recorder {
	return &Recorder{source: source, store: store, interval: interval, now: time.Now}
}

// Run samples straight away and then every interval, pruning old days as it
// goes. It returns when ctx is cancelled.
func (r *Recorder) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		if err := r.SampleOnce(ctx); err != nil {
			slog.Warn("capacity: sample failed", "err", err)
		}
		if _, err := r.store.Prune(ctx, r.now().Add(-Retention)); err != nil {
			slog.Warn("capacity: prune failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SampleOnce records today's reading for every pool that reports its size.
// Pools without one (exported, offline, mid-import) are skipped rather than
// recorded as empty, which would drag a forecast towards zero.
func (r *Recorder) SampleOnce(ctx context.Context) error {
	raw, err := r.source.GetRaw(ctx, "/pool")
	if err != nil {
		return fmt.Errorf("list pools: %w", err)
	}
	var pools []struct {
		Name      string `json:"name"`
		Size      *int64 `json:"size"`
		Allocated *int64 `json:"allocated"`
		Free      *int64 `json:"free"`
	}
	if err := json.Unmarshal(raw, &pools); err != nil {
		return fmt.Errorf("decode pools: %w", err)
	}

	now := r.now().UTC()
	for _, p := range pools {
		if p.Name == "" || p.Size == nil || p.Allocated == nil || p.Free == nil || *p.Size <= 0 {
			continue
		}
		if err := r.store.Record(ctx, Sample{
			Day:       dayOf(now),
			Pool:      p.Name,
			Size:      *p.Size,
			Allocated: *p.Allocated,
			Free:      *p.Free,
			SampledAt: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }
