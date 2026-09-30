// Package truenas talks to a single TrueNAS host over JSON-RPC 2.0
// (WebSocket) to /api/current. The package was REST-only until this
// migration; the GetRaw / PostRaw signatures are kept so every
// existing call site (pollers, tools, remoteaccess handler) keeps
// working — only the underlying transport changed.
//
// Why the migration: TrueNAS Scale 26.04 removes the REST API
// entirely. The deprecation alert started firing weeks before the
// helper got close to the deadline; this rewrite gets us off the
// retired surface.
package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// Client talks to a single TrueNAS host's JSON-RPC API. Public API
// (GetRaw/PostRaw) is unchanged from the REST era so existing
// callers don't need to know about the transport switch.
type Client struct {
	rpc    *rpcClient
	apiKey string
	base   string
}

// NewClient builds a client against the given base URL (e.g.
// "https://host.docker.internal"). The /api/v2.0 path that REST
// used is dropped; we always talk JSON-RPC at /api/current. The API
// key is sent via auth.login_with_api_key on first connect.
func NewClient(baseURL, apiKey string) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("truenas: empty base url")
	}
	wsURL, err := rpcURLFromBase(baseURL)
	if err != nil {
		return nil, fmt.Errorf("truenas: derive ws url: %w", err)
	}
	rpc := newRPCClient(wsURL, apiKey, slog.Default())
	return &Client{
		rpc:    rpc,
		apiKey: apiKey,
		base:   strings.TrimRight(baseURL, "/"),
	}, nil
}

// Configured reports whether the client has non-empty base URL + API
// key. Callers use this to decide whether to register TrueNAS-backed
// tools / pollers.
func (c *Client) Configured() bool {
	return c != nil && c.base != "" && c.apiKey != ""
}

// Close releases the underlying WebSocket connection. Safe to call
// multiple times. Helper main.go defers this on shutdown.
func (c *Client) Close() {
	if c == nil || c.rpc == nil {
		return
	}
	c.rpc.Close()
}

// GetRaw replaces a REST GET. Routes through the mapper, hits
// JSON-RPC, returns the raw result bytes — same shape callers got
// back from the old HTTP path so JSON unmarshal sites don't change.
func (c *Client) GetRaw(ctx context.Context, path string) (json.RawMessage, error) {
	m, err := mapREST("GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("truenas GET %s: %w", path, err)
	}
	result, err := c.rpc.Call(ctx, m.method, m.params)
	if err != nil {
		return nil, fmt.Errorf("truenas %s: %w", m.method, err)
	}
	return result, nil
}

// PostRaw replaces a REST POST. The body is forwarded as the first
// positional JSON-RPC param after any /id/X segment in the path
// (none of the helper's POST endpoints use /id/X today).
func (c *Client) PostRaw(ctx context.Context, path string, body any) (json.RawMessage, error) {
	m, err := mapREST("POST", path, body)
	if err != nil {
		return nil, fmt.Errorf("truenas POST %s: %w", path, err)
	}
	result, err := c.rpc.Call(ctx, m.method, m.params)
	if err != nil {
		return nil, fmt.Errorf("truenas %s: %w", m.method, err)
	}
	return result, nil
}

// Call invokes a JSON-RPC method with positional params and returns the
// raw result.
func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	result, err := c.rpc.Call(ctx, method, params)
	if err != nil {
		return nil, fmt.Errorf("truenas %s: %w", method, err)
	}
	return result, nil
}

// IsMethodNotFound reports whether TrueNAS doesn't have the method called,
// as when a newer method is called on an older version.
func IsMethodNotFound(err error) bool {
	var e *rpcError
	return errors.As(err, &e) && e.Code == -32601
}

// Reason is what TrueNAS said went wrong, without the JSON-RPC framing:
// for a validation error, "Application is already running" rather than
// "rpc error -32001: Method call error: [EINVAL] app_name: Application is
// already running".
func Reason(err error) string {
	var e *rpcError
	if !errors.As(err, &e) {
		return err.Error()
	}
	reason := e.Message
	if data, ok := e.Data.(map[string]any); ok {
		if r, ok := data["reason"].(string); ok && strings.TrimSpace(r) != "" {
			reason = r
		}
	}
	return tidyReason(reason)
}

// tidyReason trims TrueNAS's framing off a message: it keeps the first
// line, without an "[EINVAL] field:" prefix.
func tidyReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if i := strings.IndexByte(reason, '\n'); i >= 0 {
		reason = strings.TrimSpace(reason[:i])
	}
	if strings.HasPrefix(reason, "[") {
		if i := strings.Index(reason, "] "); i > 0 {
			reason = reason[i+2:]
			// A validation error names the field: "app_name: message".
			if j := strings.Index(reason, ": "); j > 0 && isFieldPath(reason[:j]) {
				reason = reason[j+2:]
			}
		}
	}
	return reason
}

// isFieldPath reports whether s looks like the field a validation error
// names, such as "app_name" or "pool_snapshot_create.dataset": lower case,
// unlike the first word of a sentence.
func isFieldPath(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-', r == '[', r == ']':
		default:
			return false
		}
	}
	return true
}
