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
	"strings"
	"sync"
)

// ErrModelMissing is returned by Chat when Ollama reports the model isn't
// pulled yet (HTTP 404 from /api/chat). Callers should invoke Pull and retry.
var ErrModelMissing = errors.New("ollama: model not found")

// Message mirrors Ollama's chat message shape. ToolCalls is populated only on
// assistant messages emitted while tools are in play; the tag is omitempty so
// plain turns serialize identically to the pre-tools protocol.
type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is one function-call Ollama emits as part of an assistant message.
// Ollama's shape is `{function: {name, arguments}}` — arguments is a JSON
// object (not a string like OpenAI), so we keep it as RawMessage and let the
// tool dispatcher unmarshal into whatever shape each tool expects.
type ToolCall struct {
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ChatResult is what one call to Chat produces. Message is the full assistant
// turn (for appending to history); ToolCalls is a convenience alias for
// Message.ToolCalls so callers don't have to reach through.
type ChatResult struct {
	Message   Message
	ToolCalls []ToolCall
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
	http    *http.Client

	// The active model is mutable at runtime so the iOS picker can switch it
	// without a helper restart. Guarded by mu so concurrent reads during a
	// switch don't race.
	mu    sync.RWMutex
	model string
}

func NewClient(baseURL, model string) *Client {
	return &Client{
		baseURL: baseURL,
		model:   model,
		// No global timeout — streaming requests can legitimately run for minutes.
		http: &http.Client{},
	}
}

// Model returns the currently-active model name.
func (c *Client) Model() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.model
}

// SetModel swaps the active model. Any in-flight Chat/Pull call continues with
// the model it captured on entry — the swap only takes effect on the next call.
func (c *Client) SetModel(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.model = model
}

// Chat runs one streamed chat completion. When tools is non-empty, the model
// may respond with tool_calls instead of (or in addition to) text; those are
// collected into the returned ChatResult for the caller to dispatch.
//
// onDelta is invoked for each non-empty content token. It's optional — pass
// nil to skip streaming (the full content still lands in the returned
// Message). Any error onDelta returns aborts the call.
func (c *Client) Chat(
	ctx context.Context,
	messages []Message,
	tools []map[string]any,
	onDelta func(string) error,
) (ChatResult, error) {
	body := map[string]any{
		"model":    c.Model(),
		"messages": messages,
		"stream":   true,
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return ChatResult{}, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/chat", bytes.NewReader(bodyBytes))
	if err != nil {
		return ChatResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return ChatResult{}, fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ChatResult{}, ErrModelMissing
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ChatResult{}, fmt.Errorf("ollama: %s: %s", resp.Status, bytes.TrimSpace(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	// Ollama line payloads carry whole tokens + context — bump from the default 64 KB.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var content strings.Builder
	var toolCalls []ToolCall

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ChatResult{}, ctx.Err()
		default:
		}

		var chunk struct {
			Message Message `json:"message"`
			Done    bool    `json:"done"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
			return ChatResult{}, fmt.Errorf("decode chunk: %w", err)
		}

		if chunk.Message.Content != "" {
			content.WriteString(chunk.Message.Content)
			if onDelta != nil {
				if err := onDelta(chunk.Message.Content); err != nil {
					return ChatResult{}, err
				}
			}
		}
		if len(chunk.Message.ToolCalls) > 0 {
			toolCalls = append(toolCalls, chunk.Message.ToolCalls...)
		}

		if chunk.Done {
			msg := Message{
				Role:      "assistant",
				Content:   content.String(),
				ToolCalls: toolCalls,
			}
			return ChatResult{Message: msg, ToolCalls: toolCalls}, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return ChatResult{}, err
	}
	// Stream ended without done:true — treat as a truncated response.
	return ChatResult{}, errors.New("ollama: stream ended without done marker")
}

// Pull downloads the configured model, invoking onStatus for each progress
// line. Returns nil on Ollama's terminal {"status":"success"} frame.
// Safe to call repeatedly — if the model is already present Ollama verifies
// layers and returns success quickly.
func (c *Client) Pull(ctx context.Context, onStatus func(PullStatus)) error {
	body, err := json.Marshal(map[string]any{
		"model":  c.Model(),
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
