package chat

import (
	"context"
	"errors"
	"strings"
)

// summaryPrompt asks for the weekly digest's summary. It reads only the
// facts it's given: no tools.
const summaryPrompt = `You write the summary at the top of a weekly digest for someone's TrueNAS NAS. ` +
	`From the facts below, write two or three short sentences: how the week went, and the one thing ` +
	`most worth their attention, if anything. Plain English, no lists, no headings, under 70 words. ` +
	`Don't invent anything that isn't in the facts.`

// Summarize has the current model write a few sentences from a digest's
// facts, in one call with no tools.
func (h *Handler) Summarize(ctx context.Context, facts string) (string, error) {
	provider := h.models.Current()
	if err := provider.Prepare(ctx, func(string) {}); err != nil {
		return "", err
	}
	session := provider.NewSession(summaryPrompt, nil)
	session.AddUserMessage("This week's facts:\n" + facts)
	var text strings.Builder
	result, err := session.Step(ctx, func(delta string) error {
		text.WriteString(delta)
		return nil
	})
	h.recordUsage(ctx, provider, result.Usage)
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(text.String())
	if summary == "" {
		return "", errors.New("the model wrote nothing")
	}
	return summary, nil
}
