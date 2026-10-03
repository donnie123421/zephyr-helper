package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A poller catching up hands the store older events after newer ones.
func TestALateOlderEventMergesOnlyNearbyAndKeepsTheNewest(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	at := time.Date(2026, 10, 3, 2, 0, 0, 0, time.UTC)
	ev := func(when time.Time, run string) Event {
		body, _ := json.Marshal(map[string]string{"run": run})
		return Event{OccurredAt: when, Kind: KindJob, Severity: SeverityInfo, Title: "replication.run succeeded", Body: body, DedupeKey: "job|replication.run|success"}
	}
	newest, _ := s.Ingest(ctx, ev(at, "newest"), 5*time.Minute)
	// A day older: its own row, not merged into the newer one.
	if older, _ := s.Ingest(ctx, ev(at.AddDate(0, 0, -1), "yesterday"), 5*time.Minute); older.ID == newest.ID {
		t.Error("a day-old event merged into today's row")
	}
	// Two minutes older: merges, but the row keeps the newest time and payload.
	merged, _ := s.Ingest(ctx, ev(at.Add(-2*time.Minute), "a little earlier"), 5*time.Minute)
	got, _ := s.Get(ctx, newest.ID)
	if merged.ID != newest.ID || got.Count != 2 || !got.LastSeen.Equal(at) || string(got.Body) != `{"run":"newest"}` {
		t.Errorf("row = %+v body %s", got, got.Body)
	}
}
