package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/actions"
	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/tools"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// How the scheduler keeps time.
const (
	// tickEvery is how often it looks for due jobs.
	tickEvery = 30 * time.Second
	// lateBy is how late a job may still run, as when the helper was
	// restarting at its time. Later than that, it's missed.
	lateBy = 2 * time.Hour
	// actionRunFor bounds one action's run, as in a chat.
	actionRunFor = 6 * time.Minute
	// checkInFor bounds a check-in, which may wait on the model loading.
	checkInFor = 15 * time.Minute
)

// stopped is the status of a run cut short by the helper stopping. It isn't
// recorded: the job shows as interrupted when the helper starts again.
const stopped = ""

// NAS is what the update rule needs of TrueNAS. *truenas.Client is one.
type NAS interface {
	Call(ctx context.Context, method string, params ...any) (json.RawMessage, error)
	WaitJob(ctx context.Context, id int64, interval time.Duration) (truenas.Job, error)
}

// CheckIns writes a check-in: the model looks the NAS over and writes a
// briefing, saved as a conversation.
type CheckIns interface {
	CheckIn(ctx context.Context, title string) (CheckInResult, error)
}

// CheckInResult is a briefing written.
type CheckInResult struct {
	ConversationID string
	// Summary is the briefing's opening, for the notification.
	Summary string
}

// Scheduler runs booked jobs when they're due.
type Scheduler struct {
	store    *Store
	tools    *tools.Registry
	log      *actions.Log
	events   *events.Store
	nas      NAS
	checkIns CheckIns
	now      func() time.Time
	tick     time.Duration

	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
	// base is what Run was given: runs end with it, not with the request
	// that asked for one.
	base context.Context
	// timing for the update rule; shortened in tests.
	ruleTiming ruleTiming
}

// New builds a scheduler. checkIns may be nil, when there's no model to
// write them.
func New(store *Store, reg *tools.Registry, log *actions.Log, evs *events.Store, nas NAS, checkIns CheckIns) *Scheduler {
	return &Scheduler{
		store: store, tools: reg, log: log, events: evs, nas: nas, checkIns: checkIns,
		now: time.Now, tick: tickEvery, running: map[string]bool{}, ruleTiming: defaultRuleTiming,
	}
}

// Run looks for due jobs until ctx ends. It first notes runs the helper
// stopped partway through, last time.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
	s.interrupted(ctx)
	s.runDue(ctx)
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-ticker.C:
			s.runDue(ctx)
		}
	}
}

// Wait blocks until every run under way has finished.
func (s *Scheduler) Wait() { s.wg.Wait() }

// interrupted notes the runs the helper stopped partway through.
func (s *Scheduler) interrupted(ctx context.Context) {
	jobs, err := s.store.Unfinished(ctx)
	if err != nil {
		slog.Error("schedule: couldn't read unfinished runs", "err", err)
		return
	}
	for _, job := range jobs {
		why := "The helper stopped while it was running, so it may not have finished."
		if job.Kind == KindUpdateRule {
			why = "The helper stopped partway through, so an app may have been left mid-update. Check your apps in the Apps tab."
		}
		s.note(ctx, job.ID+"-"+newID()[:6], entryFor(job), events.SeverityWarning, "Interrupted: "+job.Title, why, job, "interrupted", nil)
		if err := s.store.Finish(ctx, job.ID, "interrupted", why); err != nil && !errors.Is(err, ErrNotFound) {
			slog.Error("schedule: couldn't record an interrupted run", "job", job.ID, "err", err)
		}
	}
}

func (s *Scheduler) runDue(ctx context.Context) {
	now := s.now()
	due, err := s.store.Due(ctx, now)
	if err != nil {
		slog.Error("schedule: couldn't read due jobs", "err", err)
		return
	}
	for _, job := range due {
		if s.isRunning(job.ID) {
			// A run of it from Run Now is still going; this one waits.
			continue
		}
		if now.Sub(*job.NextRun) > lateBy {
			s.missed(ctx, job, now)
			continue
		}
		s.startDue(ctx, job, now)
	}
}

// startDue claims a due job's run, its next run booked first, and starts
// it.
func (s *Scheduler) startDue(ctx context.Context, job Job, now time.Time) {
	var next *time.Time
	if t, ok := job.When.Next(now); ok && job.When.Repeat != "once" {
		next = &t
	}
	if !s.reserve(job.ID) {
		return
	}
	claimed, err := s.store.Claim(ctx, job.ID, *job.NextRun, now, next)
	if err != nil || !claimed {
		s.release(job.ID)
		if err != nil {
			slog.Error("schedule: couldn't claim a run", "job", job.ID, "err", err)
		}
		return
	}
	s.launch(ctx, job)
}

// RunNow runs a job straight away, as the app's Run Now does, leaving its
// schedule as it is.
func (s *Scheduler) RunNow(ctx context.Context, id string) error {
	job, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	// The run carries on after the request that asked for it is answered.
	base := s.baseContext()
	if base == nil {
		base = context.WithoutCancel(ctx)
	}
	if !s.reserve(job.ID) {
		return fmt.Errorf("%s is already running", job.Title)
	}
	if err := s.store.Started(ctx, job.ID, s.now()); err != nil {
		s.release(job.ID)
		return err
	}
	s.launch(base, job)
	return nil
}

func (s *Scheduler) reserve(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[id] {
		return false
	}
	s.running[id] = true
	return true
}

func (s *Scheduler) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, id)
}

func (s *Scheduler) isRunning(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[id]
}

// launch does a reserved job's run in the background, and records how it
// went.
func (s *Scheduler) launch(ctx context.Context, job Job) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.release(job.ID)
		runID := job.ID + "-" + newID()[:6]
		status, message := s.run(ctx, job, runID)
		if status == stopped {
			return
		}
		if err := s.store.Finish(context.WithoutCancel(ctx), job.ID, status, message); err != nil && !errors.Is(err, ErrNotFound) {
			slog.Error("schedule: couldn't record a run", "job", job.ID, "err", err)
		}
	}()
}

// run does the job, and says how it went.
func (s *Scheduler) run(ctx context.Context, job Job, runID string) (status, message string) {
	slog.Info("schedule: running", "job", job.ID, "kind", job.Kind, "title", job.Title)
	switch job.Kind {
	case KindAction:
		return s.runAction(ctx, job, runID)
	case KindUpdateRule:
		return s.runUpdateRule(ctx, job, runID)
	case KindCheckIn:
		return s.runCheckIn(ctx, job, runID)
	}
	return "failed", "unknown kind of job"
}

// runAction runs a booked action as its card was approved: it's checked
// against the NAS again first, as things may have changed since.
func (s *Scheduler) runAction(ctx context.Context, job Job, runID string) (string, string) {
	action := s.tools.Action(job.Tool)
	if action == nil {
		return s.skipped(ctx, job, runID, "This helper can't do that any more.")
	}
	card, err := action.Describe(ctx, job.Arguments)
	if err != nil {
		if ctx.Err() != nil {
			return stopped, ""
		}
		// What it would act on has changed: the pool's scrubbing already,
		// or the app has no update.
		return s.skipped(ctx, job, runID, upperFirst(err.Error())+".")
	}
	args := job.Arguments
	if len(card.Args) > 0 {
		args = card.Args
	}
	// Once started, it finishes even if the helper is stopping.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), actionRunFor)
	outcome, err := action.Run(runCtx, args)
	cancel()
	s.log.Record(context.WithoutCancel(ctx), actions.Entry{
		ID: runID, Tool: job.Tool, Arguments: args, Card: card, Outcome: outcome, Err: err, Scheduled: true,
	})
	switch {
	case err != nil:
		return "failed", upperFirst(err.Error())
	case outcome.Job != nil:
		return "started", outcome.Message
	}
	return "done", outcome.Message
}

// skipped notes a run that had nothing to do, or couldn't.
func (s *Scheduler) skipped(ctx context.Context, job Job, runID, why string) (string, string) {
	s.note(ctx, runID, events.AssistantScheduled, events.SeverityInfo, "Didn't run: "+job.Title, why, job, "skipped", nil)
	return "skipped", why
}

// missed notes a job whose time passed more than lateBy ago. A one-off is
// done with; a repeating job carries on from its next time.
func (s *Scheduler) missed(ctx context.Context, job Job, now time.Time) {
	at := job.NextRun.In(locationOf(job.When)).Format("3:04 pm on Mon 2 Jan")
	why := fmt.Sprintf("It was due at %s, and didn't run within two hours of then; the helper may not have been running.", at)
	var next *time.Time
	severity := events.SeverityWarning
	if t, ok := job.When.Next(now); ok && job.When.Repeat != "once" {
		next = &t
		severity = events.SeverityInfo
	}
	recorded, err := s.store.Miss(ctx, job.ID, *job.NextRun, now, why, next)
	if err != nil || !recorded {
		if err != nil {
			slog.Error("schedule: couldn't record a missed run", "job", job.ID, "err", err)
		}
		return
	}
	s.note(ctx, job.ID+"-"+newID()[:6], entryFor(job), severity, "Missed: "+job.Title, why, job, "missed", nil)
}

// entryFor is the feed entry for a job's own notes: a rule's are the
// rule's.
func entryFor(job Job) string {
	if job.Kind == KindUpdateRule {
		return events.AssistantRule
	}
	return events.AssistantScheduled
}

// note puts an entry for a job in the activity feed, with anything extra
// the app needs in its body.
func (s *Scheduler) note(ctx context.Context, runID, entry string, severity events.Severity, title, summary string, job Job, status string, extra map[string]any) {
	if s.events == nil {
		return
	}
	detail := map[string]any{"job_id": job.ID, "kind": job.Kind, "title": job.Title, "status": status, "message": summary}
	for k, v := range extra {
		detail[k] = v
	}
	body, _ := json.Marshal(map[string]any{"assistant_schedule": detail})
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.events.Ingest(ctx, events.Event{
		Kind:      events.KindJob,
		Severity:  severity,
		Title:     title,
		Summary:   summary,
		Body:      body,
		DedupeKey: events.AssistantKey(runID, entry),
	}, 0)
	if err != nil {
		slog.Error("schedule: couldn't record an entry", "title", title, "err", err)
	}
}

func locationOf(w When) *time.Location {
	loc, err := w.location()
	if err != nil {
		return time.UTC
	}
	return loc
}

func upperFirst(s string) string {
	for i, r := range s {
		if i == 0 && r >= 'a' && r <= 'z' {
			return string(r-'a'+'A') + s[1:]
		}
		break
	}
	return s
}

// baseContext is what Run was given, or nil before it starts.
func (s *Scheduler) baseContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base
}
