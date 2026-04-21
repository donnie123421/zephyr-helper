package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrModelMissing is returned by Chat when Ollama reports the model isn't
// pulled yet (HTTP 404 from /api/chat). Callers should invoke Pull and retry.
var ErrModelMissing = errors.New("ollama: model not found")

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type chatChunk struct {
	Message Message `json:"message"`
	Done    bool    `json:"done"`
}

// Delta is one streaming chunk from Ollama.
type Delta struct {
	Content string
	Done    bool
}

// PullStatus is one progress update from /api/pull.
type PullStatus struct {
	Status    string `json:"status"`
	Digest    string `json:"digest,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Completed int64  `json:"completed,omitempty"`
}

type Client struct {
	baseURL string
	model   string
	http    *http.Client
}

func NewClient(baseURL, model string) *Client {
	return &Client{
		baseURL: baseURL,
		model:   model,
		// No global timeout — streaming requests can legitimately run for minutes.
		http: &http.Client{},
	}
}

// Model returns the configured model name (used for user-facing status text).
func (c *Client) Model() string { return c.model }

// Chat streams a completion for the given messages. Each Delta is sent on out;
// the channel is closed when the stream ends or on error (the error is also
// returned). Cancel via ctx.
func (c *Client) Chat(ctx context.Context, messages []Message, out chan<- Delta) error {
	defer close(out)

	body, err := json.Marshal(chatRequest{
		Model:    c.model,
		Messages: messages,
		Stream:   true,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrModelMissing
	}
	if resp.StatusCode != http.StatusOK {
		// Capture Ollama's response body — its error JSON usually has
		// actionable detail (e.g. "model requires more memory", a file
		// path that failed to open, etc).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("ollama: %s: %s", resp.Status, bytes.TrimSpace(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	// Ollama line payloads carry whole tokens + context — bump from the default 64 KB.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var chunk chatChunk
		if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
			return fmt.Errorf("decode chunk: %w", err)
		}
		out <- Delta{Content: chunk.Message.Content, Done: chunk.Done}
		if chunk.Done {
			return nil
		}
	}
	return scanner.Err()
}

// Pull downloads the configured model, invoking onStatus for each progress
// line. Returns nil on Ollama's terminal {"status":"success"} frame.
// Safe to call repeatedly — if the model is already present Ollama verifies
// layers and returns success quickly.
func (c *Client) Pull(ctx context.Context, onStatus func(PullStatus)) error {
	body, err := json.Marshal(map[string]any{
		"model":  c.model,
		"stream": true,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama pull: %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var s PullStatus
		if err := json.Unmarshal(scanner.Bytes(), &s); err != nil {
			return fmt.Errorf("decode pull status: %w", err)
		}
		if onStatus != nil {
			onStatus(s)
		}
		if s.Status == "success" {
			return nil
		}
	}
	return scanner.Err()
}
