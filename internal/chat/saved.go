package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/conversations"
	"github.com/donnie123421/zephyr-helper/internal/llm"
)

// conversationEvent tells the app which saved conversation the chat is, once
// it has one, so the app can reopen it later.
type conversationEvent struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Codes on error frames the app acts on, rather than just showing.
const (
	// codeConversationNotFound: the chat being reopened was deleted.
	codeConversationNotFound = "conversation_not_found"
	// codeConversationsUnavailable: the helper can't open its saved chats.
	codeConversationsUnavailable = "conversations_unavailable"
)

// contextChatTitle names a context chat the app didn't name, rather than
// title it after the first line of the hidden context.
const contextChatTitle = "Explanation"

// savedChat keeps one connection's chat in the conversation store. A chat is
// saved from its first message, so opening the chat screen and leaving
// doesn't leave an empty conversation behind.
type savedChat struct {
	store *conversations.Store
	send  func(any) error
	id    string
	// For a chat that hasn't started: the title the app asked for, and
	// whether its first message is context the app doesn't show.
	title   string
	context bool
	// pending holds a context chat's messages until its first reply. The
	// app asks for these explanations as soon as a sheet opens; one closed
	// before the answer came would otherwise be saved with nothing to show.
	pending []conversations.Message
	// off stops saving a chat that was deleted while it was open, rather
	// than bring it back.
	off bool
}

// newSavedChat reads the app's options for a new chat from the query:
// title, and context=1 when the first message is hidden context.
func newSavedChat(store *conversations.Store, send func(any) error, query url.Values) *savedChat {
	return &savedChat{store: store, send: send, title: query.Get("title"), context: query.Get("context") == "1"}
}

func (c *savedChat) active() bool { return c.store != nil && !c.off }

// resume carries on the saved conversation id in session. It returns
// conversations.ErrNotFound when it has been deleted.
func (c *savedChat) resume(ctx context.Context, id string, session llm.Session) (conversations.Conversation, error) {
	conv, err := c.store.Get(ctx, id)
	if err != nil {
		return conversations.Conversation{}, err
	}
	for _, m := range replayable(conv.Messages) {
		if m.Role == conversations.RoleUser {
			session.AddUserMessage(m.Content)
		} else {
			session.AddAssistantMessage(m.Content)
		}
	}
	c.id = conv.ID
	return conv, nil
}

// replayable tidies saved messages into a transcript every model accepts:
// no blank text (Claude rejects whitespace-only blocks), user first, and
// strictly alternating roles, which some servers' chat templates require.
// A failed turn leaves two user messages in a row; they're joined. Replies
// that proposed actions say what became of them, so a model carrying the
// chat on knows what was done.
func replayable(messages []conversations.Message) []conversations.Message {
	var out []conversations.Message
	for _, m := range messages {
		content := m.Content
		if m.Role == conversations.RoleAssistant {
			if note := actionNote(m.Tools); note != "" {
				content = strings.TrimSpace(content + "\n\n" + note)
			}
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		if m.Role != conversations.RoleUser && m.Role != conversations.RoleAssistant {
			continue
		}
		if len(out) == 0 && m.Role != conversations.RoleUser {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			out[len(out)-1].Content += "\n\n" + content
			continue
		}
		out = append(out, conversations.Message{Role: m.Role, Content: content})
	}
	return out
}

// ranActions reports whether any of a reply's actions ran: done, started
// or failed.
func ranActions(uses []conversations.ToolUse) bool {
	for _, use := range uses {
		if use.Action == nil {
			continue
		}
		switch use.Action.Status {
		case actionDone, actionStarted, actionFailed:
			return true
		}
	}
	return false
}

// actionNote says what became of a reply's actions, for the model.
func actionNote(uses []conversations.ToolUse) string {
	var lines []string
	for _, use := range uses {
		a := use.Action
		if a == nil {
			continue
		}
		var what string
		switch a.Status {
		case actionDone, actionStarted:
			what = a.Message
		case actionFailed:
			what = "it failed: " + a.Message
		case actionDeclined:
			what = "the user declined it"
		default:
			what = "it wasn't done"
		}
		lines = append(lines, fmt.Sprintf("“%s”: %s", a.Title, strings.TrimSpace(what)))
	}
	if len(lines) == 0 {
		return ""
	}
	return "(Actions in this reply, which the user was asked to approve: " + strings.Join(lines, "; ") + ")"
}

// userSaid saves the user's message. The first one starts the conversation,
// and the app is told its ID; a context chat waits for its first reply.
func (c *savedChat) userSaid(ctx context.Context, text string) {
	if !c.active() {
		return
	}
	ctx, cancel := storeContext(ctx)
	defer cancel()

	message := conversations.Message{Role: conversations.RoleUser, Content: text}
	if c.id != "" {
		c.append(ctx, message)
		return
	}
	if c.context {
		// Only the chat's very first message is the app's hidden context.
		message.Hidden = len(c.pending) == 0
		c.pending = append(c.pending, message)
		return
	}
	title := c.title
	if title == "" {
		title = conversations.TitleFrom(text)
	}
	// If this fails the chat carries on unsaved, and the next message
	// tries again.
	if c.start(ctx, title) {
		c.append(ctx, message)
	}
}

// turnEnded saves the reply when keep is set (the turn finished, or the
// user stopped it and so has seen it), and records the model and what the
// turn cost either way.
func (c *savedChat) turnEnded(ctx context.Context, provider llm.Provider, turn *turnRecord, keep bool) {
	if !c.active() {
		return
	}
	ctx, cancel := storeContext(ctx)
	defer cancel()

	// A turn that failed partway isn't saved: the model never finished
	// the reply, and the session doesn't keep it either. Nor is a reply
	// with no text, which can't be replayed. Unless an action ran: that
	// happened, so a reopened chat shows it, and the model carrying the
	// chat on is told (replayable).
	reply := turn.text.String()
	answered := keep && strings.TrimSpace(reply) != ""
	if !answered && ranActions(turn.tools) {
		answered = true
	}

	if c.id == "" {
		if len(c.pending) == 0 || !answered {
			return
		}
		title := c.title
		if title == "" {
			title = contextChatTitle
		}
		if !c.start(ctx, title) {
			return
		}
		for _, m := range c.pending {
			c.append(ctx, m)
		}
		c.pending = nil
	}
	if answered {
		c.append(ctx, conversations.Message{
			Role:    conversations.RoleAssistant,
			Content: reply,
			Tools:   turn.tools,
		})
	}
	if !c.active() {
		return
	}
	u := turn.usage
	err := c.store.RecordTurn(ctx, c.id, provider.Name(), provider.Model(),
		u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens, u.OutputTokens)
	if err != nil && !errors.Is(err, conversations.ErrNotFound) {
		slog.Error("chat: couldn't record the turn", "err", err)
	}
}

// dropReplies forgets what came after the last question, for Retry.
func (c *savedChat) dropReplies(ctx context.Context) {
	if !c.active() || c.id == "" {
		return
	}
	ctx, cancel := storeContext(ctx)
	defer cancel()
	if err := c.store.DropReplies(ctx, c.id); err != nil {
		slog.Error("chat: couldn't drop the answer being redone", "err", err)
	}
}

// start creates the conversation and tells the app its ID.
func (c *savedChat) start(ctx context.Context, title string) bool {
	summary, err := c.store.Start(ctx, title)
	if err != nil {
		slog.Error("chat: couldn't save the conversation", "err", err)
		return false
	}
	c.id = summary.ID
	_ = c.send(conversationEvent{Type: "conversation", ID: summary.ID, Title: summary.Title})
	return true
}

func (c *savedChat) append(ctx context.Context, m conversations.Message) {
	if c.off {
		return
	}
	err := c.store.Append(ctx, c.id, m)
	if errors.Is(err, conversations.ErrNotFound) {
		slog.Info("chat: conversation deleted while open; no longer saving it", "id", c.id)
		c.off = true
		return
	}
	if err != nil {
		slog.Error("chat: couldn't save a message", "err", err)
	}
}

// storeContext outlives the connection for a moment, so a reply that
// finished just as the app left is still saved.
func storeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// turnRecord collects what one turn showed the app: the reply's text, the
// tools that ran and the tokens used.
type turnRecord struct {
	text  strings.Builder
	tools []conversations.ToolUse
	usage llm.Usage
}

// observe wraps send, noting each frame as it goes out.
func (t *turnRecord) observe(send func(any) error) func(any) error {
	return func(v any) error {
		switch ev := v.(type) {
		case outboundEvent:
			if ev.Type == "delta" {
				t.text.WriteString(ev.Content)
			}
		case toolUsedEvent:
			t.tools = append(t.tools, ev.ToolUse)
		case usageEvent:
			t.usage.Add(ev.Usage)
		}
		return send(v)
	}
}
