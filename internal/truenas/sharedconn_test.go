package truenas

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A caller giving up must not take the shared connection down with it. The
// websocket library closes a connection whose write context ends, even
// one that ends just as the write finishes, so a job poll timing out used
// to fail whatever call came next.
func TestACallerGivingUpDoesntCloseTheConnection(t *testing.T) {
	fake := &fakeTrueNAS{loginSeen: make(chan struct{})}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c := newRPCClient("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/current", "key", nil)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := c.Call(ctx, "pool.query", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for d := time.Duration(i) * time.Microsecond; ; d = (d + 7*time.Microsecond) % (200 * time.Microsecond) {
				select {
				case <-stop:
					return
				default:
				}
				quick, cancel := context.WithTimeout(ctx, d)
				_, _ = c.Call(quick, "core.get_jobs", nil)
				cancel()
				// Without a pause the abandoned calls flood the socket,
				// and the others queue behind them.
				time.Sleep(time.Millisecond)
			}
		}(i)
	}
	failures := 0
	for i := 0; i < 100; i++ {
		if _, err := c.Call(ctx, "pool.query", nil); err != nil {
			if failures == 0 {
				t.Logf("first failure: %v", err)
			}
			failures++
		}
	}
	close(stop)
	wg.Wait()
	if failures > 0 {
		t.Errorf("%d of 100 calls failed while other callers gave up", failures)
	}
}
