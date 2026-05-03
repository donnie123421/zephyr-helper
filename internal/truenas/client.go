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
