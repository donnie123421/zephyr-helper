// Package actions keeps the activity feed's record of what the assistant
// did with the user's approval, and follows the TrueNAS jobs those actions
// leave running, so how they end lands in the feed too.
package actions

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/tools"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// followInterval is how often a followed job is checked.
const followInterval = 10 * time.Second

// maxFollow bounds how long a job is followed. A backup of a big dataset
// can run for hours; past a day, the jobs poller's record has to do.
const maxFollow = 24 * time.Hour

// forgetAfter is how long a finished job stays marked as followed, so the
// jobs poller, which keeps seeing recent jobs for a while, leaves it alone.
const forgetAfter = time.Hour

// approvedSummary says where an entry came from.
const approvedSummary = "You approved this in a chat with Zephyr."

// scheduledSummary says where a booked action's entry came from.
const scheduledSummary = "It ran on schedule, as booked in a chat with Zephyr."

// JobWaiter waits for TrueNAS jobs. *truenas.Client is one.
type JobWaiter interface {
	WaitJob(ctx context.Context, id int64, interval time.Duration) (truenas.Job, error)
}

// Log records the assistant's actions in the events store.
type Log struct {
	store *events.Store
	jobs  JobWaiter
	// ctx is the helper's lifetime: followers stop with it.
	ctx context.Context
	// interval is how often followed jobs are checked.
	interval time.Duration

	mu sync.Mutex
	// following maps a job being followed to when it can be forgotten:
	// zero while it runs.
	following map[int64]time.Time
	wg        sync.WaitGroup
}

// New builds a Log writing to store. Jobs are followed until ctx ends.
func New(ctx context.Context, store *events.Store, jobs JobWaiter) *Log {
	return &Log{store: store, jobs: jobs, ctx: ctx, interval: followInterval, following: map[int64]time.Time{}}
}

// Entry is an action the user approved and what came of running it.
type Entry struct {
	// ID is the card's.
	ID        string
	Tool      string
	Arguments json.RawMessage
	Card      tools.Card
	Outcome   tools.Outcome
	// Err is why it failed.
	Err error
	// Scheduled marks an action booked for later, running on its own.
	Scheduled bool
}

// Record adds an approved action to the activity feed, and follows the job
// it left running.
func (l *Log) Record(ctx context.Context, e Entry) {
	summary, entry := approvedSummary, events.AssistantRan
	if e.Scheduled {
		// Nobody was watching: a failure notifies.
		summary, entry = scheduledSummary, events.AssistantScheduled
	}
	ev := events.Event{
		Kind:      events.KindJob,
		Severity:  events.SeverityInfo,
		Title:     title(e.Outcome.Message),
		Summary:   summary,
		DedupeKey: events.AssistantKey(e.ID, entry),
	}
	status := "done"
	switch {
	case e.Err != nil:
		status = "failed"
		ev.Severity = events.SeverityWarning
		ev.Title = Failed(e.Card)
		ev.Summary = e.Err.Error()
	case e.Outcome.Job != nil:
		status = "started"
		ev.Summary = summary + " How it ends will show here."
	}
	ev.Body = body(e, status, ev.Summary)
	l.ingest(ctx, ev)

	// Jobs the action saw through have their entry: this one.
	for _, id := range e.Outcome.Ran {
		l.mark(id)
	}
	if id, ok := tools.FailedJobID(e.Err); ok {
		l.mark(id)
	}
	if e.Err == nil && e.Outcome.Job != nil {
		l.follow(e)
	}
}

// mark notes a finished job as recorded here, so the jobs poller leaves it.
func (l *Log) mark(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.following[id] = time.Now().Add(forgetAfter)
	l.sweep()
}

// sweep forgets jobs past their time. The caller holds mu.
func (l *Log) sweep() {
	now := time.Now()
	for id, until := range l.following {
		if !until.IsZero() && now.After(until) {
			delete(l.following, id)
		}
	}
}

// Following reports whether a job is followed here, so the jobs poller can
// leave it to the entry this writes.
func (l *Log) Following(jobID int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.following[jobID]
	if ok && !until.IsZero() && time.Now().After(until) {
		delete(l.following, jobID)
		return false
	}
	return ok
}

// Wait blocks until every follower has finished.
func (l *Log) Wait() { l.wg.Wait() }

// follow waits in the background for the job an action started, and
// records how it ended.
func (l *Log) follow(e Entry) {
	job := *e.Outcome.Job
	l.mu.Lock()
	l.sweep()
	l.following[job.ID] = time.Time{}
	l.mu.Unlock()

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer func() {
			l.mu.Lock()
			l.following[job.ID] = time.Now().Add(forgetAfter)
			l.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(l.ctx, maxFollow)
		defer cancel()
		ended, err := l.jobs.WaitJob(ctx, job.ID, l.interval)
		if err != nil || !ended.Finished() {
			// The helper is stopping, the job outlasted maxFollow, or
			// TrueNAS dropped it without saying how it went. The jobs
			// poller still records a failure it sees.
			slog.Info("actions: stopped following a job", "job", job.ID, "err", err)
			return
		}

		ending := Ending(job, ended, e.Card)
		if job.Report != nil {
			ending = job.Report(ctx, ended)
		}
		ev := events.Event{
			Kind:      events.KindJob,
			Severity:  events.SeverityInfo,
			Title:     title(ending.Title),
			Summary:   ending.Summary,
			DedupeKey: events.AssistantKey(e.ID, events.AssistantJobDone),
		}
		status := "job_done"
		if ending.Bad {
			status = "job_failed"
			ev.Severity = events.SeverityWarning
			ev.DedupeKey = events.AssistantKey(e.ID, events.AssistantJobFailed)
		}
		if ev.Title == "" {
			ev.Title = title(e.Outcome.Message)
		}
		if ev.Summary == "" {
			ev.Summary = approvedSummary
		}
		ev.Body = body(e, status, ev.Summary)
		l.ingest(ctx, ev)
	}()
}

// Ending is how a job ended, by its own state: done, or failed and why.
func Ending(job tools.Job, ended truenas.Job, card tools.Card) tools.Ending {
	if ended.Succeeded() {
		return tools.Ending{Title: job.Done, Summary: approvedSummary}
	}
	failed := job.Failed
	if failed == "" {
		failed = Failed(card)
	}
	return tools.Ending{Title: failed, Summary: ended.Failure(), Bad: true}
}

func (l *Log) ingest(ctx context.Context, ev events.Event) {
	// The entry outlives the chat that approved it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := l.store.Ingest(ctx, ev, 0); err != nil {
		slog.Error("actions: couldn't record an action", "title", ev.Title, "err", err)
	}
}

// Failed titles an action that didn't work, from its card: card titles
// start with a verb, so "Update Plex" becomes "Couldn't update Plex".
func Failed(card tools.Card) string {
	return "Couldn't " + lowerFirst(title(card.Title))
}

// title makes a sentence a title, without its closing full stop.
func title(s string) string {
	return strings.TrimSuffix(strings.TrimSpace(s), ".")
}

func lowerFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToLower(r)) + s[size:]
}

// body is the entry's detail: the card, what happened and the job.
func body(e Entry, status, message string) json.RawMessage {
	action := map[string]any{
		"id":      e.ID,
		"tool":    e.Tool,
		"title":   e.Card.Title,
		"detail":  e.Card.Detail,
		"risk":    string(e.Card.Risk),
		"status":  status,
		"message": message,
	}
	if len(e.Arguments) > 0 && json.Valid(e.Arguments) {
		action["arguments"] = e.Arguments
	}
	if e.Outcome.Job != nil {
		action["job_id"] = e.Outcome.Job.ID
	}
	data, err := json.Marshal(map[string]any{"assistant_action": action})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}
