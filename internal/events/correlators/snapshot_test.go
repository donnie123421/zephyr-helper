package correlators

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

func TestIsSnapshotJobKey(t *testing.T) {
	cases := map[string]bool{
		"job|pool.snapshottask.run|success":  true,
		"job|pool.snapshottask.run|failed":   false,
		"job|zettarepl.snapshot_created|success": true,
		"job|pool.scrub|success":             false,
		"job|replication.run|success":        false,
		"alert|warning|ZfsDegraded|Pool tank": false,
		"malformed":                          false,
	}
	for k, want := range cases {
		if got := IsSnapshotJobKey(k); got != want {
			t.Errorf("IsSnapshotJobKey(%q) = %v, want %v", k, got, want)
		}
	}
}

// ingestSnapshotJob writes a snapshot-task success row with the
// canonical dedupe key. Passing mergeWindow=0 keeps each call a
// distinct row rather than collapsing on dedupe — we want the
// correlator to see three separate children.
func ingestSnapshotJob(t *testing.T, ctx context.Context, store *events.Store, occurred time.Time, idx int) events.Event {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"id":     float64(idx),
		"method": "pool.snapshottask.run",
		"state":  "SUCCESS",
	})
	e, err := store.Ingest(ctx, events.Event{
		OccurredAt: occurred,
		Kind:       events.KindJob,
		Severity:   events.SeverityInfo,
		Title:      "pool.snapshottask.run succeeded",
		Summary:    "pool.snapshottask.run succeeded",
		Body:       body,
		DedupeKey:  "job|pool.snapshottask.run|success",
	}, 0)
	if err != nil {
		t.Fatalf("ingest %d: %v", idx, err)
	}
	return e
}

func TestSnapshotRollupBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	store, err := events.Open(dir + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	// 2 snapshot successes — below the default threshold of 3.
	_ = ingestSnapshotJob(t, ctx, store, now.Add(-time.Hour), 1)
	child2 := ingestSnapshotJob(t, ctx, store, now, 2)

	s := NewSnapshotRollup(store)
	s.handle(ctx, child2)

	list, err := store.List(ctx, events.ListFilter{Kind: events.KindSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("expected no rollup below threshold, got %d", len(list))
	}
}

func TestSnapshotRollupCrossesThresholdAndBackLinks(t *testing.T) {
	dir := t.TempDir()
	store, err := events.Open(dir + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	// Three snapshot successes within 24h, each at a distinct
	// occurred_at with mergeWindow=0 so they land as three separate
	// rows despite sharing the canonical dedupe key.
	c1 := ingestSnapshotJob(t, ctx, store, now.Add(-3*time.Hour), 1)
	c2 := ingestSnapshotJob(t, ctx, store, now.Add(-90*time.Minute), 2)
	c3 := ingestSnapshotJob(t, ctx, store, now, 3)

	s := NewSnapshotRollup(store)
	s.handle(ctx, c3)

	list, err := store.List(ctx, events.ListFilter{Kind: events.KindSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 rollup, got %d", len(list))
	}
	parent := list[0]
	if parent.Title != "Snapshot tasks completed today" {
		t.Errorf("title = %q", parent.Title)
	}
	if parent.Summary != "3 snapshot task runs succeeded in the last 24 hours" {
		t.Errorf("summary = %q", parent.Summary)
	}

	// All three children should now correlate to the parent.
	for _, id := range []string{c1.ID, c2.ID, c3.ID} {
		e, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if e.CorrelatesTo == nil || *e.CorrelatesTo != parent.ID {
			t.Errorf("child %s correlates_to = %v, want %s", id, e.CorrelatesTo, parent.ID)
		}
	}
}

func TestSnapshotRollupIgnoresNonSnapshotJob(t *testing.T) {
	dir := t.TempDir()
	store, err := events.Open(dir + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := NewSnapshotRollup(store)
	child := events.Event{
		Kind:      events.KindJob,
		DedupeKey: "job|pool.scrub|success",
	}
	s.handle(context.Background(), child)

	list, _ := store.List(context.Background(), events.ListFilter{Kind: events.KindSnapshot})
	if len(list) != 0 {
		t.Errorf("expected no rollup for non-snapshot job, got %d", len(list))
	}
}
