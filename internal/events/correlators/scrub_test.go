package correlators

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

func makeJobEvent(t *testing.T, body map[string]any) events.Event {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return events.Event{
		ID:         "child-id",
		OccurredAt: time.Unix(1714000000, 0).UTC(),
		Kind:       events.KindJob,
		Severity:   events.SeverityInfo,
		Title:      "pool.scrub succeeded",
		Body:       raw,
		DedupeKey:  "job|pool.scrub|success",
	}
}

func TestScrubParentForSuccess(t *testing.T) {
	child := makeJobEvent(t, map[string]any{
		"id":             float64(1),
		"method":         "pool.scrub",
		"state":          "SUCCESS",
		"arguments":      []any{"tank", "START"},
		"time_started":   map[string]any{"$date": float64(1713990000000)},
		"time_finished":  map[string]any{"$date": float64(1714000000000)},
	})
	parent, ok := ScrubParentFor(child)
	if !ok {
		t.Fatal("expected parent")
	}
	if parent.Kind != events.KindScrub {
		t.Errorf("kind = %s, want scrub", parent.Kind)
	}
	if parent.Severity != events.SeverityInfo {
		t.Errorf("severity = %s, want info", parent.Severity)
	}
	if parent.Title != "Scrub of tank completed" {
		t.Errorf("title = %q", parent.Title)
	}
	if parent.Summary != "Scrub of tank completed in 2h46m" {
		t.Errorf("summary = %q", parent.Summary)
	}
	if parent.DedupeKey != "scrub|tank|success" {
		t.Errorf("dedupe = %q", parent.DedupeKey)
	}
	if !parent.OccurredAt.Equal(child.OccurredAt) {
		t.Error("occurred_at should mirror child")
	}
}

func TestScrubParentForFailure(t *testing.T) {
	child := makeJobEvent(t, map[string]any{
		"method":    "pool.scrub",
		"state":     "FAILED",
		"arguments": []any{"tank"},
	})
	parent, ok := ScrubParentFor(child)
	if !ok {
		t.Fatal("expected parent")
	}
	if parent.Severity != events.SeverityWarning {
		t.Errorf("severity = %s, want warning", parent.Severity)
	}
	if parent.Title != "Scrub of tank failed" {
		t.Errorf("title = %q", parent.Title)
	}
}

func TestScrubParentForAbortedNoPool(t *testing.T) {
	child := makeJobEvent(t, map[string]any{
		"method": "pool.scrub",
		"state":  "ABORTED",
	})
	parent, ok := ScrubParentFor(child)
	if !ok {
		t.Fatal("expected parent even without pool name")
	}
	if parent.Title != "Scrub aborted" {
		t.Errorf("title = %q", parent.Title)
	}
	if parent.DedupeKey != "scrub||aborted" {
		t.Errorf("dedupe = %q", parent.DedupeKey)
	}
}

func TestScrubParentForNumericArg(t *testing.T) {
	// Older TrueNAS passes the pool id as a number. We stringify so
	// the dedupe key stays stable.
	child := makeJobEvent(t, map[string]any{
		"method":    "pool.scrub",
		"state":     "SUCCESS",
		"arguments": []any{float64(1)},
	})
	parent, ok := ScrubParentFor(child)
	if !ok {
		t.Fatal("expected parent")
	}
	if parent.Title != "Scrub of 1 completed" {
		t.Errorf("title = %q", parent.Title)
	}
	if parent.DedupeKey != "scrub|1|success" {
		t.Errorf("dedupe = %q", parent.DedupeKey)
	}
}

func TestScrubParentForSkipsNonScrub(t *testing.T) {
	child := makeJobEvent(t, map[string]any{
		"method": "replication.run",
		"state":  "SUCCESS",
	})
	if _, ok := ScrubParentFor(child); ok {
		t.Error("expected skip for non-scrub method")
	}
}

func TestScrubParentForSkipsNonJobKind(t *testing.T) {
	child := events.Event{
		Kind: events.KindAlert,
		Body: json.RawMessage(`{"method":"pool.scrub","state":"SUCCESS"}`),
	}
	if _, ok := ScrubParentFor(child); ok {
		t.Error("expected skip for non-job kind")
	}
}

func TestScrubParentForSkipsNonTerminal(t *testing.T) {
	child := makeJobEvent(t, map[string]any{
		"method": "pool.scrub",
		"state":  "RUNNING",
	})
	if _, ok := ScrubParentFor(child); ok {
		t.Error("expected skip for non-terminal state")
	}
}

func TestScrubParentForSkipsWhenBodyEmpty(t *testing.T) {
	child := events.Event{Kind: events.KindJob}
	if _, ok := ScrubParentFor(child); ok {
		t.Error("expected skip for empty body")
	}
}

func TestScrubParentForSkipsWhenBodyInvalid(t *testing.T) {
	child := events.Event{
		Kind: events.KindJob,
		Body: json.RawMessage(`not json`),
	}
	if _, ok := ScrubParentFor(child); ok {
		t.Error("expected skip for invalid body")
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{3 * time.Second, "3s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m00s"},
		{4*time.Minute + 12*time.Second, "4m12s"},
		{time.Hour, "1h00m"},
		{2*time.Hour + 15*time.Minute + 7*time.Second, "2h15m"},
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRunEmitsParentAndLinksChild(t *testing.T) {
	// Use a tempfile rather than :memory: — the correlator runs on a
	// separate goroutine and SQLite gives each connection its own
	// :memory: database, so the correlator would read an empty DB.
	dir := t.TempDir()
	store, err := events.Open(dir + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	correlator := NewScrubOutcome(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		correlator.Run(ctx)
		close(done)
	}()

	// Ingest a pool.scrub job terminal. The correlator should pick it
	// up via Subscribe, emit a Kind=Scrub parent, and set
	// correlates_to on the child.
	body, _ := json.Marshal(map[string]any{
		"id":            float64(1),
		"method":        "pool.scrub",
		"state":         "SUCCESS",
		"arguments":     []any{"tank"},
		"time_started":  map[string]any{"$date": float64(1713999880000)},
		"time_finished": map[string]any{"$date": float64(1714000000000)},
	})
	child, err := store.Ingest(ctx, events.Event{
		OccurredAt: time.Unix(1714000000, 0).UTC(),
		Kind:       events.KindJob,
		Severity:   events.SeverityInfo,
		Title:      "pool.scrub succeeded",
		Summary:    "pool.scrub succeeded",
		Body:       body,
		DedupeKey:  "job|pool.scrub|success",
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// The correlator runs async; poll briefly for the parent row.
	var parent events.Event
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, err := store.List(ctx, events.ListFilter{Kind: events.KindScrub})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 {
			parent = list[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if parent.ID == "" {
		t.Fatal("correlator did not emit scrub parent")
	}
	if parent.Title != "Scrub of tank completed" {
		t.Errorf("parent title = %q", parent.Title)
	}

	// Child should now point at parent via correlates_to.
	refreshed, err := store.Get(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.CorrelatesTo == nil || *refreshed.CorrelatesTo != parent.ID {
		t.Errorf("child correlates_to = %v, want %s", refreshed.CorrelatesTo, parent.ID)
	}

	cancel()
	<-done
}
