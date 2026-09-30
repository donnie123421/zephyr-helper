package chat

import (
	"context"
	"log/slog"
	"strings"

	"github.com/donnie123421/zephyr-helper/internal/conversations"
	"github.com/donnie123421/zephyr-helper/internal/llm"
)

// features are what the app can use in a chat, announced in the ready
// frame: "stop" ends the answer being written, keeping what's arrived,
// "retry" writes the last answer again, and "actions" means the helper can
// take actions in chats that ask for them.
func (h *Handler) features() []string {
	features := []string{"stop", "retry"}
	if h.actionsAvailable() {
		features = append(features, "actions")
	}
	return features
}

// readyEvent opens every chat. Actions says this chat has them on.
type readyEvent struct {
	Type     string   `json:"type"`
	Features []string `json:"features"`
	Actions  bool     `json:"actions,omitempty"`
}

// doneEvent ends a turn. Stopped marks one the user stopped partway.
type doneEvent struct {
	Type    string `json:"type"`
	Stopped bool   `json:"stopped,omitempty"`
}

// codeNothingToRetry: Retry in a chat with no question to answer.
const codeNothingToRetry = "nothing_to_retry"

// codeRetryAfterActions: Retry of an answer whose actions ran. Writing it
// again would lose what was done, and invite doing it twice.
const codeRetryAfterActions = "retry_after_actions"

// chatConn is one app's chat over one connection.
type chatConn struct {
	h        *Handler
	send     func(any) error
	onStatus func(string)
	provider llm.Provider
	session  llm.Session
	saved    *savedChat
	// actions is on when the app asked for them and the helper has them.
	actions   bool
	approvals *approvals
	// transcript is the chat as both sides saw it, so Retry can start the
	// session again without the answer it's replacing.
	transcript []conversations.Message
}

// resume carries on a saved conversation.
func (c *chatConn) resume(ctx context.Context, id string) (conversations.Summary, error) {
	conv, err := c.saved.resume(ctx, id, c.session)
	if err != nil {
		return conversations.Summary{}, err
	}
	c.transcript = conv.Messages
	return conv.Summary, nil
}

// ask answers the user's message. It returns false once the connection
// has gone.
func (c *chatConn) ask(connCtx, turnCtx context.Context, text string) bool {
	c.session.AddUserMessage(text)
	c.transcript = append(c.transcript, conversations.Message{Role: conversations.RoleUser, Content: text})
	c.saved.userSaid(connCtx, text)
	return c.answer(connCtx, turnCtx)
}

// retry answers the last question again, as if the answer since had never
// been written: the session starts over from the transcript up to that
// question, and the saved chat drops the old answer.
func (c *chatConn) retry(connCtx, turnCtx context.Context) bool {
	last := -1
	for i := len(c.transcript) - 1; i >= 0; i-- {
		if c.transcript[i].Role == conversations.RoleUser {
			last = i
			break
		}
	}
	if last < 0 {
		return c.send(outboundEvent{Type: "error", Code: codeNothingToRetry, Message: "There's no question to answer again."}) == nil
	}
	for _, m := range c.transcript[last+1:] {
		if ranActions(m.Tools) {
			return c.send(outboundEvent{Type: "error", Code: codeRetryAfterActions,
				Message: "This answer made changes on your NAS, so it can't be written again. Ask again instead."}) == nil
		}
	}
	c.transcript = c.transcript[:last+1]
	c.session = c.provider.NewSession(c.h.systemPrompt(c.actions), c.h.toolList(c.actions))
	for _, m := range replayable(c.transcript) {
		if m.Role == conversations.RoleUser {
			c.session.AddUserMessage(m.Content)
		} else {
			c.session.AddAssistantMessage(m.Content)
		}
	}
	c.saved.dropReplies(connCtx)
	return c.answer(connCtx, turnCtx)
}

// answer runs a turn and ends it for the app: done, done and stopped, or
// an error that leaves the chat open. It returns false once the
// connection has gone.
func (c *chatConn) answer(connCtx, turnCtx context.Context) bool {
	turn := &turnRecord{}
	unfinished, err := c.h.runTurn(turnCtx, turn.observe(c.send), c.onStatus, c.provider, c.session, c.turnActions())
	c.h.recordUsage(connCtx, c.provider, turn.usage)
	reply := turn.text.String()
	switch {
	case err == nil:
		c.remember(reply, turn.tools)
		c.saved.turnEnded(connCtx, c.provider, turn, true)
		return c.send(doneEvent{Type: "done"}) == nil

	case connCtx.Err() != nil:
		// The app left mid-answer; the tokens were still spent.
		c.saved.turnEnded(connCtx, c.provider, turn, false)
		return false

	case turnCtx.Err() != nil:
		// Stopped by the user. What arrived is kept, as they saw it: the
		// session hasn't kept the step that was cut short, so it's added.
		if partial := strings.TrimSpace(unfinished); partial != "" {
			c.session.AddAssistantMessage(partial)
		}
		c.remember(reply, turn.tools)
		c.saved.turnEnded(connCtx, c.provider, turn, true)
		return c.send(doneEvent{Type: "done", Stopped: true}) == nil

	default:
		// Report it and keep the chat open: a rate limit or a flaky
		// connection shouldn't cost the user the conversation.
		slog.Error("chat: turn failed", "provider", c.provider.Name(), "err", err)
		if ranActions(turn.tools) {
			// What the actions did stands, even though the reply didn't.
			c.remember(reply, turn.tools)
		}
		c.saved.turnEnded(connCtx, c.provider, turn, false)
		return c.send(outboundEvent{Type: "error", Message: err.Error()}) == nil
	}
}

// turnActions puts a reply's actions to the user; nil in a chat without
// actions.
func (c *chatConn) turnActions() *turnActions {
	if !c.actions {
		return nil
	}
	return &turnActions{tools: c.h.tools, approvals: c.approvals, log: c.h.actions, timeout: approvalTimeout}
}

// remember adds a reply to the transcript. One with no text isn't a reply,
// unless its actions ran.
func (c *chatConn) remember(reply string, tools []conversations.ToolUse) {
	if strings.TrimSpace(reply) != "" || ranActions(tools) {
		c.transcript = append(c.transcript, conversations.Message{Role: conversations.RoleAssistant, Content: reply, Tools: tools})
	}
}
