package schedule

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

// runCheckIn has the model look the NAS over and write a briefing, saved
// as a conversation, then tells the user it's ready. It only reads: fixes
// it suggests are for the user to ask for.
func (s *Scheduler) runCheckIn(ctx context.Context, job Job, runID string) (string, string) {
	if s.checkIns == nil {
		return s.checkInFailed(ctx, job, runID, "There's no model set up to write it.")
	}
	title := "Check-in · " + s.now().In(locationOf(job.When)).Format("Mon 2 Jan")
	runCtx, cancel := context.WithTimeout(ctx, checkInFor)
	defer cancel()
	result, err := s.checkIns.CheckIn(runCtx, title)
	if err != nil {
		if ctx.Err() != nil {
			return stopped, ""
		}
		slog.Warn("schedule: check-in failed", "job", job.ID, "err", err)
		why := upperFirst(err.Error())
		if runCtx.Err() != nil {
			why = fmt.Sprintf("The model took longer than %d minutes.", int(checkInFor.Minutes()))
		}
		return s.checkInFailed(ctx, job, runID, why)
	}
	s.note(ctx, runID, events.AssistantCheckIn, events.SeverityInfo, "Your check-in is ready", result.Summary, job, "done",
		map[string]any{"conversation_id": result.ConversationID, "conversation_title": title})
	return "done", "Wrote “" + title + "”."
}

func (s *Scheduler) checkInFailed(ctx context.Context, job Job, runID, why string) (string, string) {
	s.note(ctx, runID, events.AssistantScheduled, events.SeverityWarning, "Check-in didn't run", why, job, "failed", nil)
	return "failed", why
}
