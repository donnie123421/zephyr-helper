package pollers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
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

	// Skip, when set, names jobs recorded elsewhere: the ones the
	// assistant started, which get their own entries.
	Skip func(id int64) bool

	// seen maps job id -> terminal state we last observed. Replaced
	// each poll with whatever the current response contains, so the
	// map is bounded by the API's returned window rather than lifetime
	// job count. Jobs that age out of the window have already been
	// ingested on an earlier tick (or they never mattered).
	seen map[int64]string
	// running are the jobs last seen running or waiting. One that leaves
	// the newest hundred before it ends (a scrub that takes days) is
	// asked for by id, so its end isn't missed.
	running map[int64]bool
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
		running:  make(map[int64]bool),
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

// jobsPath asks for the newest 100 jobs. TrueNAS keeps up to 1,000, oldest
// first, so without order_by the limit took the oldest: once 100 jobs had
// run since TrueNAS started, a day or two on most NASes, no new job was
// ever seen. NASes don't finish 100 jobs in a 2-minute poll.
const jobsPath = "/core/get_jobs?limit=100&order_by=-id"

func (j *Jobs) poll(ctx context.Context) {
	raw, err := j.tn.GetRaw(ctx, jobsPath)
	if err != nil {
		slog.Warn("jobs: poll", "err", err)
		return
	}
	var jobs []map[string]any
	if err := json.Unmarshal(raw, &jobs); err != nil {
		slog.Warn("jobs: decode", "err", err)
		return
	}
	jobs, unfetched := j.withDeparted(ctx, jobs)
	// Oldest first. The store merges an event into a same-kind row a few
	// minutes either side, keeping the newest; in the newest-first order
	// TrueNAS now answers in, each older job merged into the row after it.
	sort.SliceStable(jobs, func(a, b int) bool {
		ida, _ := readJobID(jobs[a])
		idb, _ := readJobID(jobs[b])
		return ida < idb
	})

	current := make(map[int64]string, len(jobs))
	running := make(map[int64]bool, len(unfetched))
	for _, id := range unfetched {
		running[id] = true
	}
	ingested := 0

	for _, job := range jobs {
		id, ok := readJobID(job)
		if !ok {
			continue
		}
		state, _ := job["state"].(string)
		upper := strings.ToUpper(state)
		if !isTerminalJobState(upper) {
			running[id] = true
			continue
		}
		if prev, already := j.seen[id]; already && prev == upper {
			current[id] = upper
			continue
		}
		if j.Skip != nil && j.Skip(id) {
			current[id] = upper
			continue
		}

		ev, ok := jobToEvent(job)
		if !ok {
			// Filtered out on purpose (routine success, unknown method) —
			// record it so the same job isn't re-evaluated every tick.
			current[id] = upper
			continue
		}
		if _, err := j.store.Ingest(ctx, ev, j.merge); err != nil {
			// Not recorded: the job's terminal state is final, so marking it
			// seen after a failed ingest would drop it for good. Retry next
			// poll instead — Ingest's merge window absorbs the duplicate.
			slog.Warn("jobs: ingest", "err", err, "id", id)
			continue
		}
		current[id] = upper
		ingested++
	}

	j.seen = current
	j.running = running
	if ingested > 0 {
		slog.Info("jobs: ingested", "new", ingested, "terminal_window", len(current))
	}
}

// withDeparted adds the jobs last seen running that have left the window,
// asked for by id. It returns the ids it couldn't ask about, to try again.
func (j *Jobs) withDeparted(ctx context.Context, jobs []map[string]any) ([]map[string]any, []int64) {
	inWindow := make(map[int64]bool, len(jobs))
	for _, job := range jobs {
		if id, ok := readJobID(job); ok {
			inWindow[id] = true
		}
	}
	var departed []int64
	for id := range j.running {
		if !inWindow[id] {
			departed = append(departed, id)
		}
	}
	if len(departed) == 0 {
		return jobs, nil
	}
	sort.Slice(departed, func(a, b int) bool { return departed[a] < departed[b] })
	raw, err := j.tn.Call(ctx, "core.get_jobs", []any{[]any{"id", "in", departed}})
	if err != nil {
		slog.Warn("jobs: follow departed", "err", err, "ids", departed)
		return jobs, departed
	}
	var more []map[string]any
	if err := json.Unmarshal(raw, &more); err != nil {
		return jobs, departed
	}
	// A job TrueNAS no longer has (restarted, or past its thousand) is
	// gone for good.
	return append(jobs, more...), nil
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

// jobSuccessIsSurfaceworthy reports whether a SUCCESSFUL job is worth a feed
// row. TrueNAS fires dozens of internal/config/boot jobs at every startup
// (smb.configure, service.control, directoryservices.setup, init scripts,
// dataset unlocks, …) that all succeed and mean nothing to a user. Denylisting
// them is a losing game against the sheer variety of middleware methods, so we
// invert it: surface a success only when it is either
//
//	(a) consumed by a correlator, which rolls the raw job into a richer parent
//	    row (scrub completion, snapshot/replication rollups, resilver) and hides
//	    the child — drop it here and the correlator never sees its child; or
//	(b) a user-meaningful standalone operation worth an archive entry.
//
// FAILED/ABORTED jobs never reach here — the caller surfaces every non-success
// terminal state regardless of method, because a failure is always actionable.
func jobSuccessIsSurfaceworthy(method string) bool {
	// TrueNAS emits both the public method and its private *_impl as separate
	// jobs, and bulk fan-out jobs alongside their work items — both
	// double-count, so never surface them (checked first so e.g.
	// zettarepl.*_impl doesn't slip through the prefix match below).
	if strings.HasSuffix(method, "_impl") ||
		strings.HasSuffix(method, ".bulk") || strings.Contains(method, ".bulk_") {
		return false
	}
	// (a) Correlator-consumed methods. These must keep flowing or the scrub /
	// snapshot / disk correlators never see their child event and can't emit
	// the narrative parent. Keep in sync with internal/events/correlators/*.
	if method == "pool.scrub" ||
		strings.HasPrefix(method, "pool.snapshottask.") ||
		strings.HasPrefix(method, "zettarepl.") ||
		strings.Contains(method, "resilver") {
		return true
	}
	// (b) Meaningful standalone operations with no correlator of their own:
	// system updates installed, backup / replication / sync tasks completing,
	// and pool lifecycle milestones.
	switch method {
	case "update.run", "update.update",
		"replication.run", "replication.run_onetime",
		"cloudsync.sync", "cloud_backup.sync", "rsynctask.run",
		"pool.create", "pool.export", "pool.import_pool", "pool.expand":
		return true
	}
	return false
}

// jobToEvent builds a store row from a /core/get_jobs entry in a
// terminal state. Unknown/missing state or method → skipped. A SUCCESS is
// surfaced only when jobSuccessIsSurfaceworthy allows it (correlator-fed or
// user-meaningful) — the flood of routine internal successes TrueNAS fires at
// boot is dropped. FAILED/ABORTED always surface: a failure is actionable
// regardless of method.
func jobToEvent(job map[string]any) (events.Event, bool) {
	method, _ := job["method"].(string)
	state, _ := job["state"].(string)
	upper := strings.ToUpper(state)
	if method == "" || !isTerminalJobState(upper) {
		return events.Event{}, false
	}
	if upper == "SUCCESS" && !jobSuccessIsSurfaceworthy(method) {
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
		DedupeKey:  jobDedupeKey(method, upper, job),
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
func jobDedupeKey(method, state string, job map[string]any) string {
	key := "job|" + method + "|" + strings.ToLower(state)
	// Two backup tasks finishing within minutes of each other (both
	// scheduled at midnight) are two events, not one.
	if taskJobMethods[method] {
		if args, ok := job["arguments"].([]any); ok && len(args) > 0 {
			if id, ok := args[0].(float64); ok && id > 0 {
				key += fmt.Sprintf("|%d", int64(id))
			}
		}
	}
	return key
}

// taskJobMethods run a task given by id as their first argument.
var taskJobMethods = map[string]bool{
	"replication.run": true, "cloudsync.sync": true, "cloud_backup.sync": true, "rsynctask.run": true,
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
