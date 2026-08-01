package pollers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// fakeNAS speaks just enough JSON-RPC to satisfy truenas.Client: it accepts
// any auth.login_* and answers every other method with a staged result
// (empty list when unstaged). Enough to drive a poller end-to-end without a
// real TrueNAS box.
func fakeNAS(t *testing.T, results map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		for {
			var req struct {
				Method string `json:"method"`
				ID     any    `json:"id"`
			}
			if err := wsjson.Read(ctx, c, &req); err != nil {
				return
			}
			var result any = true
			if req.Method != "auth.login_with_api_key" && req.Method != "auth.login_ex" {
				if staged, ok := results[req.Method]; ok {
					result = staged
				} else {
					result = []any{}
				}
			}
			if err := wsjson.Write(ctx, c, map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": result,
			}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testClient(t *testing.T, results map[string]any) *truenas.Client {
	t.Helper()
	srv := fakeNAS(t, results)
	c, err := truenas.NewClient(srv.URL, "test-key")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// closedStore returns a store whose writes always fail, standing in for a
// transient SQLite error (busy, disk full) during a poll.
func closedStore(t *testing.T) *events.Store {
	t.Helper()
	s, err := events.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return s
}

// A failed ingest must not mark the alert seen: the fingerprint doesn't change
// until the alert re-fires, so marking it would drop the occurrence for good —
// including the push for a CRITICAL.
func TestAlertsRetriedAfterFailedIngest(t *testing.T) {
	tn := testClient(t, map[string]any{
		"alert.list": []any{
			map[string]any{
				"uuid": "u-1", "level": "CRITICAL", "klass": "ZfsDegraded",
				"formatted": "Pool tank is degraded", "dismissed": false,
				"last_occurrence": map[string]any{"$date": float64(1714000000000)},
			},
		},
	})

	p := NewAlerts(tn, closedStore(t), time.Minute, time.Hour)
	p.poll(context.Background())

	if _, marked := p.seen["u-1"]; marked {
		t.Errorf("alert marked seen after a failed ingest — next poll would skip it; seen=%v", p.seen)
	}
}

// Same contract for jobs: a terminal state is final, so a dropped ingest can
// never be recovered by a later poll unless we leave it unmarked.
func TestJobsRetriedAfterFailedIngest(t *testing.T) {
	tn := testClient(t, map[string]any{
		"core.get_jobs": []any{
			map[string]any{
				"id": float64(42), "method": "replication.run", "state": "FAILED",
				"error": "boom", "time_finished": map[string]any{"$date": float64(1714000000000)},
			},
		},
	})

	p := NewJobs(tn, closedStore(t), time.Minute, time.Minute)
	p.poll(context.Background())

	if _, marked := p.seen[42]; marked {
		t.Errorf("job marked seen after a failed ingest — next poll would skip it; seen=%v", p.seen)
	}
}

// Audit rows never change, so a security detection lost to a store error is
// lost permanently unless the row stays unmarked.
func TestSecurityRetriedAfterFailedIngest(t *testing.T) {
	tn := testClient(t, map[string]any{
		"audit.query": []any{
			map[string]any{
				"audit_id": "a-1", "event": "METHOD_CALL",
				"message_timestamp": float64(1714000000),
				"event_data": map[string]any{
					"method": "user.create", "target_username": "mallory",
				},
			},
		},
	})

	p := NewSecurity(tn, closedStore(t), time.Minute)
	p.poll(context.Background())

	if p.seenAuditIDs["a-1"] {
		t.Errorf("audit row marked seen after a failed ingest — next poll would skip it")
	}
}

// The happy path must still mark rows seen, or every poll re-ingests the same
// alert and the feed fills with merge churn.
func TestAlertsMarkedSeenOnSuccess(t *testing.T) {
	tn := testClient(t, map[string]any{
		"alert.list": []any{
			map[string]any{
				"uuid": "u-1", "level": "CRITICAL", "klass": "ZfsDegraded",
				"formatted": "Pool tank is degraded", "dismissed": false,
				"last_occurrence": map[string]any{"$date": float64(1714000000000)},
			},
		},
	})

	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	p := NewAlerts(tn, store, time.Minute, time.Hour)
	p.poll(context.Background())

	if _, marked := p.seen["u-1"]; !marked {
		t.Errorf("alert not marked seen after a successful ingest; seen=%v", p.seen)
	}
	list, err := store.List(context.Background(), events.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d events, want 1", len(list))
	}
}
