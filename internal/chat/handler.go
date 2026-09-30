package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/donnie123421/zephyr-helper/internal/actions"
	"github.com/donnie123421/zephyr-helper/internal/conversations"
	"github.com/donnie123421/zephyr-helper/internal/llm"
	"github.com/donnie123421/zephyr-helper/internal/tools"
	"github.com/donnie123421/zephyr-helper/internal/usage"
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
	// ID and Approved answer an approval_request.
	ID       string `json:"id,omitempty"`
	Approved bool   `json:"approved,omitempty"`
}

type outboundEvent struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Message string `json:"message,omitempty"`
	// Code marks an error the app acts on (codeConversation*).
	Code string `json:"code,omitempty"`
}

// toolUsedEvent reports a tool the helper just ran and what it returned, so
// the app can leave a "Checked pools" chip in the chat that opens on what
// the assistant looked at.
type toolUsedEvent struct {
	Type string `json:"type"`
	conversations.ToolUse
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
	// saved keeps each chat so the app can reopen it. Nil runs chats
	// without saving them.
	saved *conversations.Store
	// spent counts the tokens hosted models use, for the app's month
	// total. Nil counts nothing.
	spent *usage.Store
	// actions records what the assistant does with the user's approval.
	// Nil turns actions off: the app can ask, but no action tools are
	// offered.
	actions *actions.Log
}

func NewHandler(models *llm.Switch, reg *tools.Registry, saved *conversations.Store, spent *usage.Store, acts *actions.Log) *Handler {
	return &Handler{models: models, tools: reg, saved: saved, spent: spent, actions: acts}
}

// actionsAvailable reports whether this helper can take actions: it has
// the tools and somewhere to record what they do.
func (h *Handler) actionsAvailable() bool {
	return h.actions != nil && h.tools != nil && h.tools.HasActions()
}

// recordUsage counts a turn's tokens toward the month, whatever became of
// the turn: they were spent either way. A turn cut short counts what the
// provider had reported by then. Claude reports the prompt as it starts
// answering; OpenAI-style servers report only at the end, so a stopped
// answer from one isn't counted.
func (h *Handler) recordUsage(ctx context.Context, provider llm.Provider, u llm.Usage) {
	if h.spent == nil || u.IsZero() {
		return
	}
	ctx, cancel := storeContext(ctx)
	defer cancel()
	tokens := usage.Tokens{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadTokens, CacheWrite: u.CacheWriteTokens}
	if err := h.spent.Record(ctx, provider.Name(), provider.Model(), tokens); err != nil {
		slog.Error("chat: couldn't count the turn's tokens", "err", err)
	}
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

	// The app asks for actions with ?actions=1 when the user has Pro and
	// has switched them on. Without it the model never sees a tool that
	// changes anything.
	actionsOn := r.URL.Query().Get("actions") == "1" && h.actionsAvailable()

	// Tell the app what this helper can do in a chat. Apps that don't know
	// the frame skip it.
	if err := send(readyEvent{Type: "ready", Features: h.features(), Actions: actionsOn}); err != nil {
		return
	}

	// The chat keeps the model it started with; a settings change applies
	// from the next chat (or when the app reconnects this one).
	provider := h.models.Current()
	chat := &chatConn{
		h:         h,
		send:      send,
		onStatus:  onStatus,
		provider:  provider,
		actions:   actionsOn,
		approvals: newApprovals(),
		session:   provider.NewSession(h.systemPrompt(actionsOn), h.toolList(actionsOn)),
		saved:     newSavedChat(h.saved, send, r.URL.Query()),
	}

	// The app reopens a saved chat with ?conversation=<id>; the model
	// picks up from its saved messages.
	if id := r.URL.Query().Get("conversation"); id != "" {
		if h.saved == nil {
			// Carrying on without the history would look like the same
			// chat to the app while the model remembered none of it.
			_ = send(outboundEvent{Type: "error", Code: codeConversationsUnavailable, Message: conversations.Unavailable})
			return
		}
		summary, err := chat.resume(ctx, id)
		if err != nil {
			event := outboundEvent{Type: "error", Message: "Couldn't open this conversation."}
			if errors.Is(err, conversations.ErrNotFound) {
				event.Code, event.Message = codeConversationNotFound, "This conversation has been deleted."
			} else {
				slog.Error("chat: couldn't load a saved conversation", "id", id, "err", err)
			}
			_ = send(event)
			return
		}
		if err := send(conversationEvent{Type: "conversation", ID: summary.ID, Title: summary.Title}); err != nil {
			return
		}
	}

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

	// The reader runs alongside the turns, so a stop reaches the answer
	// it's meant for. Anything else waits its turn; the app sends one
	// message at a time, so the queue is a backstop.
	inbox := make(chan inboundEvent, 8)
	var stopMu sync.Mutex
	var stopTurn context.CancelFunc
	go func() {
		defer cancel() // a read error ends the chat
		for {
			var msg inboundEvent
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				slog.Info("chat: ws read ended", "err", err)
				return
			}
			if msg.Type == "stop" {
				stopMu.Lock()
				if stopTurn != nil {
					stopTurn()
				}
				stopMu.Unlock()
				continue
			}
			if msg.Type == "approval_response" {
				// The turn is waiting on it, so it can't queue behind it.
				chat.approvals.deliver(msg.ID, msg.Approved)
				continue
			}
			select {
			case inbox <- msg:
			default:
				slog.Warn("chat: dropped a message sent while the queue was full", "type", msg.Type)
			}
		}
	}()

	for {
		var msg inboundEvent
		select {
		case msg = <-inbox:
		case <-ctx.Done():
			return // the reader cancelled; the pull and any turn abort
		}
		switch {
		case msg.Type == "user_message" && msg.Content != "":
		case msg.Type == "retry":
		default:
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

		turnCtx, stop := context.WithCancel(ctx)
		stopMu.Lock()
		stopTurn = stop
		stopMu.Unlock()

		var open bool
		if msg.Type == "retry" {
			open = chat.retry(ctx, turnCtx)
		} else {
			open = chat.ask(ctx, turnCtx, msg.Content)
		}

		stopMu.Lock()
		stopTurn = nil
		stopMu.Unlock()
		stop()
		if !open {
			return
		}
	}
}

func (h *Handler) systemPrompt(withActions bool) string {
	if h.tools == nil || h.tools.Empty() {
		return systemPromptBase
	}
	if withActions {
		return systemPromptBase + systemPromptWithTools + systemPromptWithActions
	}
	return systemPromptBase + systemPromptWithTools
}

func (h *Handler) toolList(withActions bool) []llm.Tool {
	if h.tools == nil {
		return nil
	}
	defs := h.tools.List(withActions)
	out := make([]llm.Tool, 0, len(defs))
	for _, def := range defs {
		out = append(out, llm.Tool{Name: def.Name, Description: def.Description, Parameters: def.Parameters})
	}
	return out
}

// runTurn drives one user message to the model's answer, running any tools
// it asks for along the way, and putting any actions to the user through
// acts (nil in a chat without actions). When it fails, unfinished is the
// text of the step that was cut short, which the session hasn't kept.
func (h *Handler) runTurn(
	ctx context.Context,
	send func(any) error,
	onStatus func(string),
	provider llm.Provider,
	session llm.Session,
	acts *turnActions,
) (unfinished string, err error) {
	// Every step's text goes out as one reply. When a step that ran tools
	// had already said something ("Let me check your pools."), the next
	// step's text starts a new paragraph instead of running on from it.
	var lastText string
	var stepText strings.Builder
	stepEnded := false
	onDelta := func(delta string) error {
		if stepEnded && lastText != "" {
			switch {
			case strings.HasSuffix(lastText, "\n\n"):
			case strings.HasSuffix(lastText, "\n"):
				delta = "\n" + delta
			default:
				delta = "\n\n" + delta
			}
		}
		stepEnded = false
		if delta != "" {
			lastText = delta
		}
		stepText.WriteString(delta)
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
		stepText.Reset()
		result, err := session.Step(ctx, onDelta)
		if errors.Is(err, llm.ErrModelNotReady) {
			slog.Info("chat: model not present mid-turn, preparing", "model", provider.Model())
			if prepErr := provider.Prepare(ctx, onStatus); prepErr != nil {
				return "", prepErr
			}
			// Single retry once the model is ready.
			result, err = session.Step(ctx, onDelta)
		}
		usage.Add(result.Usage)
		if err != nil {
			return stepText.String(), err
		}
		if len(result.ToolCalls) == 0 {
			// The answer already streamed through onDelta.
			return "", nil
		}

		// Tool calls to dispatch. Emit a user-facing status line for each so
		// the iOS UI can render "Checking pools…" rather than a silent pause,
		// and a `tool_used` frame once the dispatch completes so the UI can
		// leave a persistent chip in the chat thread. Actions wait for the
		// reads, then go to the user as cards.
		results := make([]llm.ToolResult, len(result.ToolCalls))
		var proposed []proposal
		for i, call := range result.ToolCalls {
			if action := h.tools.Action(call.Name); action != nil && acts != nil {
				proposed = append(proposed, proposal{index: i, call: call, action: action})
				continue
			}
			// An action named in a chat without actions isn't one of its
			// tools, and Dispatch refuses it as unknown.
			onStatus(h.tools.StatusLine(call.Name))
			slog.Info("chat: dispatching tool", "name", call.Name, "args", string(call.Arguments))
			content, isError := h.tools.Dispatch(ctx, call.Name, call.Arguments)
			use := conversations.NewToolUse(call.Name, call.Arguments, content, isError, time.Now())
			_ = send(toolUsedEvent{Type: "tool_used", ToolUse: use})
			results[i] = llm.ToolResult{CallID: call.ID, Content: content, IsError: isError}
		}
		if len(proposed) > 0 {
			acts.run(ctx, send, onStatus, proposed, results)
		}
		session.AddToolResults(results)
		stepEnded = true
	}

	return "", fmt.Errorf("tool-call loop exceeded %d iterations", maxToolIterations)
}
