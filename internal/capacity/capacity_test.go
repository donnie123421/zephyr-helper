package capacity

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type stubSource struct {
	body string
	err  error
}

func (s stubSource) GetRaw(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(s.body), s.err
}

func openStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "capacity.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSampleRecordsPoolsWithSizes(t *testing.T) {
	store := openStore(t)
	rec := NewRecorder(stubSource{body: `[
		{"name": "tank", "size": 1000, "allocated": 400, "free": 600},
		{"name": "offline", "size": null, "allocated": null, "free": null},
		{"name": "empty", "size": 0, "allocated": 0, "free": 0}
	]`}, store, time.Hour)
	day := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return day }

	if err := rec.SampleOnce(context.Background()); err != nil {
		t.Fatalf("sample: %v", err)
	}

	got, err := store.History(context.Background(), "tank", day.AddDate(0, 0, -1))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(got) != 1 || got[0].Day != "2026-09-28" || got[0].Allocated != 400 || got[0].Size != 1000 {
		t.Fatalf("tank history = %+v", got)
	}
	for _, skipped := range []string{"offline", "empty"} {
		rows, err := store.History(context.Background(), skipped, day.AddDate(0, 0, -1))
		if err != nil {
			t.Fatalf("history %s: %v", skipped, err)
		}
		if len(rows) != 0 {
			t.Errorf("%s should not be recorded without a size, got %+v", skipped, rows)
		}
	}
}

func TestLaterSampleReplacesTheDay(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	morning := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)

	for i, allocated := range []int64{100, 150} {
		if err := store.Record(ctx, Sample{
			Day: dayOf(morning), Pool: "tank", Size: 1000, Allocated: allocated, Free: 1000 - allocated,
			SampledAt: morning.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := store.Record(ctx, Sample{
		Day: dayOf(morning.AddDate(0, 0, 1)), Pool: "tank", Size: 1000, Allocated: 200, Free: 800,
		SampledAt: morning.AddDate(0, 0, 1),
	}); err != nil {
		t.Fatalf("record next day: %v", err)
	}

	got, err := store.History(ctx, "tank", morning.AddDate(0, 0, -1))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want one row per day, got %+v", got)
	}
	if got[0].Allocated != 150 || got[1].Allocated != 200 {
		t.Errorf("rows = %+v; the day should keep its last reading, oldest first", got)
	}
}

func TestPruneDropsOldDays(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

	for _, age := range []int{500, 10} {
		day := now.AddDate(0, 0, -age)
		if err := store.Record(ctx, Sample{Day: dayOf(day), Pool: "tank", Size: 1, Allocated: 1, Free: 0, SampledAt: day}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	deleted, err := store.Prune(ctx, now.Add(-Retention))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 1 {
		t.Errorf("pruned %d rows, want 1", deleted)
	}
	left, err := store.History(ctx, "tank", now.AddDate(-5, 0, 0))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(left) != 1 {
		t.Errorf("rows left = %+v, want only the recent one", left)
	}
}

func TestSampleSurfacesSourceErrors(t *testing.T) {
	rec := NewRecorder(stubSource{err: errors.New("unreachable")}, openStore(t), time.Hour)
	if err := rec.SampleOnce(context.Background()); err == nil {
		t.Fatal("expected the source error to be returned")
	}
}
