package chat

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/donnie123421/zephyr-helper/internal/conversations"
	"github.com/donnie123421/zephyr-helper/internal/schedule"
)

// checkInPrompt asks for a check-in's briefing. It's the conversation's
// first message, which the app keeps out of sight.
const checkInPrompt = `This is a scheduled check-in, and the user isn't in the chat: they'll read what you write later. ` +
	`Look the NAS over with your tools (pools, alerts, disks, apps, backup tasks and snapshots), then write a short ` +
	`briefing: one sentence on how things are, then what needs attention, if anything, and what to do about it. ` +
	`Keep it under 200 words. You can't make changes now: suggest them, and the user can ask for them in a chat.`

// CheckIn has the current model look the NAS over and write a briefing,
// saved as a conversation titled title. It only reads: no action tool is
// offered.
func (h *Handler) CheckIn(ctx context.Context, title string) (schedule.CheckInResult, error) {
	if h.saved == nil {
		return schedule.CheckInResult{}, errors.New(conversations.Unavailable)
	}
	provider := h.models.Current()
	if err := provider.Prepare(ctx, func(string) {}); err != nil {
		return schedule.CheckInResult{}, err
	}
	session := provider.NewSession(h.systemPrompt(false), h.toolList(false))
	session.AddUserMessage(checkInPrompt)

	turn := &turnRecord{}
	_, err := h.runTurn(ctx, turn.observe(func(any) error { return nil }), func(string) {}, provider, session, nil)
	h.recordUsage(ctx, provider, turn.usage)
	if err != nil {
		return schedule.CheckInResult{}, err
	}
	briefing := strings.TrimSpace(turn.text.String())
	if briefing == "" {
		return schedule.CheckInResult{}, errors.New("the model wrote nothing")
	}

	saveCtx, cancel := storeContext(ctx)
	defer cancel()
	summary, err := h.saved.Start(saveCtx, title)
	if err != nil {
		return schedule.CheckInResult{}, err
	}
	for _, m := range []conversations.Message{
		{Role: conversations.RoleUser, Content: checkInPrompt, Hidden: true},
		{Role: conversations.RoleAssistant, Content: briefing, Tools: turn.tools},
	} {
		if err := h.saved.Append(saveCtx, summary.ID, m); err != nil {
			return schedule.CheckInResult{}, err
		}
	}
	u := turn.usage
	_ = h.saved.RecordTurn(saveCtx, summary.ID, provider.Name(), provider.Model(),
		u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens, u.OutputTokens)
	return schedule.CheckInResult{ConversationID: summary.ID, Summary: opening(briefing)}, nil
}

// opening is a briefing's first sentence or line, plain, for the
// notification.
func opening(text string) string {
	text = strings.NewReplacer("**", "", "__", "", "#", "", "`", "").Replace(text)
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	if i := strings.Index(text, ". "); i >= 0 {
		text = text[:i+1]
	}
	const max = 160
	if utf8.RuneCountInString(text) > max {
		text = string([]rune(text)[:max-1]) + "…"
	}
	return text
}
