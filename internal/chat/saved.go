package chat

import (
	"context"
	"errors"
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

// savedChat keeps one connection's chat in the conversation store. A chat is
// saved from its first message, so opening the chat screen and leaving
// doesn't leave an empty conversation behind.
type savedChat struct {
	store *conversations.Store
	id    string
	// For a chat that hasn't started: the title the app asked for, and
	// whether its first message is context the app doesn't show.
	title   string
	context bool
	// off stops saving a chat that was deleted while it was open, rather
	// than bring it back.
	off bool
}

// newSavedChat reads the app's options for a new chat from the query:
// title, and context=1 when the first message is hidden context.
func newSavedChat(store *conversations.Store, query url.Values) *savedChat {
	return &savedChat{store: store, title: query.Get("title"), context: query.Get("context") == "1"}
}

func (c *savedChat) active() bool { return c.store != nil && !c.off }

// resume carries on the saved conversation id in session. It returns
// conversations.ErrNotFound when it has been deleted.
func (c *savedChat) resume(ctx context.Context, id string, session llm.Session) (conversations.Summary, error) {
	conv, err := c.store.Get(ctx, id)
	if err != nil {
		return conversations.Summary{}, err
	}
	for _, m := range conv.Messages {
		switch m.Role {
		case conversations.RoleUser:
			session.AddUserMessage(m.Content)
		case conversations.RoleAssistant:
			// An empty reply can't be replayed to Claude.
			if m.Content != "" {
				session.AddAssistantMessage(m.Content)
			}
		}
	}
	c.id = conv.ID
	return conv.Summary, nil
}

// userSaid saves the user's message. The first one starts the conversation,
// and the app is told its ID.
func (c *savedChat) userSaid(ctx context.Context, send func(any) error, text string) {
	if !c.active() {
		return
	}
	ctx, cancel := storeContext(ctx)
	defer cancel()

	hidden := false
	if c.id == "" {
		title := c.title
		if title == "" {
			title = conversations.TitleFrom(text)
		}
		summary, err := c.store.Start(ctx, title)
		if err != nil {
			// The chat carries on unsaved; the next message tries again.
			slog.Error("chat: couldn't save the conversation", "err", err)
			return
		}
		c.id = summary.ID
		hidden = c.context
		_ = send(conversationEvent{Type: "conversation", ID: summary.ID, Title: summary.Title})
	}
	c.append(ctx, conversations.Message{Role: conversations.RoleUser, Content: text, Hidden: hidden})
}

// turnEnded saves the reply, if the turn finished, and records the model
// and what the turn cost either way.
func (c *savedChat) turnEnded(ctx context.Context, provider llm.Provider, turn *turnRecord, finished bool) {
	if !c.active() || c.id == "" {
		return
	}
	ctx, cancel := storeContext(ctx)
	defer cancel()

	// A turn that failed partway isn't saved: the model never finished
	// the reply, and the session doesn't keep it either.
	if finished && turn.text.Len() > 0 {
		c.append(ctx, conversations.Message{
			Role:    conversations.RoleAssistant,
			Content: turn.text.String(),
			Tools:   turn.tools,
		})
		if !c.active() {
			return
		}
	}
	u := turn.usage
	err := c.store.RecordTurn(ctx, c.id, provider.Name(), provider.Model(),
		u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens, u.OutputTokens)
	if err != nil && !errors.Is(err, conversations.ErrNotFound) {
		slog.Error("chat: couldn't record the turn", "err", err)
	}
}

func (c *savedChat) append(ctx context.Context, m conversations.Message) {
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
	tools []string
	usage llm.Usage
}

// observe wraps send, noting each frame as it goes out.
func (t *turnRecord) observe(send func(any) error) func(any) error {
	return func(v any) error {
		switch ev := v.(type) {
		case outboundEvent:
			switch ev.Type {
			case "delta":
				t.text.WriteString(ev.Content)
			case "tool_used":
				t.tools = append(t.tools, ev.Name)
			}
		case usageEvent:
			t.usage.Add(ev.Usage)
		}
		return send(v)
	}
}
