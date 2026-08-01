package push

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

// stalledRelay accepts /push but holds every request until Release is called,
// standing in for a relay that's up but very slow (or an OneSignal hiccup).
type stalledRelay struct {
	release     chan struct{}
	releaseOnce sync.Once

	mu       sync.Mutex
	received int
}

func (s *stalledRelay) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
}

// Release unblocks every in-flight and future request. Idempotent so tests can
// call it explicitly and still rely on the cleanup hook.
func (s *stalledRelay) Release() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func newStalledRelay(t *testing.T) (*stalledRelay, *httptest.Server) {
	t.Helper()
	r := &stalledRelay{release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-r.release:
		case <-req.Context().Done():
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		r.mu.Lock()
		r.received++
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	// Registered after srv.Close so it runs *first* (cleanups are LIFO):
	// httptest.Server.Close blocks on outstanding handlers, and a handler
	// parked on `release` would wedge teardown for the whole package.
	t.Cleanup(r.Release)
	return r, srv
}

// The regression: delivery used to run inline in the Run loop, so a stalled
// relay stopped the loop draining its subscription. events.Store.broadcast
// drops frames for a backed-up subscriber (buffer 64), so pushes were silently
// discarded during precisely the outage you'd want reported. Receiving is now
// decoupled from sending, so a burst larger than the subscription buffer still
// gets delivered once the relay recovers.
func TestNotifierKeepsDrainingWhileRelayStalls(t *testing.T) {
	relay, srv := newStalledRelay(t)

	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	n := New(store, srv.URL, "sub-1", "tok-1", nil, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		n.Run(ctx)
	}()

	// Give Run time to subscribe before we start producing.
	waitFor(t, 5*time.Second, func() bool { return store.SubscriberCount() > 0 })

	// Comfortably more than the store's 64-deep subscriber buffer, comfortably
	// under the notifier's own queue depth.
	const burst = 120
	for i := 0; i < burst; i++ {
		ev := events.Event{
			OccurredAt: time.Now().UTC(),
			Kind:       events.KindPool,
			Severity:   events.SeverityCritical,
			Title:      fmt.Sprintf("pool %d degraded", i),
			Summary:    "test",
			DedupeKey:  fmt.Sprintf("test|pool|%d", i),
		}
		if _, err := store.Ingest(ctx, ev, time.Hour); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	// Nothing should have landed yet — the relay is holding every request.
	if got := relay.count(); got != 0 {
		t.Fatalf("relay received %d before release, want 0", got)
	}

	relay.Release()

	waitFor(t, 30*time.Second, func() bool { return relay.count() >= burst })
	if got := relay.count(); got != burst {
		t.Errorf("relay received %d pushes, want all %d — events were dropped "+
			"while the relay was stalled", got, burst)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Error("Run did not return after context cancellation")
	}
}

// A cancelled context must stop the notifier promptly even with a backlog
// queued, rather than working through it against a dead relay.
func TestNotifierShutsDownWithBacklog(t *testing.T) {
	// Never released — every send is still in flight when we cancel.
	_, srv := newStalledRelay(t)

	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	n := New(store, srv.URL, "sub-1", "tok-1", nil, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		n.Run(ctx)
	}()
	waitFor(t, 5*time.Second, func() bool { return store.SubscriberCount() > 0 })

	for i := 0; i < 50; i++ {
		_, _ = store.Ingest(ctx, events.Event{
			OccurredAt: time.Now().UTC(),
			Kind:       events.KindPool,
			Severity:   events.SeverityCritical,
			Title:      "degraded",
			Summary:    "test",
			DedupeKey:  fmt.Sprintf("shutdown|%d", i),
		}, time.Hour)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return promptly with a queued backlog")
	}
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", limit)
}
