// Package correlators turns raw ingested events into narrative parent
// rows. A correlator subscribes to the Store, watches for patterns
// across child events, and emits a richer summary row that child events
// reference via correlates_to. The feed can then show just the parent
// and hide the noisy children as a detail-view expansion.
package correlators

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

// DefaultScrubMergeWindow dedupes back-to-back emissions of the same
// pool+state. Scrubs are long — minutes to hours — so the window only
// needs to cover clock skew between finish and ingest, not retry storms.
const DefaultScrubMergeWindow = 5 * time.Minute

// ScrubOutcome watches the event stream for pool.scrub job terminations
// and emits a richer Kind=Scrub parent row with pool name and duration.
// The original Kind=Job row is linked via correlates_to so the default
// feed can show just the scrub narrative and keep the underlying job as
// a detail-view child.
type ScrubOutcome struct {
	store *events.Store
	merge time.Duration
}

// NewScrubOutcome builds a correlator using DefaultScrubMergeWindow.
func NewScrubOutcome(store *events.Store) *ScrubOutcome {
	return &ScrubOutcome{store: store, merge: DefaultScrubMergeWindow}
}

// Run blocks until ctx is cancelled, processing every event the store
// broadcasts. The store's non-blocking broadcast means a stalled
// correlator drops frames; that's fine — scrub events are rare enough
// that any drop would be noticed via the underlying Kind=Job row.
func (s *ScrubOutcome) Run(ctx context.Context) {
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

func (s *ScrubOutcome) handle(ctx context.Context, child events.Event) {
	parent, ok := ScrubParentFor(child)
	if !ok {
		return
	}
	stored, err := s.store.Ingest(ctx, parent, s.merge)
	if err != nil {
		slog.Warn("scrub correlator: ingest", "err", err)
		return
	}
	if err := s.store.SetCorrelation(ctx, child.ID, stored.ID); err != nil {
		slog.Warn("scrub correlator: link", "err", err, "child", child.ID, "parent", stored.ID)
	}
}

// ScrubParentFor inspects a child event and, if it looks like a
// pool.scrub job terminal, returns the scrub parent event to ingest.
// Exported so the transform can be tested without a live store.
func ScrubParentFor(child events.Event) (events.Event, bool) {
	if child.Kind != events.KindJob || len(child.Body) == 0 {
		return events.Event{}, false
	}
	var raw map[string]any
	if err := json.Unmarshal(child.Body, &raw); err != nil {
		return events.Event{}, false
	}
	method, _ := raw["method"].(string)
	if method != "pool.scrub" {
		return events.Event{}, false
	}
	state, _ := raw["state"].(string)
	upper := strings.ToUpper(state)
	if !isTerminalScrubState(upper) {
		return events.Event{}, false
	}
	pool := extractPoolArgument(raw)
	duration := extractScrubDuration(raw)
	return events.Event{
		OccurredAt: child.OccurredAt,
		Kind:       events.KindScrub,
		Severity:   scrubSeverity(upper),
		Title:      scrubTitle(pool, upper),
		Summary:    scrubSummary(pool, upper, duration),
		Body:       child.Body,
		DedupeKey:  scrubDedupeKey(pool, upper),
	}, true
}

func isTerminalScrubState(state string) bool {
	switch state {
	case "SUCCESS", "FAILED", "ABORTED":
		return true
	}
	return false
}

// extractPoolArgument pulls the pool handle out of the job's arguments.
// Newer TrueNAS passes the pool name as the first argument; older
// versions pass a numeric id. Either is unique within a deployment and
// dedupe only needs a stable handle, not a resolved name.
func extractPoolArgument(raw map[string]any) string {
	args, _ := raw["arguments"].([]any)
	if len(args) == 0 {
		return ""
	}
	switch v := args[0].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%d", int64(v))
	}
	return ""
}

// extractScrubDuration returns finished-minus-started. Returns zero
// when either timestamp is missing; callers omit duration from the
// summary in that case rather than emitting "0s".
func extractScrubDuration(raw map[string]any) time.Duration {
	started := parseDateField(raw["time_started"])
	finished := parseDateField(raw["time_finished"])
	if started.IsZero() || finished.IsZero() || !finished.After(started) {
		return 0
	}
	return finished.Sub(started)
}

// parseDateField mirrors pollers.parseTrueNASTime. Duplicated here
// because importing `internal/pollers` would create a cycle (pollers
// will eventually need to live alongside correlators).
func parseDateField(v any) time.Time {
	switch x := v.(type) {
	case map[string]any:
		if ms, ok := x["$date"].(float64); ok {
			return time.UnixMilli(int64(ms)).UTC()
		}
	case string:
		if x == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func scrubSeverity(state string) events.Severity {
	switch state {
	case "FAILED", "ABORTED":
		return events.SeverityWarning
	}
	return events.SeverityInfo
}

func scrubTitle(pool, state string) string {
	subject := "Scrub"
	if pool != "" {
		subject = "Scrub of " + pool
	}
	switch state {
	case "SUCCESS":
		return subject + " completed"
	case "FAILED":
		return subject + " failed"
	case "ABORTED":
		return subject + " aborted"
	}
	return subject + " " + strings.ToLower(state)
}

func scrubSummary(pool, state string, duration time.Duration) string {
	base := scrubTitle(pool, state)
	if duration == 0 {
		return base
	}
	return fmt.Sprintf("%s in %s", base, formatDuration(duration))
}

// formatDuration renders durations compactly for feed summaries.
// Examples: "3s", "4m12s", "2h15m". Seconds get stripped above an hour
// because an imprecise "2h15m" is more readable than "2h15m07s".
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	return fmt.Sprintf("%dh%02dm", h, m)
}

func scrubDedupeKey(pool, state string) string {
	return "scrub|" + pool + "|" + strings.ToLower(state)
}
