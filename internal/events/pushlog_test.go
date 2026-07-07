package events

import (
	"testing"
	"time"
)

func TestPushLogRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	now := time.Now().UTC()

	if err := s.RecordPush(ctx, "ev-1", now.Add(-time.Hour)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.RecordPush(ctx, "ev-2", now.Add(-30*time.Hour)); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Re-record upserts rather than erroring.
	if err := s.RecordPush(ctx, "ev-1", now); err != nil {
		t.Fatalf("re-record: %v", err)
	}

	recent, err := s.RecentPushes(ctx, now.Add(-6*time.Hour))
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if _, ok := recent["ev-1"]; !ok {
		t.Error("ev-1 pushed within window should be returned")
	}
	if _, ok := recent["ev-2"]; ok {
		t.Error("ev-2 outside window should be excluded")
	}

	// Prune clears old push_log rows on the same cutoff as events.
	if _, err := s.Prune(ctx, now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	all, err := s.RecentPushes(ctx, now.Add(-100*time.Hour))
	if err != nil {
		t.Fatalf("recent after prune: %v", err)
	}
	if _, ok := all["ev-2"]; ok {
		t.Error("prune should drop push_log rows older than cutoff")
	}
	if _, ok := all["ev-1"]; !ok {
		t.Error("prune must keep push_log rows newer than cutoff")
	}
}
