package pollers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// DefaultJobInterval is the cadence for /core/get_jobs. 2 min is the
// plan-recommended default — long-running jobs don't need sub-minute
// polling, and most job volume is recurring schedules that benefit
// more from dedupe than from freshness.
const DefaultJobInterval = 2 * time.Minute

// DefaultJobMergeWindow collapses rapid retries of the same task onto
// one row. A repeated cron-triggered job outside this window creates
// a new row because the occurrence is far enough apart to be a
// distinct event in the feed.
const DefaultJobMergeWindow = 5 * time.Minute

// Jobs polls TrueNAS /core/get_jobs and ingests terminal-state
// transitions (SUCCESS, FAILED, ABORTED). Running or waiting jobs are
// intentionally skipped — the feed is an archive of what happened, not
// a live progress view.
type Jobs struct {
	tn       *truenas.Client
	store    *events.Store
	interval time.Duration
	merge    time.Duration

	// seen maps job id -> terminal state we last observed. Replaced
	// each poll with whatever the current response contains, so the
	// map is bounded by the API's returned window rather than lifetime
	// job count. Jobs that age out of the window have already been
	// ingested on an earlier tick (or they never mattered).
	seen map[int64]string
}

// NewJobs builds a poller. Use DefaultJobInterval + DefaultJobMergeWindow
// for plan defaults.
func NewJobs(tn *truenas.Client, store *events.Store, interval, merge time.Duration) *Jobs {
	return &Jobs{
		tn:       tn,
		store:    store,
		interval: interval,
		merge:    merge,
		seen:     make(map[int64]string),
	}
}

// Run blocks until ctx is cancelled. First poll is immediate so newly
// booted helpers catch recent terminal jobs before the first tick.
func (j *Jobs) Run(ctx context.Context) {
	j.poll(ctx)
	t := time.NewTicker(j.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.poll(ctx)
		}
	}
}

func (j *Jobs) poll(ctx context.Context) {
	// limit=100 bounds a single poll; NASes don't normally finish 100
	// jobs in a 2-minute window, so this is generous padding.
	raw, err := j.tn.GetRaw(ctx, "/core/get_jobs?limit=100")
	if err != nil {
		slog.Warn("jobs: poll", "err", err)
		return
	}
	var jobs []map[string]any
	if err := json.Unmarshal(raw, &jobs); err != nil {
		slog.Warn("jobs: decode", "err", err)
		return
	}

	current := make(map[int64]string, len(jobs))
	ingested := 0

	for _, job := range jobs {
		id, ok := readJobID(job)
		if !ok {
			continue
		}
		state, _ := job["state"].(string)
		upper := strings.ToUpper(state)
		if !isTerminalJobState(upper) {
			continue
		}
		current[id] = upper

		if prev, already := j.seen[id]; already && prev == upper {
			continue
		}

		ev, ok := jobToEvent(job)
		if !ok {
			continue
		}
		if _, err := j.store.Ingest(ctx, ev, j.merge); err != nil {
			slog.Warn("jobs: ingest", "err", err, "id", id)
			continue
		}
		ingested++
	}

	j.seen = current
	if ingested > 0 {
		slog.Info("jobs: ingested", "new", ingested, "terminal_window", len(current))
	}
}

func readJobID(job map[string]any) (int64, bool) {
	switch v := job["id"].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	}
	return 0, false
}

func isTerminalJobState(state string) bool {
	switch state {
	case "SUCCESS", "FAILED", "ABORTED":
		return true
	}
	return false
}

// jobMethodIsNoise filters scheduled/infrastructure methods that finish
// successfully in the background and don't represent anything the user
// cares to see in a feed. Failures for these still surface because a
// broken cache refresh or failed auto-update is actionable.
//
// Any `*_impl` method is dropped because TrueNAS emits both the public
// method and its private implementation as separate jobs — ingesting
// both double-counts the same work.
func jobMethodIsNoise(method string) bool {
	if strings.HasSuffix(method, "_impl") {
		return true
	}
	switch method {
	case
		"directoryservices.cache_refresh",
		"update.check_available",
		"update.download",
		"app.pull_images",
		"app.redeploy",
		"app.start",
		"app.stop",
		"core.get_jobs":
		return true
	}
	return false
}

// jobToEvent builds a store row from a /core/get_jobs entry in a
// terminal state. Unknown/missing state or method → skipped. Successful
// low-signal methods (scheduled cache refreshes, auto update downloads,
// app lifecycle calls the UI already reflects) are also skipped —
// failures for those methods still surface because a broken schedule
// is something the user needs to see.
func jobToEvent(job map[string]any) (events.Event, bool) {
	method, _ := job["method"].(string)
	state, _ := job["state"].(string)
	upper := strings.ToUpper(state)
	if method == "" || !isTerminalJobState(upper) {
		return events.Event{}, false
	}
	if upper == "SUCCESS" && jobMethodIsNoise(method) {
		return events.Event{}, false
	}

	occurred := parseTrueNASTime(job["time_finished"])
	if occurred.IsZero() {
		occurred = parseTrueNASTime(job["time_started"])
	}
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}

	title := jobTitle(method, upper)
	summary := title
	if upper == "FAILED" {
		if errStr, _ := job["error"].(string); errStr != "" {
			summary = fmt.Sprintf("%s: %s", title, firstLine(errStr))
		}
	}
	if len(summary) > 240 {
		summary = summary[:237] + "…"
	}

	body, err := json.Marshal(job)
	if err != nil {
		body = []byte("{}")
	}

	return events.Event{
		OccurredAt: occurred,
		Kind:       events.KindJob,
		Severity:   jobSeverity(upper),
		Title:      title,
		Summary:    summary,
		Body:       body,
		DedupeKey:  jobDedupeKey(method, upper),
	}, true
}

func jobTitle(method, state string) string {
	switch state {
	case "SUCCESS":
		return method + " succeeded"
	case "FAILED":
		return method + " failed"
	case "ABORTED":
		return method + " aborted"
	default:
		return method + " " + strings.ToLower(state)
	}
}

func jobSeverity(state string) events.Severity {
	switch state {
	case "FAILED", "ABORTED":
		return events.SeverityWarning
	default:
		return events.SeverityInfo
	}
}

// jobDedupeKey collapses rapid retries of the same method in the same
// terminal state onto one row (within the 5-min merge window). The
// same scheduled task running tomorrow gets a fresh row because it's
// outside the window, which is what we want for an archive.
func jobDedupeKey(method, state string) string {
	return "job|" + method + "|" + strings.ToLower(state)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
