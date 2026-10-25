package pollers

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

type scriptedPools struct {
	mu    sync.Mutex
	pools []map[string]any
	// jobs answers core.get_jobs; jobsErr fails it.
	jobs    []map[string]any
	jobsErr error
	methods []string
}

func (s *scriptedPools) set(pools ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pools = pools
}

func (s *scriptedPools) setJobs(jobs ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = jobs
}

func (s *scriptedPools) Call(_ context.Context, method string, _ ...any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods = append(s.methods, method)
	if method == "core.get_jobs" {
		if s.jobsErr != nil {
			return nil, s.jobsErr
		}
		return json.Marshal(s.jobs)
	}
	return json.Marshal(s.pools)
}

func pool(name, function, state string, ended time.Time, errors int) map[string]any {
	scan := map[string]any{"function": function, "state": state, "errors": errors, "end_time": nil}
	if !ended.IsZero() {
		scan["end_time"] = map[string]any{"$date": ended.UnixMilli()}
	}
	return map[string]any{"name": name, "scan": scan}
}

// scrubJob is a scrub job as core.get_jobs lists it; a zero finished
// means it's still running.
func scrubJob(id int64, method string, arg any, started, finished time.Time) map[string]any {
	job := map[string]any{"id": id, "method": method, "arguments": []any{arg},
		"time_started": map[string]any{"$date": started.UnixMilli()}, "time_finished": nil}
	if !finished.IsZero() {
		job["time_finished"] = map[string]any{"$date": finished.UnixMilli()}
	}
	return job
}

func watcher(t *testing.T) (*Scans, *scriptedPools, *events.Store) {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tn := &scriptedPools{}
	return NewScans(tn, store, time.Minute), tn, store
}

func feed(t *testing.T, store *events.Store) []events.Event {
	t.Helper()
	evs, err := store.List(context.Background(), events.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return evs
}

func titles(evs []events.Event) map[string]bool {
	out := map[string]bool{}
	for _, ev := range evs {
		out[ev.Title] = true
	}
	return out
}

// A resilver seen running, then finished, is noted once.
func TestAResilverFinishingIsNotedOnce(t *testing.T) {
	r, tn, store := watcher(t)
	ctx := context.Background()
	tn.set(pool("backup", "RESILVER", "SCANNING", time.Time{}, 0))
	r.poll(ctx)
	if len(feed(t, store)) != 0 {
		t.Fatal("noted a resilver still running")
	}
	// Long ago, as far as the clock goes: it was seen running.
	ended := time.Now().Add(-3 * time.Hour)
	tn.set(pool("backup", "RESILVER", "FINISHED", ended, 0))
	r.poll(ctx)
	r.poll(ctx)
	evs := feed(t, store)
	if len(evs) != 1 || evs[0].Title != "Resilver of backup finished" || evs[0].Severity != events.SeverityInfo ||
		!strings.HasPrefix(evs[0].DedupeKey, events.RebuiltKeyPrefix+"backup|") {
		t.Errorf("feed = %+v", evs)
	}
}

// One that finished long before the helper started isn't news; one that
// finished while it restarted is, a scrub as much as a resilver.
func TestOnlyARecentFinishIsNewsAtStart(t *testing.T) {
	r, tn, store := watcher(t)
	tn.set(pool("tank", "RESILVER", "FINISHED", time.Now().Add(-48*time.Hour), 0),
		pool("backup", "RESILVER", "FINISHED", time.Now().Add(-5*time.Minute), 0),
		pool("scratch", "SCRUB", "FINISHED", time.Now().Add(-time.Minute), 0),
		pool("media", "SCRUB", "FINISHED", time.Now().Add(-9*24*time.Hour), 0))
	r.poll(context.Background())
	evs := feed(t, store)
	got := titles(evs)
	if len(evs) != 2 || !got["Resilver of backup finished"] || !got["Scrub of scratch finished"] {
		t.Errorf("feed = %+v", evs)
	}
}

// A second rebuild of the same pool is news too: its key has its own end.
func TestASecondRebuildIsNotedToo(t *testing.T) {
	r, tn, store := watcher(t)
	ctx := context.Background()
	first := time.Now().Add(-2 * time.Minute)
	tn.set(pool("backup", "RESILVER", "FINISHED", first, 0))
	r.poll(ctx)
	tn.set(pool("backup", "RESILVER", "SCANNING", time.Time{}, 0))
	r.poll(ctx)
	tn.set(pool("backup", "RESILVER", "FINISHED", time.Now(), 3))
	r.poll(ctx)
	evs := feed(t, store)
	if len(evs) != 2 {
		t.Fatalf("feed = %+v", evs)
	}
	var withErrors *events.Event
	for i := range evs {
		if evs[i].Severity == events.SeverityWarning {
			withErrors = &evs[i]
		}
	}
	if withErrors == nil || !strings.Contains(withErrors.Summary, "with 3 errors") {
		t.Errorf("feed = %+v", evs)
	}
}

// A scrub, scheduled or started from the app, is noted once when it
// finishes. A scheduled run that wasn't due leaves the scan as it was,
// so it notes nothing.
func TestAScrubFinishingIsNotedOnce(t *testing.T) {
	r, tn, store := watcher(t)
	ctx := context.Background()
	last := time.Now().Add(-20 * 24 * time.Hour)
	tn.set(pool("tank", "SCRUB", "FINISHED", last, 0))
	r.poll(ctx)
	r.poll(ctx)
	if evs := feed(t, store); len(evs) != 0 {
		t.Fatalf("noted the last scrub again: %+v", evs)
	}
	tn.set(pool("tank", "SCRUB", "SCANNING", last, 0))
	r.poll(ctx)
	ended := time.Now().Add(-2 * time.Hour)
	tn.set(pool("tank", "SCRUB", "FINISHED", ended, 0))
	r.poll(ctx)
	r.poll(ctx)
	evs := feed(t, store)
	if len(evs) != 1 || evs[0].Kind != events.KindScrub || evs[0].Severity != events.SeverityInfo ||
		evs[0].Title != "Scrub of tank finished" || evs[0].Summary != "The scrub found no errors on tank." ||
		evs[0].OccurredAt.Sub(ended).Abs() > time.Second {
		t.Errorf("feed = %+v", evs)
	}
}

// A scrub that found errors is a warning, which notifies, and says how
// many it found.
func TestAScrubThatFoundErrorsWarns(t *testing.T) {
	r, tn, store := watcher(t)
	tn.set(pool("tank", "SCRUB", "FINISHED", time.Now().Add(-time.Minute), 1),
		pool("backup", "SCRUB", "FINISHED", time.Now().Add(-time.Minute), 4))
	r.poll(context.Background())
	evs := feed(t, store)
	got := titles(evs)
	if len(evs) != 2 || !got["Scrub of tank found 1 error"] || !got["Scrub of backup found 4 errors"] {
		t.Fatalf("feed = %+v", evs)
	}
	for _, ev := range evs {
		if ev.Severity != events.SeverityWarning || ev.Kind != events.KindScrub {
			t.Errorf("event = %+v", ev)
		}
	}
}

// A pool's scrub and resilver are told apart: one finishing doesn't stop
// the other being noted.
func TestAPoolsScrubAndResilverAreKeptApart(t *testing.T) {
	r, tn, store := watcher(t)
	ctx := context.Background()
	tn.set(pool("tank", "SCRUB", "SCANNING", time.Time{}, 0))
	r.poll(ctx)
	tn.set(pool("tank", "RESILVER", "SCANNING", time.Time{}, 0))
	r.poll(ctx)
	ended := time.Now().Add(-4 * time.Hour)
	tn.set(pool("tank", "RESILVER", "FINISHED", ended, 0))
	r.poll(ctx)
	tn.set(pool("tank", "SCRUB", "FINISHED", ended.Add(2*time.Hour), 0))
	r.poll(ctx)
	got := titles(feed(t, store))
	if len(got) != 2 || !got["Resilver of tank finished"] || !got["Scrub of tank finished"] {
		t.Errorf("feed = %+v", feed(t, store))
	}
}

// A scrub the assistant started has its own entry, from the job it
// followed, so it isn't noted twice. Another pool's still is.
func TestTheAssistantsScrubIsLeftToItsEntry(t *testing.T) {
	r, tn, store := watcher(t)
	followed := map[int64]bool{41: true}
	r.Skip = func(id int64) bool { return followed[id] }
	ended := time.Now().Add(-time.Minute)
	tn.setJobs(
		scrubJob(41, "zpool.scrub.run", map[string]any{"pool_name": "tank", "action": "START"}, ended.Add(-time.Hour), ended.Add(time.Second)),
		scrubJob(40, "pool.scrub.scrub", "backup", ended.Add(-2*time.Hour), ended.Add(time.Second)),
	)
	tn.set(pool("tank", "SCRUB", "FINISHED", ended, 0), pool("backup", "SCRUB", "FINISHED", ended, 0))
	r.poll(context.Background())
	evs := feed(t, store)
	if len(evs) != 1 || evs[0].Title != "Scrub of backup finished" {
		t.Errorf("feed = %+v", evs)
	}
}

// The assistant cancelling an earlier scrub doesn't hide the next one.
func TestAnEarlierAssistantJobDoesntHideAScrub(t *testing.T) {
	r, tn, store := watcher(t)
	r.Skip = func(id int64) bool { return id == 7 }
	ended := time.Now().Add(-time.Minute)
	tn.setJobs(scrubJob(7, "zpool.scrub.run", map[string]any{"pool_name": "tank", "action": "CANCEL"},
		ended.Add(-40*time.Minute), ended.Add(-40*time.Minute)))
	tn.set(pool("tank", "SCRUB", "FINISHED", ended, 0))
	r.poll(context.Background())
	if evs := feed(t, store); len(evs) != 1 || evs[0].Title != "Scrub of tank finished" {
		t.Errorf("feed = %+v", evs)
	}
}

// Jobs that can't be read leave the scrub noted: two entries beat none.
func TestAScrubIsNotedWhenItsJobsCantBeRead(t *testing.T) {
	r, tn, store := watcher(t)
	r.Skip = func(int64) bool { return true }
	tn.jobsErr = errors.New("connection reset")
	tn.set(pool("tank", "SCRUB", "FINISHED", time.Now().Add(-time.Minute), 0))
	r.poll(context.Background())
	if evs := feed(t, store); len(evs) != 1 {
		t.Errorf("feed = %+v", evs)
	}
}

// Without a Skip, nothing asks TrueNAS for jobs.
func TestNoSkipAsksForNoJobs(t *testing.T) {
	r, tn, store := watcher(t)
	tn.set(pool("tank", "SCRUB", "FINISHED", time.Now().Add(-time.Minute), 0))
	r.poll(context.Background())
	if len(feed(t, store)) != 1 {
		t.Fatalf("feed = %+v", feed(t, store))
	}
	for _, m := range tn.methods {
		if m == "core.get_jobs" {
			t.Errorf("asked for jobs: %v", tn.methods)
		}
	}
}

func TestScrubJobPool(t *testing.T) {
	cases := []struct {
		args []any
		want string
	}{
		{[]any{map[string]any{"pool_name": "tank", "threshold": float64(35)}}, "tank"},
		{[]any{"backup", "START"}, "backup"},
		{[]any{float64(3), "START"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := scrubJobPool(c.args); got != c.want {
			t.Errorf("scrubJobPool(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}
