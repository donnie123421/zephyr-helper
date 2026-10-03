package pollers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// fakeJobsNAS answers core.get_jobs from answer, given its params.
func fakeJobsNAS(t *testing.T, answer func(params []json.RawMessage) any) *truenas.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		for {
			var req struct {
				Method string            `json:"method"`
				ID     any               `json:"id"`
				Params []json.RawMessage `json:"params"`
			}
			if err := wsjson.Read(r.Context(), c, &req); err != nil {
				return
			}
			var result any = true
			if req.Method == "core.get_jobs" {
				result = answer(req.Params)
			}
			if err := wsjson.Write(r.Context(), c, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, err := truenas.NewClient(srv.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func jobAt(id int64, method, state string, finished time.Time, args ...any) map[string]any {
	return map[string]any{"id": float64(id), "method": method, "state": state, "arguments": args,
		"time_finished": map[string]any{"$date": float64(finished.UnixMilli())}}
}

func TestNewestFirstJobsKeepTheirOwnRows(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	// Newest first, as order_by=-id answers: a week of nightly runs.
	var jobs []any
	for i := 0; i < 7; i++ {
		jobs = append(jobs, jobAt(int64(100-i), "replication.run", "SUCCESS", now.AddDate(0, 0, -i), float64(1)))
	}
	p := NewJobs(fakeJobsNAS(t, func([]json.RawMessage) any { return jobs }), store, time.Minute, DefaultJobMergeWindow)
	p.poll(context.Background())
	evs, _ := store.List(context.Background(), events.ListFilter{})
	if len(evs) != 7 {
		t.Fatalf("%d rows for 7 runs", len(evs))
	}
	for _, ev := range evs {
		if ev.Count != 1 || !strings.HasSuffix(ev.DedupeKey, "|1") {
			t.Errorf("row %+v", ev)
		}
	}
}

// A replication that runs for days, while a hundred newer jobs push it out
// of the newest hundred, is still seen to end.
func TestALongJobIsFollowedOutOfTheWindow(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var mu sync.Mutex
	scrubDone := false
	byID := 0
	answer := func(params []json.RawMessage) any {
		mu.Lock()
		defer mu.Unlock()
		if len(params) > 0 && strings.Contains(string(params[0]), `"in"`) {
			byID++
			return []any{jobAt(5, "replication.run", "FAILED", time.Now().UTC(), float64(2))}
		}
		if !scrubDone {
			return []any{map[string]any{"id": float64(5), "method": "replication.run", "state": "RUNNING", "arguments": []any{float64(2)}}}
		}
		// A hundred newer jobs later, the scrub is out of the window.
		return []any{jobAt(200, "pool.dataset.query", "SUCCESS", time.Now().UTC())}
	}
	p := NewJobs(fakeJobsNAS(t, answer), store, time.Minute, DefaultJobMergeWindow)
	p.poll(context.Background())
	mu.Lock()
	scrubDone = true
	mu.Unlock()
	p.poll(context.Background())
	evs, _ := store.List(context.Background(), events.ListFilter{})
	if byID != 1 || len(evs) != 1 || evs[0].DedupeKey != "job|replication.run|failed|2" {
		t.Errorf("asked by id %d times, events %+v", byID, evs)
	}
	if len(p.running) != 0 {
		t.Errorf("still following %v", p.running)
	}
}
