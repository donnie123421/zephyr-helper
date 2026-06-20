// JSON-RPC 2.0 over WebSocket transport for TrueNAS Scale 25.04+.
// Replaces the deprecated REST API the helper used to talk to TrueNAS,
// keeping the client.go surface (GetRaw/PostRaw) so every existing
// caller (pollers, tools, remote-access handler) keeps working without
// changes.
//
// TrueNAS 26.04 removes REST entirely — every helper-side call has to
// move to JSON-RPC before that release. This client + the mapper that
// sits on top of it are the migration vehicle.
package truenas

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// rpcRequest mirrors the JSON-RPC 2.0 request envelope. We always
// emit string IDs so multiplexed responses can be looked up in the
// pending map without numeric collisions.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  []any  `json:"params,omitempty"`
	ID      string `json:"id"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	ID      string          `json:"id,omitempty"`
}

// rpcError matches the JSON-RPC 2.0 error object. -32601 = method
// not found; -32602 = invalid params. Both used by the iOS adapter
// for fallback chains; mirror here for symmetry.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// rpcClient owns a long-lived WebSocket to TrueNAS at /api/current.
// Concurrent callers go through Call(); responses route back to the
// caller via a per-request channel keyed by id.
type rpcClient struct {
	wsURL  string
	apiKey string
	log    *slog.Logger

	// connectMu serializes Dial attempts so a thundering-herd of
	// concurrent first-callers doesn't open N connections.
	connectMu sync.Mutex
	// connMu protects conn + pending. Read-locked on every Call to
	// access the live connection; write-locked when (re)connecting.
	connMu  sync.RWMutex
	conn    *websocket.Conn
	pending map[string]chan rpcResponse

	nextID atomic.Uint64
	closed atomic.Bool

	// Per-call timeout. JSON-RPC over WS doesn't time out on its
	// own; without this a stuck server hangs every caller forever.
	callTimeout time.Duration
}

// newRPCClient builds an unconnected client. Connect() is called
// lazily on the first Call so a misconfigured TrueNAS URL doesn't
// fail the helper's startup.
func newRPCClient(wsURL, apiKey string, log *slog.Logger) *rpcClient {
	if log == nil {
		log = slog.Default()
	}
	return &rpcClient{
		wsURL:       wsURL,
		apiKey:      apiKey,
		log:         log,
		pending:     map[string]chan rpcResponse{},
		callTimeout: 30 * time.Second,
	}
}

// Call invokes the named JSON-RPC method with positional `params`
// and returns the raw result bytes. Triggers a connect on first use
// and on any transport error.
func (c *rpcClient) Call(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, errors.New("rpc: client closed")
	}

	conn, err := c.ensureConnected(ctx)
	if err != nil {
		return nil, fmt.Errorf("rpc connect: %w", err)
	}

	id := strconv.FormatUint(c.nextID.Add(1), 10)
	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	}

	respCh := make(chan rpcResponse, 1)
	c.connMu.Lock()
	c.pending[id] = respCh
	c.connMu.Unlock()
	defer func() {
		c.connMu.Lock()
		delete(c.pending, id)
		c.connMu.Unlock()
	}()

	writeCtx, cancelWrite := context.WithTimeout(ctx, 10*time.Second)
	defer cancelWrite()
	if err := wsjson.Write(writeCtx, conn, req); err != nil {
		// Drop the dead connection so the next Call reconnects.
		c.dropConnection(conn)
		return nil, fmt.Errorf("rpc write %s: %w", method, err)
	}

	timeoutCtx, cancelWait := context.WithTimeout(ctx, c.callTimeout)
	defer cancelWait()
	select {
	case <-timeoutCtx.Done():
		return nil, fmt.Errorf("rpc %s: %w", method, timeoutCtx.Err())
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// ensureConnected returns a live connection, opening one if needed.
// Single-flighted via connectMu so concurrent first-callers don't
// thundering-herd the dial.
func (c *rpcClient) ensureConnected(ctx context.Context) (*websocket.Conn, error) {
	c.connMu.RLock()
	conn := c.conn
	c.connMu.RUnlock()
	if conn != nil {
		return conn, nil
	}

	c.connectMu.Lock()
	defer c.connectMu.Unlock()

	// Double-checked: another caller may have connected while we
	// were waiting for the lock.
	c.connMu.RLock()
	conn = c.conn
	c.connMu.RUnlock()
	if conn != nil {
		return conn, nil
	}

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	httpClient := &http.Client{
		// TrueNAS commonly uses self-signed certs — same policy as
		// the legacy REST client we're replacing.
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
	dialOpts := &websocket.DialOptions{HTTPClient: httpClient}

	newConn, _, err := websocket.Dial(dialCtx, c.wsURL, dialOpts)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.wsURL, err)
	}
	// Default frame limit is too small for some TrueNAS responses
	// (e.g. /pool/dataset on a busy NAS). 4 MiB matches the cap the
	// legacy REST client used.
	newConn.SetReadLimit(4 << 20)

	c.connMu.Lock()
	c.conn = newConn
	c.connMu.Unlock()

	go c.readLoop(newConn)

	if err := c.authenticate(ctx, newConn); err != nil {
		c.dropConnection(newConn)
		return nil, fmt.Errorf("auth: %w", err)
	}

	c.log.Info("rpc: connected", "url", c.wsURL)
	return newConn, nil
}

// authenticate logs in with the helper's TrueNAS API key. Tries the
// modern auth.login_ex(API_KEY_PLAIN) first only when we have a
// username; falls back to the legacy auth.login_with_api_key which
// still works on 26.0 and accepts a bare key.
func (c *rpcClient) authenticate(ctx context.Context, conn *websocket.Conn) error {
	// We don't carry a username for the helper API key, so jump
	// straight to the legacy method. If TrueNAS removes it we'll
	// add the username collection at install time.
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  "auth.login_with_api_key",
		Params:  []any{c.apiKey},
		ID:      id,
	}
	respCh := make(chan rpcResponse, 1)
	c.connMu.Lock()
	c.pending[id] = respCh
	c.connMu.Unlock()
	defer func() {
		c.connMu.Lock()
		delete(c.pending, id)
		c.connMu.Unlock()
	}()

	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := wsjson.Write(writeCtx, conn, req); err != nil {
		return fmt.Errorf("write login: %w", err)
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, 15*time.Second)
	defer cancelWait()
	select {
	case <-waitCtx.Done():
		return waitCtx.Err()
	case resp := <-respCh:
		if resp.Error != nil {
			return resp.Error
		}
		if !loginResultOK(resp.Result) {
			return errors.New("auth.login_with_api_key: authentication failed")
		}
		return nil
	}
}

// loginResultOK reports whether an auth.login_* result indicates success.
// TrueNAS changed the result shape across versions: pre-25.10.3 returned a
// bare bool, while 25.10.3+ (and 26) return an auth.login_ex-style object
// {response_type: "SUCCESS"|"AUTH_ERR"|...}. Accept either positive shape and
// treat everything else — including an unparseable or unknown result — as a
// failure, so an expired or invalid key fails loudly instead of silently
// being treated as authenticated (the old bool-only cast returned success for
// any non-bool result, i.e. every 25.10.3+ response). Mirrors the iOS
// WebSocketClient.authenticate logic.
func loginResultOK(raw json.RawMessage) bool {
	// Object shape: {response_type: "SUCCESS", ...}. Checked first because
	// unmarshalling an object into a bool fails, but unmarshalling a bare
	// bool into this struct also fails — so the two checks don't overlap.
	var obj struct {
		ResponseType string `json:"response_type"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.ResponseType != "" {
		return strings.EqualFold(obj.ResponseType, "SUCCESS")
	}
	// Bare bool shape (pre-25.10.3).
	var ok bool
	if err := json.Unmarshal(raw, &ok); err == nil {
		return ok
	}
	return false
}

// readLoop owns the connection's read side. Routes every response
// back to the waiting Call via the pending map. On read error it
// drops the connection so the next Call reconnects.
func (c *rpcClient) readLoop(conn *websocket.Conn) {
	for {
		var resp rpcResponse
		err := wsjson.Read(context.Background(), conn, &resp)
		if err != nil {
			if !c.closed.Load() {
				c.log.Warn("rpc read loop ended", "err", err)
			}
			c.dropConnection(conn)
			return
		}
		if resp.ID == "" {
			// Notification — no id to route to. TrueNAS uses these
			// for subscription pushes, which the helper doesn't
			// consume yet. Drop silently.
			continue
		}
		c.connMu.RLock()
		ch := c.pending[resp.ID]
		c.connMu.RUnlock()
		if ch == nil {
			continue
		}
		// Non-blocking send so a stalled caller can't wedge the
		// reader. The Call goroutine drains the buffered channel
		// once on resume.
		select {
		case ch <- resp:
		default:
		}
	}
}

// dropConnection clears the live connection so the next Call
// reconnects. Idempotent so the writer + reader can both call it
// after observing the same fault.
func (c *rpcClient) dropConnection(conn *websocket.Conn) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn == conn {
		_ = c.conn.Close(websocket.StatusInternalError, "transport reset")
		c.conn = nil
	}
}

// Close shuts the client down for good — used at program exit so
// the WS connection is torn down cleanly. Safe to call multiple
// times.
func (c *rpcClient) Close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close(websocket.StatusNormalClosure, "shutdown")
		c.conn = nil
	}
}

// rpcURLFromBase converts the user-supplied TrueNAS base URL into the
// JSON-RPC WebSocket URL. https://host → wss://host/api/current.
// Tolerant of trailing slashes and pre-supplied /api/v2.0 suffix.
func rpcURLFromBase(base string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if trimmed == "" {
		return "", errors.New("empty base url")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse base url: %w", err)
	}
	scheme := parsed.Scheme
	switch strings.ToLower(scheme) {
	case "https":
		scheme = "wss"
	case "http":
		scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	out := &url.URL{
		Scheme: scheme,
		Host:   parsed.Host,
		Path:   "/api/current",
	}
	return out.String(), nil
}
