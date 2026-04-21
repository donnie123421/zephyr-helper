package chat

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/donnie123421/zephyr-helper/internal/ollama"
)

// Wire events are simple tagged unions so the iOS client can switch on `type`.
type inboundEvent struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type outboundEvent struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Message string `json:"message,omitempty"`
}

type Handler struct {
	ollama *ollama.Client
}

func NewHandler(c *ollama.Client) *Handler {
	return &Handler{ollama: c}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// App clients aren't browsers; bearer auth is already enforced upstream
		// so the origin check isn't meaningful for this endpoint.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Error("ws accept", "err", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx := r.Context()
	var history []ollama.Message // v1.0 keeps history per-connection only

	for {
		var msg inboundEvent
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		if msg.Type != "user_message" || msg.Content == "" {
			continue
		}
		history = append(history, ollama.Message{Role: "user", Content: msg.Content})

		reply, err := h.stream(ctx, conn, history)
		if err != nil {
			slog.Error("stream", "err", err)
			_ = wsjson.Write(ctx, conn, outboundEvent{Type: "error", Message: err.Error()})
			return
		}
		history = append(history, ollama.Message{Role: "assistant", Content: reply})
	}
}

func (h *Handler) stream(ctx context.Context, conn *websocket.Conn, history []ollama.Message) (string, error) {
	deltas := make(chan ollama.Delta, 16)
	errCh := make(chan error, 1)

	go func() { errCh <- h.ollama.Chat(ctx, history, deltas) }()

	var buf strings.Builder
	for d := range deltas {
		if d.Content == "" {
			continue
		}
		buf.WriteString(d.Content)
		if err := wsjson.Write(ctx, conn, outboundEvent{Type: "delta", Content: d.Content}); err != nil {
			return "", err
		}
	}
	if err := <-errCh; err != nil {
		return "", err
	}
	if err := wsjson.Write(ctx, conn, outboundEvent{Type: "done"}); err != nil {
		return "", err
	}
	return buf.String(), nil
}
