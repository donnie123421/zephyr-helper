// Package truenas is a tiny REST client for TrueNAS Scale's /api/v2.0
// endpoints. It's deliberately minimal — no generic "do request" surface,
// just the handful of calls the tool registry needs.
//
// TrueNAS typically uses self-signed certificates, so the client skips TLS
// verification by default (matching the iOS app's behavior).
package truenas

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a single TrueNAS host's REST API.
type Client struct {
	base   string
	token  string
	http   *http.Client
}

// NewClient builds a client against the given base URL (e.g.
// "https://host.docker.internal"). The "/api/v2.0" prefix is appended
// automatically. The API key is sent as a Bearer token on every request.
func NewClient(baseURL, apiKey string) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("truenas: empty base url")
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("truenas: invalid base url: %w", err)
	}
	return &Client{
		base:  strings.TrimRight(baseURL, "/"),
		token: apiKey,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // TrueNAS self-signed
			},
		},
	}, nil
}

// Configured reports whether the client has non-empty base URL + API key.
// Callers use this to decide whether to register TrueNAS-backed tools.
func (c *Client) Configured() bool {
	return c != nil && c.base != "" && c.token != ""
}

// getJSON performs a GET /api/v2.0/<path> and decodes the response into v.
// Paths must start with a leading slash.
func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v2.0"+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("truenas: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB cap
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("truenas %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// GetRaw returns the raw JSON bytes of a GET call. Useful for tool handlers
// that just want to pass the response straight back to the LLM without
// re-marshaling through a typed struct.
func (c *Client) GetRaw(ctx context.Context, path string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.getJSON(ctx, path, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// PostRaw performs a POST against /api/v2.0/<path> with the given JSON
// body and returns the raw response. TrueNAS's filterable query
// endpoints (e.g. /audit/query) require POST with a body of the form
// `{"query-filters": [...], "query-options": {...}}`.
func (c *Client) PostRaw(ctx context.Context, path string, body any) (json.RawMessage, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v2.0"+path, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("truenas: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("truenas %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	return json.RawMessage(respBody), nil
}
