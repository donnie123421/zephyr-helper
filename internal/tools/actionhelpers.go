package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// How long actions wait on TrueNAS. Variables so tests can shorten them.
var (
	// shortJobWait is how long an action waits for a quick job (an app
	// or a service starting) to finish, so its card can say it's done
	// rather than started. A job still running then is followed into the
	// activity feed.
	shortJobWait = 90 * time.Second
	// quickCheck is how long an action waits on a long job (a backup, an
	// app update) before calling it started: long enough to catch a
	// refusal, such as a task that's already running.
	quickCheck = 3 * time.Second
	// jobPoll is how often an action checks a job it's waiting on.
	jobPoll = time.Second
	// scanCheck is how often a scrub started on 26 is checked for.
	scanCheck = time.Second
	// guestSecond turns a VM's or container's shutdown timeout into a
	// wait, and shutdownGrace is the margin on top.
	guestSecond   = time.Second
	shutdownGrace = 30 * time.Second
)

// jobError is a job that failed while an action waited on it. Its id lets
// the activity feed keep one entry for the failure, not the action's and
// the jobs poller's.
type jobError struct {
	id     int64
	reason string
}

func (e jobError) Error() string { return e.reason }

// JobFailure is the error for TrueNAS job id failing with reason.
func JobFailure(id int64, reason string) error { return jobError{id: id, reason: reason} }

// FailedJobID is the TrueNAS job behind an action's error, when a job
// failed.
func FailedJobID(err error) (int64, bool) {
	var failed jobError
	if errors.As(err, &failed) {
		return failed.id, true
	}
	return 0, false
}

// decodeArgs reads the model's arguments into v. Empty arguments leave v
// as it is.
func decodeArgs(args json.RawMessage, v any) error {
	if len(args) == 0 || string(args) == "null" {
		return nil
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}

// call makes a TrueNAS call, reporting a failure as TrueNAS put it.
func call(ctx context.Context, tn *truenas.Client, method string, params ...any) (json.RawMessage, error) {
	raw, err := tn.Call(ctx, method, params...)
	if err != nil {
		return nil, reasonError{reason: truenas.Reason(err), err: err}
	}
	return raw, nil
}

// reasonError reads as what TrueNAS said went wrong, and keeps the
// original error, so truenas.IsMethodNotFound still sees through it.
type reasonError struct {
	reason string
	err    error
}

func (e reasonError) Error() string { return e.reason }
func (e reasonError) Unwrap() error { return e.err }

// query runs a .query method with filters, decoding the rows.
func query(ctx context.Context, tn *truenas.Client, method string, filters []any) ([]map[string]any, error) {
	if filters == nil {
		filters = []any{}
	}
	raw, err := tn.Call(ctx, method, filters)
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("decode %s: %w", method, err)
	}
	return rows, nil
}

// jobRun is how a job an action started went: done in time, or still
// running and left to the activity feed.
type jobRun struct {
	id       int64
	finished bool
}

// runJob starts a job with method and waits up to wait for it. A job that
// fails in that time is an error.
func runJob(ctx context.Context, tn *truenas.Client, wait time.Duration, method string, params ...any) (jobRun, error) {
	raw, err := call(ctx, tn, method, params...)
	if err != nil {
		return jobRun{}, err
	}
	id, err := truenas.JobID(raw)
	if err != nil {
		return jobRun{}, fmt.Errorf("%s: %w", method, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	job, err := tn.WaitJob(waitCtx, id, jobPoll)
	if err != nil || !job.Finished() {
		return jobRun{id: id}, nil
	}
	if !job.Succeeded() {
		return jobRun{id: id}, jobError{id: id, reason: job.Failure()}
	}
	return jobRun{id: id, finished: true}, nil
}

// outcome reports a job an action started: done when it finished in time,
// otherwise started, with the job left for the activity feed.
func (r jobRun) outcome(done, started, doneTitle, failedTitle string) Outcome {
	if r.finished {
		return Outcome{Message: done, Ran: []int64{r.id}}
	}
	return Outcome{Message: started, Job: &Job{ID: r.id, Done: doneTitle, Failed: failedTitle}}
}

// plain makes a name from TrueNAS fit for a card: control characters
// and line separators become spaces, and a very long one loses its
// middle, keeping the start and the end that tell it from its neighbours.
func plain(s string) string {
	s = strings.Join(strings.Fields(strings.Map(cardRune, s)), " ")
	const keep = 70
	if runes := []rune(s); len(runes) > 2*keep+1 {
		s = string(runes[:keep]) + "…" + string(runes[len(runes)-keep:])
	}
	return s
}

// cardRune keeps text from TrueNAS from misleading on a card: control
// characters and line separators become spaces, and format characters,
// which can reverse how a line reads (bidi overrides) or hide inside a
// word (zero-width spaces), are dropped.
func cardRune(r rune) rune {
	switch {
	case unicode.IsControl(r) || r == '\u2028' || r == '\u2029':
		return ' '
	case unicode.Is(unicode.Cf, r):
		return -1
	}
	return r
}

// quoted is a name from TrueNAS in quotes, for a card.
func quoted(s string) string { return "“" + plain(s) + "”" }

// firstLine is the start of a message from TrueNAS, for a card.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.Join(strings.Fields(strings.Map(cardRune, s)), " ")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max-1]) + "…"
	}
	return s
}

// ago says how long ago t was, roughly.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 2*time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "an hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// trueNASTime reads a TrueNAS timestamp: {"$date": ms} from datastore
// rows, or epoch seconds or milliseconds as a number.
func trueNASTime(v any) (time.Time, bool) {
	switch x := v.(type) {
	case map[string]any:
		if ms, ok := x["$date"].(float64); ok {
			return time.UnixMilli(int64(ms)), true
		}
	case float64:
		if x > 1e12 {
			return time.UnixMilli(int64(x)), true
		}
		if x > 0 {
			return time.Unix(int64(x), 0), true
		}
	}
	return time.Time{}, false
}

// str reads a string field, "" when absent.
func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// num reads a numeric field.
func num(m map[string]any, key string) (int64, bool) {
	switch x := m[key].(type) {
	case float64:
		return int64(x), true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	}
	return 0, false
}

// bytesText writes a size the way TrueNAS's UI does, in binary units.
func bytesText(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %siB", float64(n)/float64(div), string("KMGTPE"[exp]))
}
