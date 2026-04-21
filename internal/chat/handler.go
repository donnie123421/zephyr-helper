package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

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

	// modelOnce serializes the first Pull across concurrent WS connections so
	// we don't fire N simultaneous downloads. After a successful pull we keep
	// modelReady=true to skip re-checks on subsequent messages.
	modelMu    sync.Mutex
	modelReady bool
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

	// r.Context() isn't reliably cancelled when a WebSocket connection closes
	// (the HTTP request "completes" at upgrade time), so we drive cancellation
	// from the read loop below: any wsjson.Read error cancels this ctx, which
	// aborts any in-flight Pull or Chat goroutines for this connection.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var history []ollama.Message // v1.0 keeps history per-connection only

	// Pre-warm the model in a background goroutine so we can pull a large
	// model while simultaneously watching the read side for a client
	// disconnect. If the client leaves, ctx cancels, Pull aborts.
	slog.Info("chat: pre-warming model on connect", "model", h.ollama.Model(), "ready", h.modelReady)
	prewarmDone := make(chan struct{})
	var prewarmErr error
	go func() {
		defer close(prewarmDone)
		prewarmErr = h.ensureModelPulled(ctx, conn)
	}()

	for {
		var msg inboundEvent
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			slog.Info("chat: ws read ended", "err", err)
			return // defer cancel() aborts pre-warm/pull
		}
		if msg.Type != "user_message" || msg.Content == "" {
			continue
		}

		// Don't attempt chat until the model is pulled and ready.
		select {
		case <-prewarmDone:
		case <-ctx.Done():
			return
		}
		if prewarmErr != nil {
			_ = wsjson.Write(ctx, conn, outboundEvent{Type: "error", Message: prewarmErr.Error()})
			return
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
	reply, err := h.runChat(ctx, conn, history)
	if errors.Is(err, ollama.ErrModelMissing) {
		slog.Info("chat: model not present, starting pull", "model", h.ollama.Model())
		if pullErr := h.ensureModelPulled(ctx, conn); pullErr != nil {
			return "", pullErr
		}
		slog.Info("chat: pull complete, retrying chat")
		reply, err = h.runChat(ctx, conn, history)
	}
	if err != nil {
		return "", err
	}

	if err := wsjson.Write(ctx, conn, outboundEvent{Type: "done"}); err != nil {
		return "", err
	}
	return reply, nil
}

// runChat performs one Chat round, forwarding deltas to the client. Returns
// the assembled reply text, or ErrModelMissing if the model isn't pulled yet.
func (h *Handler) runChat(ctx context.Context, conn *websocket.Conn, history []ollama.Message) (string, error) {
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
	return buf.String(), nil
}

// ensureModelPulled triggers a pull if the model isn't known-ready yet,
// forwarding progress frames to the iOS client so the UI can render them
// inline. Serialized across concurrent connections via modelMu.
func (h *Handler) ensureModelPulled(ctx context.Context, conn *websocket.Conn) error {
	h.modelMu.Lock()
	defer h.modelMu.Unlock()

	if h.modelReady {
		_ = wsjson.Write(ctx, conn, outboundEvent{Type: "status", Content: "Model ready."})
		return nil
	}

	model := h.ollama.Model()
	if err := wsjson.Write(ctx, conn, outboundEvent{
		Type:    "status",
		Content: fmt.Sprintf("Downloading model %s — this can take several minutes.", model),
	}); err != nil {
		slog.Warn("chat: status write failed (intro)", "err", err)
	}

	var lastPct int = -1
	err := h.ollama.Pull(ctx, func(s ollama.PullStatus) {
		// Collapse noisy frames: only forward progress updates where the
		// rounded percent has changed, plus terminal phase transitions.
		if s.Total > 0 {
			pct := int((s.Completed * 100) / s.Total)
			if pct == lastPct {
				return
			}
			lastPct = pct
			if err := wsjson.Write(ctx, conn, outboundEvent{
				Type:    "status",
				Content: fmt.Sprintf("%s (%d%%)", s.Status, pct),
			}); err != nil {
				slog.Warn("chat: status write failed (pct)", "err", err, "pct", pct)
			}
			return
		}
		// Phase transition without percent (e.g. "verifying sha256 digest").
		if err := wsjson.Write(ctx, conn, outboundEvent{
			Type:    "status",
			Content: s.Status,
		}); err != nil {
			slog.Warn("chat: status write failed (phase)", "err", err, "phase", s.Status)
		}
	})
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}

	h.modelReady = true
	_ = wsjson.Write(ctx, conn, outboundEvent{Type: "status", Content: "Model ready."})
	return nil
}
