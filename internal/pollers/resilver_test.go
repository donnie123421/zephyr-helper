package pollers

import (
	"context"
	"encoding/json"
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
}

func (s *scriptedPools) set(pools ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pools = pools
}

func (s *scriptedPools) Call(_ context.Context, method string, _ ...any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Marshal(s.pools)
}

func pool(name, function, state string, ended time.Time, errors int) map[string]any {
	scan := map[string]any{"function": function, "state": state, "errors": errors, "end_time": nil}
	if !ended.IsZero() {
		scan["end_time"] = map[string]any{"$date": ended.UnixMilli()}
	}
	return map[string]any{"name": name, "scan": scan}
}

func watcher(t *testing.T) (*Resilvers, *scriptedPools, *events.Store) {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tn := &scriptedPools{}
	return NewResilvers(tn, store, time.Minute), tn, store
}

func feed(t *testing.T, store *events.Store) []events.Event {
	t.Helper()
	evs, err := store.List(context.Background(), events.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return evs
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
// finished while it restarted is.
func TestOnlyARecentFinishIsNewsAtStart(t *testing.T) {
	r, tn, store := watcher(t)
	tn.set(pool("tank", "RESILVER", "FINISHED", time.Now().Add(-48*time.Hour), 0),
		pool("backup", "RESILVER", "FINISHED", time.Now().Add(-5*time.Minute), 0),
		pool("scratch", "SCRUB", "FINISHED", time.Now().Add(-time.Minute), 0))
	r.poll(context.Background())
	evs := feed(t, store)
	if len(evs) != 1 || evs[0].Title != "Resilver of backup finished" {
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
