package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// fakeNAS answers JSON-RPC like TrueNAS, with a scripted reply per method,
// and records every call.
type fakeNAS struct {
	mu      sync.Mutex
	replies map[string]func(params []json.RawMessage) (any, *rpcFault)
	calls   []nasCall
}

type nasCall struct {
	method string
	params []json.RawMessage
}

// rpcFault is a JSON-RPC error reply.
type rpcFault struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

// notFound is TrueNAS's reply for a method it doesn't have.
var notFound = &rpcFault{Code: -32601, Message: "Method not found"}

// failure is a TrueNAS validation error.
func failure(reason string) *rpcFault {
	return &rpcFault{Code: -32001, Message: "Method call error", Data: map[string]any{"reason": reason}}
}

func newFakeNAS() *fakeNAS {
	return &fakeNAS{replies: map[string]func([]json.RawMessage) (any, *rpcFault){}}
}

// on scripts the reply to method.
func (f *fakeNAS) on(method string, reply func(params []json.RawMessage) (any, *rpcFault)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method] = reply
}

// returns scripts a fixed result for method.
func (f *fakeNAS) returns(method string, result any) {
	f.on(method, func([]json.RawMessage) (any, *rpcFault) { return result, nil })
}

// called lists the calls to method, oldest first.
func (f *fakeNAS) called(method string) []nasCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []nasCall
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeNAS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()
	for {
		var req struct {
			ID     string            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := wsjson.Read(ctx, conn, &req); err != nil {
			return
		}
		reply := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if req.Method == "auth.login_with_api_key" {
			reply["result"] = true
		} else {
			f.mu.Lock()
			f.calls = append(f.calls, nasCall{method: req.Method, params: req.Params})
			handler, ok := f.replies[req.Method]
			f.mu.Unlock()
			if !ok {
				reply["error"] = notFound
			} else if result, fault := handler(req.Params); fault != nil {
				reply["error"] = fault
			} else {
				reply["result"] = result
			}
		}
		if err := wsjson.Write(ctx, conn, reply); err != nil {
			return
		}
	}
}

// client connects a TrueNAS client to the fake.
func (f *fakeNAS) client(t *testing.T) *truenas.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	tn, err := truenas.NewClient(srv.URL, "key")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(tn.Close)
	return tn
}

// decodeParam decodes a call's i'th param into v.
func decodeParam(t *testing.T, c nasCall, i int, v any) {
	t.Helper()
	if i >= len(c.params) {
		t.Fatalf("%s has %d params, want at least %d", c.method, len(c.params), i+1)
	}
	if err := json.Unmarshal(c.params[i], v); err != nil {
		t.Fatalf("%s param %d: %v", c.method, i, err)
	}
}
