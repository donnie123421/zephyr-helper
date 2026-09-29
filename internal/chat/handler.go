package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/donnie123421/zephyr-helper/internal/llm"
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
// The anti-hallucination guidance is load-bearing: llama3.1:8b will otherwise
// invent plausible-looking pool/disk names when it doesn't have real data.
const systemPromptWithTools = ` You have tools for querying live NAS state. Always prefer calling a tool over guessing. ` +
	`Call a tool whenever the user asks about pools, disks, apps, alerts, shares, users, services, security settings, or the system. ` +
	`Never invent pool, disk, app, alert, share, or user names — only use values that appear in a tool result from this conversation. ` +
	`If you don't know an exact name, call the matching list_* tool first and use one of the names it returns. ` +
	`After a tool returns, summarize the result in plain English rather than dumping JSON. ` +
	`If a tool returns an error, report the error honestly and suggest next steps — do not fabricate data to fill the gap.`

// Wire events — simple tagged unions so the iOS client can switch on `type`.
type inboundEvent struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type outboundEvent struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Message string `json:"message,omitempty"`
	// Name is only populated on `tool_used` frames — the tool the helper just
	// finished dispatching, so iOS can render a persistent "Checked pools" chip.
	Name string `json:"name,omitempty"`
}

// usageEvent reports the tokens one turn used, for models that count them.
// Clients that don't know the frame skip it.
type usageEvent struct {
	Type  string `json:"type"`
	Model string `json:"model"`
	llm.Usage
}

type Handler struct {
	models *llm.Switch
	tools  *tools.Registry
}

func NewHandler(models *llm.Switch, reg *tools.Registry) *Handler {
	return &Handler{models: models, tools: reg}
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
	// aborts any in-flight Prepare or Step for this connection.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	send := func(v any) error { return wsjson.Write(ctx, conn, v) }
	onStatus := func(status string) {
		if err := send(outboundEvent{Type: "status", Content: status}); err != nil {
			slog.Warn("chat: status write failed", "err", err)
		}
	}

	// The chat keeps the model it started with; a settings change applies
	// from the next chat.
	provider := h.models.Current()
	session := provider.NewSession(h.systemPrompt(), h.toolList())

	// Prepare the model in the background so a large Ollama pull can run
	// while the read side watches for a client disconnect. If the client
	// leaves, ctx cancels and the pull aborts.
	slog.Info("chat: connected", "provider", provider.Name(), "model", provider.Model(), "tools", !h.tools.Empty())
	prepareDone := make(chan struct{})
	var prepareErr error
	go func() {
		defer close(prepareDone)
		prepareErr = provider.Prepare(ctx, onStatus)
		if prepareErr != nil {
			// Tell the client immediately — otherwise they'll sit waiting for a
			// download that never finishes and only see the failure when they
			// type a message.
			_ = send(outboundEvent{Type: "error", Message: prepareErr.Error()})
		}
	}()

	for {
		var msg inboundEvent
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			slog.Info("chat: ws read ended", "err", err)
			return // defer cancel() aborts the prepare/pull
		}
		if msg.Type != "user_message" || msg.Content == "" {
			continue
		}

		// Don't chat until the model is ready.
		select {
		case <-prepareDone:
		case <-ctx.Done():
			return
		}
		if prepareErr != nil {
			// The error frame went out when Prepare failed.
			return
		}

		session.AddUserMessage(msg.Content)
		if err := h.runTurn(ctx, send, onStatus, provider, session); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Report it and keep the chat open: a rate limit or a flaky
			// connection shouldn't cost the user the conversation.
			slog.Error("chat: turn failed", "provider", provider.Name(), "err", err)
			if send(outboundEvent{Type: "error", Message: err.Error()}) != nil {
				return
			}
			continue
		}

		if err := send(outboundEvent{Type: "done"}); err != nil {
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

func (h *Handler) toolList() []llm.Tool {
	if h.tools == nil {
		return nil
	}
	defs := h.tools.List()
	out := make([]llm.Tool, 0, len(defs))
	for _, def := range defs {
		out = append(out, llm.Tool{Name: def.Name, Description: def.Description, Parameters: def.Parameters})
	}
	return out
}

// runTurn drives one user message to the model's answer, running any tools
// it asks for along the way.
func (h *Handler) runTurn(
	ctx context.Context,
	send func(any) error,
	onStatus func(string),
	provider llm.Provider,
	session llm.Session,
) error {
	onDelta := func(delta string) error {
		return send(outboundEvent{Type: "delta", Content: delta})
	}

	var usage llm.Usage
	defer func() {
		// Sent even when the turn fails partway: those tokens were spent.
		if !usage.IsZero() {
			_ = send(usageEvent{Type: "usage", Model: provider.Model(), Usage: usage})
		}
	}()

	for iter := 0; iter < maxToolIterations; iter++ {
		result, err := session.Step(ctx, onDelta)
		if errors.Is(err, llm.ErrModelNotReady) {
			slog.Info("chat: model not present mid-turn, preparing", "model", provider.Model())
			if prepErr := provider.Prepare(ctx, onStatus); prepErr != nil {
				return prepErr
			}
			// Single retry once the model is ready.
			result, err = session.Step(ctx, onDelta)
		}
		usage.Add(result.Usage)
		if err != nil {
			return err
		}
		if len(result.ToolCalls) == 0 {
			// The answer already streamed through onDelta.
			return nil
		}

		// Tool calls to dispatch. Emit a user-facing status line for each so
		// the iOS UI can render "Checking pools…" rather than a silent pause,
		// and a `tool_used` frame once the dispatch completes so the UI can
		// leave a persistent chip in the chat thread.
		results := make([]llm.ToolResult, 0, len(result.ToolCalls))
		for _, call := range result.ToolCalls {
			onStatus(h.tools.StatusLine(call.Name))
			slog.Info("chat: dispatching tool", "name", call.Name, "args", string(call.Arguments))
			content, isError := h.tools.Dispatch(ctx, call.Name, call.Arguments)
			_ = send(outboundEvent{Type: "tool_used", Name: call.Name})
			results = append(results, llm.ToolResult{CallID: call.ID, Content: content, IsError: isError})
		}
		session.AddToolResults(results)
	}

	return fmt.Errorf("tool-call loop exceeded %d iterations", maxToolIterations)
}
