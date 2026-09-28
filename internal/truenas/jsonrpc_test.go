package truenas

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// fakeTrueNAS answers JSON-RPC the way TrueNAS does around login: the
// login reply takes a while (TrueNAS hashes the key), and any other call on
// a socket that hasn't logged in yet is rejected with -32001.
type fakeTrueNAS struct {
	loginDelay time.Duration
	loginSeen  chan struct{}
	once       sync.Once
}

func (f *fakeTrueNAS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()
	var mu sync.Mutex
	authenticated := false
	for {
		var req rpcRequest
		if err := wsjson.Read(ctx, conn, &req); err != nil {
			return
		}
		if req.Method == "auth.login_with_api_key" {
			f.once.Do(func() { close(f.loginSeen) })
			go func(id string) {
				time.Sleep(f.loginDelay)
				mu.Lock()
				authenticated = true
				mu.Unlock()
				_ = wsjson.Write(ctx, conn, map[string]any{"jsonrpc": "2.0", "id": id, "result": true})
			}(req.ID)
			continue
		}
		mu.Lock()
		loggedIn := authenticated
		mu.Unlock()
		if !loggedIn {
			_ = wsjson.Write(ctx, conn, map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error": map[string]any{
					"code":    -32001,
					"message": "Method call error",
					"data":    map[string]any{"reason": "Not authenticated"},
				},
			})
			continue
		}
		_ = wsjson.Write(ctx, conn, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": []any{}})
	}
}

// Calls that arrive while the first caller is still logging in must wait for
// the login rather than go out on the unauthenticated socket. At startup the
// pollers and the capacity recorder all make their first call at once.
func TestCallsDuringLoginWaitForIt(t *testing.T) {
	fake := &fakeTrueNAS{loginDelay: 300 * time.Millisecond, loginSeen: make(chan struct{})}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	c := newRPCClient("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/current", "key", nil)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const callers = 6
	errs := make(chan error, callers)
	call := func() {
		_, err := c.Call(ctx, "pool.query", nil)
		errs <- err
	}

	go call()
	select {
	case <-fake.loginSeen:
	case <-ctx.Done():
		t.Fatal("login never reached the server")
	}
	for i := 1; i < callers; i++ {
		go call()
	}
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Errorf("call failed: %v", err)
		}
	}
}

func TestRPCErrorIncludesReason(t *testing.T) {
	withReason := &rpcError{Code: -32001, Message: "Method call error", Data: map[string]any{"reason": "Not authenticated"}}
	if got, want := withReason.Error(), "rpc error -32001: Method call error: Not authenticated"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	plain := &rpcError{Code: -32601, Message: "Method not found"}
	if got, want := plain.Error(), "rpc error -32601: Method not found"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
