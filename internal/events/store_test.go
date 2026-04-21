package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func baseEvent() Event {
	return Event{
		// Store persists unix millis; truncate here so test comparisons are
		// lossless across the roundtrip.
		OccurredAt: time.Now().UTC().Truncate(time.Millisecond),
		Kind:       KindAlert,
		Severity:   SeverityWarning,
		Title:      "Pool tank is degraded",
		Summary:    "One or more devices are missing.",
		Body:       json.RawMessage(`{"klass":"ZfsDegraded"}`),
		DedupeKey:  "alert|warning|ZfsDegraded|tank",
	}
}

func TestIngestFresh(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	in := baseEvent()
	out, err := s.Ingest(ctx, in, time.Hour)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if out.ID == "" {
		t.Fatal("expected id assigned")
	}
	if out.Count != 1 {
		t.Errorf("count = %d, want 1", out.Count)
	}

	got, err := s.Get(ctx, out.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != in.Title {
		t.Errorf("title = %q, want %q", got.Title, in.Title)
	}
}

func TestIngestMergesWithinWindow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	first, err := s.Ingest(ctx, baseEvent(), time.Hour)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	dup := baseEvent()
	dup.OccurredAt = first.OccurredAt.Add(5 * time.Minute)
	merged, err := s.Ingest(ctx, dup, time.Hour)
	if err != nil {
		t.Fatalf("merged ingest: %v", err)
	}

	if merged.ID != first.ID {
		t.Errorf("merged id = %s, want same as first %s", merged.ID, first.ID)
	}
	if merged.Count != 2 {
		t.Errorf("merged count = %d, want 2", merged.Count)
	}
	if !merged.LastSeen.Equal(dup.OccurredAt) {
		t.Errorf("last_seen = %v, want %v", merged.LastSeen, dup.OccurredAt)
	}
	if !merged.FirstSeen.Equal(first.OccurredAt) {
		t.Errorf("first_seen mutated: got %v, want %v", merged.FirstSeen, first.OccurredAt)
	}
}

func TestIngestOutsideWindowCreatesNewRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	first, err := s.Ingest(ctx, baseEvent(), time.Hour)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	late := baseEvent()
	late.OccurredAt = first.OccurredAt.Add(90 * time.Minute)
	second, err := s.Ingest(ctx, late, time.Hour)
	if err != nil {
		t.Fatalf("late ingest: %v", err)
	}

	if second.ID == first.ID {
		t.Error("expected new row when outside merge window, got same id")
	}
}

func TestListFilters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()

	crit := baseEvent()
	crit.Severity = SeverityCritical
	crit.DedupeKey = "alert|critical|ZfsDegraded|tank"
	crit.OccurredAt = now
	if _, err := s.Ingest(ctx, crit, time.Hour); err != nil {
		t.Fatal(err)
	}

	warn := baseEvent()
	warn.DedupeKey = "alert|warning|ZfsDegraded|tank"
	warn.OccurredAt = now.Add(-time.Minute)
	if _, err := s.Ingest(ctx, warn, time.Hour); err != nil {
		t.Fatal(err)
	}

	// Severity filter
	gotCrit, err := s.List(ctx, ListFilter{Severity: SeverityCritical})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotCrit) != 1 || gotCrit[0].Severity != SeverityCritical {
		t.Errorf("severity filter: got %+v", gotCrit)
	}

	// No filter returns newest first
	all, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2, got %d", len(all))
	}
	if all[0].Severity != SeverityCritical {
		t.Errorf("newest-first: got %s first", all[0].Severity)
	}

	// Since filter excludes older event
	since, err := s.List(ctx, ListFilter{Since: now.Add(-30 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(since) != 1 {
		t.Errorf("since filter: expected 1, got %d", len(since))
	}
}

func TestAckAndPrune(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	old := baseEvent()
	old.OccurredAt = time.Now().Add(-60 * 24 * time.Hour)
	old.DedupeKey = "old"
	if _, err := s.Ingest(ctx, old, time.Hour); err != nil {
		t.Fatal(err)
	}

	fresh := baseEvent()
	fresh.DedupeKey = "fresh"
	freshOut, err := s.Ingest(ctx, fresh, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Ack(ctx, freshOut.ID, time.Now().UTC()); err != nil {
		t.Fatalf("ack: %v", err)
	}
	acked, err := s.Get(ctx, freshOut.ID)
	if err != nil {
		t.Fatal(err)
	}
	if acked.AckedAt == nil {
		t.Error("expected acked_at populated")
	}

	deleted, err := s.Prune(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("pruned %d, want 1", deleted)
	}
}

func TestAckUnknownReturnsNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.Ack(ctx, "nonexistent", time.Now())
	if err != ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestSubscribeReceivesIngest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ch, cancel := s.Subscribe(4)
	defer cancel()

	go func() {
		_, _ = s.Ingest(ctx, baseEvent(), time.Hour)
	}()

	select {
	case got := <-ch:
		if got.Title == "" {
			t.Error("subscriber received empty event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive event")
	}
}
