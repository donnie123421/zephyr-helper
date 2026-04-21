package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

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

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: %s", resp.Status)
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
