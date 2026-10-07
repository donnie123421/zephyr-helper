package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// While Zephyr Pro has lapsed nothing booked runs, nor is anything missed;
// when it's back each carries on from its next time.
func TestALapseHoldsWhatsBookedAndItsReturnCarriesOn(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	weekly := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Start a scrub on tank every Sunday at 2:00 am",
		When: When{Repeat: "weekly", Weekday: "sunday", Time: "02:00"}, NextRun: at(r.now.Add(-time.Minute))})
	once := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Start a scrub on tank at 3:30 pm on Thu 1 Oct",
		When: When{Repeat: "once", Date: "2026-10-01", Time: "15:30"}, NextRun: at(r.now.Add(time.Hour))})
	byHand := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "paused by hand", Paused: true,
		When: When{Repeat: "daily", Time: "14:00"}, NextRun: at(r.now.Add(-30 * time.Minute))})

	lapsed := true
	r.sched.Held = func() bool { return lapsed }
	if n, err := r.store.Hold(ctx); err != nil || n != 2 {
		t.Fatalf("held %d, %v; want the two not paused", n, err)
	}
	r.sched.runDue(ctx)
	r.sched.Wait()
	if r.action.count() != 0 {
		t.Fatal("ran while Pro had lapsed")
	}
	// Booked meanwhile, it's held on the scheduler's next look.
	daily := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Start a scrub on tank every day at 2:00 pm",
		When: When{Repeat: "daily", Time: "14:00"}, NextRun: at(r.now.Add(-time.Minute))})
	r.sched.runDue(ctx)
	r.sched.Wait()
	if got, _ := r.store.Get(ctx, daily.ID); r.action.count() != 0 || !got.Paused || !got.Held {
		t.Fatalf("booked during the lapse: ran %d, %+v", r.action.count(), got)
	}
	if err := r.sched.RunNow(ctx, weekly.ID); err == nil {
		t.Error("Run Now ran while Pro had lapsed")
	}

	// Two days on, past the one-off's time, Pro is back.
	r.now = r.now.Add(48 * time.Hour)
	lapsed = false
	resumed, expired, err := r.store.Release(ctx, r.now)
	if err != nil || resumed != 2 || expired != 1 {
		t.Fatalf("released %d carrying on, %d done with, %v", resumed, expired, err)
	}
	if got, _ := r.store.Get(ctx, once.ID); got.NextRun != nil || got.Paused || got.Held || got.LastStatus != "skipped" ||
		got.LastMessage != passedWhileHeld {
		t.Errorf("the one-off whose time passed: %+v", got)
	}
	sydney, _ := time.LoadLocation("Australia/Sydney")
	if got, _ := r.store.Get(ctx, weekly.ID); got.Paused || got.Held || got.NextRun == nil ||
		!got.NextRun.Equal(time.Date(2026, 10, 4, 2, 0, 0, 0, sydney)) {
		t.Errorf("the weekly job: %+v", got)
	}
	if got, _ := r.store.Get(ctx, byHand.ID); !got.Paused || got.Held {
		t.Errorf("the one paused by hand: %+v", got)
	}

	// Nothing ran late, and nothing was noted as missed.
	r.sched.runDue(ctx)
	if evs := r.feed(t); r.action.count() != 0 || len(evs) != 0 {
		t.Errorf("after the lapse: ran %d, feed %+v", r.action.count(), evs)
	}
	r.now = time.Date(2026, 10, 4, 2, 0, 30, 0, sydney)
	r.sched.runDue(ctx)
	r.sched.Wait()
	if r.action.count() != 1 {
		t.Errorf("ran %d at 2 am on Sunday, want the weekly", r.action.count())
	}
	r.now = time.Date(2026, 10, 4, 14, 0, 30, 0, sydney)
	r.sched.runDue(ctx)
	r.sched.Wait()
	if r.action.count() != 2 {
		t.Errorf("ran %d by 2 pm on Sunday, want the daily too", r.action.count())
	}
}

// The request that says Pro is back may not get to the bookings; the
// scheduler's next look does.
func TestTheSchedulerCatchesUpWithAReturn(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	job := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Start a scrub on tank every day at 2:00 pm",
		When: When{Repeat: "daily", Time: "14:00"}, NextRun: at(r.now.Add(time.Hour))})
	if _, err := r.store.Hold(ctx); err != nil {
		t.Fatal(err)
	}
	r.sched.runDue(ctx)
	if got, _ := r.store.Get(ctx, job.ID); got.Paused || got.Held {
		t.Errorf("still held: %+v", got)
	}
}

// A pause made by hand during the lapse isn't the lapse's to lift.
func TestAPauseByHandOutlastsTheLapse(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	job := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Start a scrub on tank every day at 2:00 pm",
		When: When{Repeat: "daily", Time: "14:00"}, NextRun: at(r.now.Add(time.Hour))})
	_, _ = r.store.Hold(ctx)
	if err := r.store.Pause(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if resumed, _, _ := r.store.Release(ctx, r.now); resumed != 0 {
		t.Errorf("released %d", resumed)
	}
	if got, _ := r.store.Get(ctx, job.ID); !got.Paused || got.Held {
		t.Errorf("job = %+v", got)
	}
}

func TestTheScheduledScreenSeesTheHold(t *testing.T) {
	r := newRig(t)
	r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Start a scrub on tank every day at 2:00 pm",
		When: When{Repeat: "daily", Time: "14:00"}, NextRun: at(r.now.Add(time.Hour))})
	_, _ = r.store.Hold(context.Background())
	mux := http.NewServeMux()
	(&Handler{Store: r.store, Scheduler: r.sched}).RegisterMux(mux, func(h http.Handler) http.Handler { return h })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/schedules", nil))
	var body struct {
		Schedules []map[string]any `json:"schedules"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if len(body.Schedules) != 1 || body.Schedules[0]["paused"] != true || body.Schedules[0]["held"] != true {
		t.Errorf("list = %d %+v", rec.Code, body)
	}
}

// A schedule file from before the hold gains its column, unpaused.
func TestAnOlderScheduleFileGainsTheHold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`
CREATE TABLE jobs (
    id TEXT PRIMARY KEY, kind TEXT NOT NULL, tool TEXT NOT NULL DEFAULT '', arguments TEXT NOT NULL DEFAULT '{}',
    title TEXT NOT NULL, detail TEXT NOT NULL, risk TEXT NOT NULL, when_json TEXT NOT NULL, created_at INTEGER NOT NULL,
    next_run INTEGER, paused INTEGER NOT NULL DEFAULT 0, last_run INTEGER,
    last_status TEXT NOT NULL DEFAULT '', last_message TEXT NOT NULL DEFAULT '');
INSERT INTO jobs (id, kind, title, detail, risk, when_json, created_at, next_run)
VALUES ('j1', 'action', 'Scrub', '', 'routine', '{"repeat":"daily","time":"14:00","zone":"UTC"}', 1, 2);`)
	_ = old.Close()
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if n, err := store.Hold(context.Background()); err != nil || n != 1 {
		t.Fatalf("held %d, %v", n, err)
	}
	if got, _ := store.Get(context.Background(), "j1"); !got.Held || !got.Paused {
		t.Errorf("job = %+v", got)
	}
	// Opening it again leaves the column be.
	_ = store.Close()
	if again, err := Open(path); err != nil {
		t.Errorf("reopen: %v", err)
	} else {
		_ = again.Close()
	}
}
