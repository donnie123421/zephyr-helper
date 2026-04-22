package correlators

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

// DefaultSnapshotRollupThreshold is the number of successful snapshot
// task runs in `DefaultSnapshotLookback` required before the correlator
// emits a rollup parent. Below threshold each snapshot stays visible
// as its own Kind=Job row; at threshold they get grouped and hidden
// under the rollup (via correlates_to) so a NAS with 20 snapshot
// schedules doesn't flood the feed.
const DefaultSnapshotRollupThreshold = 3

// DefaultSnapshotLookback is the window the correlator inspects when
// deciding whether the threshold has been crossed. 24h matches "today"
// for most users and folds overnight schedules onto a single rollup.
const DefaultSnapshotLookback = 24 * time.Hour

// SnapshotRollup collapses multiple successful snapshot-task runs in a
// 24h window onto a single Kind=Snapshot parent row. Each snapshot
// child is linked via correlates_to so a feed that hides linked
// children will show one "Snapshot tasks completed today" row instead
// of a dozen near-identical job rows.
//
// The correlator uses the store as its source of truth — it doesn't
// track counts in memory, so a restart doesn't lose the threshold
// decision. Every snapshot success re-queries the 24h window; once
// the threshold is crossed the rollup is emitted and all prior
// unlinked siblings get back-linked.
type SnapshotRollup struct {
	store     *events.Store
	threshold int
	lookback  time.Duration
}

// NewSnapshotRollup builds a correlator with plan-recommended defaults.
func NewSnapshotRollup(store *events.Store) *SnapshotRollup {
	return &SnapshotRollup{
		store:     store,
		threshold: DefaultSnapshotRollupThreshold,
		lookback:  DefaultSnapshotLookback,
	}
}

// Run blocks until ctx is cancelled.
func (s *SnapshotRollup) Run(ctx context.Context) {
	ch, cancel := s.store.Subscribe(64)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			s.handle(ctx, ev)
		}
	}
}

func (s *SnapshotRollup) handle(ctx context.Context, child events.Event) {
	if !IsSnapshotJobKey(child.DedupeKey) {
		return
	}
	matches, err := s.recentSnapshotMatches(ctx, child.OccurredAt)
	if err != nil {
		slog.Warn("snapshot correlator: list", "err", err)
		return
	}
	if len(matches) < s.threshold {
		return
	}
	parent := s.buildRollup(child, len(matches))
	stored, err := s.store.Ingest(ctx, parent, s.lookback+2*time.Hour)
	if err != nil {
		slog.Warn("snapshot correlator: ingest", "err", err)
		return
	}
	// Back-link every matching child that isn't already linked. The
	// list call above returned them newest-first, but order doesn't
	// matter here — SetCorrelation is idempotent per row.
	for _, m := range matches {
		if m.CorrelatesTo != nil {
			continue
		}
		if err := s.store.SetCorrelation(ctx, m.ID, stored.ID); err != nil {
			slog.Warn("snapshot correlator: link", "err", err, "child", m.ID)
		}
	}
}

func (s *SnapshotRollup) recentSnapshotMatches(ctx context.Context, now time.Time) ([]events.Event, error) {
	since := now.Add(-s.lookback)
	list, err := s.store.List(ctx, events.ListFilter{
		Kind:  events.KindJob,
		Since: since,
		Limit: 200,
	})
	if err != nil {
		return nil, err
	}
	out := list[:0]
	for _, e := range list {
		if IsSnapshotJobKey(e.DedupeKey) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *SnapshotRollup) buildRollup(child events.Event, count int) events.Event {
	dateBucket := child.OccurredAt.UTC().Format("2006-01-02")
	summary := fmt.Sprintf("%d snapshot task runs succeeded in the last 24 hours", count)
	return events.Event{
		OccurredAt: child.OccurredAt,
		Kind:       events.KindSnapshot,
		Severity:   events.SeverityInfo,
		Title:      "Snapshot tasks completed today",
		Summary:    summary,
		Body:       child.Body,
		DedupeKey:  "snapshot-rollup|" + dateBucket,
	}
}

// IsSnapshotJobKey matches the job poller's dedupe key format
// (`job|<method>|<state>`) for methods that represent scheduled
// snapshot runs. Exported so tests can exercise the classification
// without constructing full events.
func IsSnapshotJobKey(key string) bool {
	parts := strings.Split(key, "|")
	if len(parts) != 3 || parts[0] != "job" || parts[2] != "success" {
		return false
	}
	method := parts[1]
	// `pool.snapshottask.run` is the headline method. `zettarepl.*`
	// shows up on servers that use the modern replication engine for
	// auto-snapshot orchestration. Other *.snapshot_create variants
	// are too rare to match without risking false positives.
	return strings.HasPrefix(method, "pool.snapshottask.") ||
		strings.HasPrefix(method, "zettarepl.")
}
