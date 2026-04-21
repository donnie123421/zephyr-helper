package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/donnie123421/zephyr-helper/internal/ollama"
	"github.com/donnie123421/zephyr-helper/internal/tools"
)

// maxToolIterations caps the tool-call loop per user message. Six is enough
// for realistic chains ("list pools" → "pool_status tank" → final answer) and
// puts a backstop on pathological cases where the model keeps calling tools
// without ever emitting text.
const maxToolIterations = 6

// systemPromptBase is the persona for every chat, regardless of whether tools
// are wired up. Kept short — long system prompts burn context and make 8B
// models less responsive.
const systemPromptBase = `You are Zephyr, an AI assistant embedded in the user's TrueNAS Scale server. ` +
	`Be concise, practical, and grounded in real data. If the user asks something ambiguous, ask a brief follow-up rather than guess.`

// systemPromptWithTools is appended when the tool registry is non-empty.
const systemPromptWithTools = ` You have tools for querying live NAS state — prefer calling them over guessing. ` +
	`Call a tool whenever the user asks about pools, disks, apps, alerts, or the system. ` +
	`After a tool returns, summarize the result in plain English rather than dumping JSON. ` +
	`If a tool returns an error, explain what likely went wrong and suggest next steps.`

// Wire events — simple tagged unions so the iOS client can switch on `type`.
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
	tools  *tools.Registry

	// modelMu serializes the first Pull across concurrent WS connections so we
	// don't fire N simultaneous downloads. After a successful pull we keep
	// modelReady=true to skip re-checks on subsequent messages.
	modelMu    sync.Mutex
	modelReady bool
}

func NewHandler(c *ollama.Client, reg *tools.Registry) *Handler {
	return &Handler{ollama: c, tools: reg}
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

	// Seed history with the system prompt so the model knows its role + tools.
	history := []ollama.Message{{Role: "system", Content: h.systemPrompt()}}

	// Pre-warm the model in a background goroutine so we can pull a large
	// model while simultaneously watching the read side for a client
	// disconnect. If the client leaves, ctx cancels, Pull aborts.
	slog.Info("chat: pre-warming model on connect",
		"model", h.ollama.Model(),
		"ready", h.modelReady,
		"tools", !h.tools.Empty(),
	)
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

		newHistory, err := h.runTurn(ctx, conn, history)
		if err != nil {
			slog.Error("turn", "err", err)
			_ = wsjson.Write(ctx, conn, outboundEvent{Type: "error", Message: err.Error()})
			return
		}
		history = newHistory

		if err := wsjson.Write(ctx, conn, outboundEvent{Type: "done"}); err != nil {
			slog.Info("chat: done write failed", "err", err)
			return
		}
	}
}

func (h *Handler) systemPrompt() string {
	if h.tools == nil || h.tools.Empty() {
		return systemPromptBase
	}
	return systemPromptBase + systemPromptWithTools
}

// runTurn drives one user-message → assistant-answer cycle, including any
// intermediate tool calls. Returns the updated history (including the
// assistant's final message and any tool results) for the caller to keep.
func (h *Handler) runTurn(ctx context.Context, conn *websocket.Conn, history []ollama.Message) ([]ollama.Message, error) {
	onDelta := func(delta string) error {
		return wsjson.Write(ctx, conn, outboundEvent{Type: "delta", Content: delta})
	}

	toolDefs := h.tools.Definitions() // empty slice is fine — Chat skips the tools key

	for iter := 0; iter < maxToolIterations; iter++ {
		result, err := h.ollama.Chat(ctx, history, toolDefs, onDelta)
		if errors.Is(err, ollama.ErrModelMissing) {
			slog.Info("chat: model not present mid-turn, pulling", "model", h.ollama.Model())
			if pullErr := h.ensureModelPulled(ctx, conn); pullErr != nil {
				return history, pullErr
			}
			// Single retry after a successful pull.
			result, err = h.ollama.Chat(ctx, history, toolDefs, onDelta)
		}
		if err != nil {
			return history, err
		}

		history = append(history, result.Message)

		if len(result.ToolCalls) == 0 {
			// Plain text answer — we already streamed it via onDelta. Done.
			return history, nil
		}

		// Tool calls to dispatch. Emit a user-facing status line for each so
		// the iOS UI can render "Checking pools…" rather than a silent pause.
		for _, tc := range result.ToolCalls {
			_ = wsjson.Write(ctx, conn, outboundEvent{
				Type:    "status",
				Content: h.tools.StatusLine(tc.Function.Name),
			})

			slog.Info("chat: dispatching tool",
				"name", tc.Function.Name,
				"args", string(tc.Function.Arguments),
			)
			toolResult := h.tools.Dispatch(ctx, tc.Function.Name, tc.Function.Arguments)
			history = append(history, ollama.Message{
				Role:    "tool",
				Content: toolResult,
			})
		}
	}

	return history, fmt.Errorf("tool-call loop exceeded %d iterations", maxToolIterations)
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
