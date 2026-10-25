package pollers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

// DefaultScanInterval is how often the pools' scans are read. A resilver
// or scrub takes hours, so a minute late is soon enough.
const DefaultScanInterval = time.Minute

// scanFreshFor is how recently a scan can have finished, without the
// watcher having seen it run, and still be news: it covers the helper
// restarting during one, and a small pool's scrub, done between polls.
const scanFreshFor = 30 * time.Minute

// scrubJobMethods are the jobs a scrub runs as: zpool.scrub.run from
// TrueNAS 26, and pool.scrub.scrub before it (and under pool.scrub, which
// the app's Scrub button calls).
var scrubJobMethods = []string{"zpool.scrub.run", "pool.scrub.scrub"}

// Caller is what the watcher needs of TrueNAS. *truenas.Client is one.
type Caller interface {
	Call(ctx context.Context, method string, params ...any) (json.RawMessage, error)
}

// Scans notes each resilver and scrub that finishes, from the pools'
// scans. TrueNAS raises no alert for either, and a resilver isn't a job. A
// scrub is, but its job has changed name between versions, and from 26
// a scheduled scrub that isn't due yet ends its job in success too.
type Scans struct {
	tn       Caller
	store    *events.Store
	interval time.Duration
	now      func() time.Time

	// Skip, when set, names jobs recorded elsewhere: a scrub the
	// assistant started gets its own entry, from the job it started.
	Skip func(id int64) bool

	// running are the scans seen running; noted, each one's last finish
	// already recorded, by its end time. Both are keyed by scanKey.
	running map[string]bool
	noted   map[string]int64
}

// NewScans builds a watcher polling every interval.
func NewScans(tn Caller, store *events.Store, interval time.Duration) *Scans {
	return &Scans{tn: tn, store: store, interval: interval, now: time.Now,
		running: map[string]bool{}, noted: map[string]int64{}}
}

// Run polls until ctx ends.
func (s *Scans) Run(ctx context.Context) {
	s.poll(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.poll(ctx)
		}
	}
}

type scanState struct {
	Function  string `json:"function"`
	State     string `json:"state"`
	StartTime any    `json:"start_time"`
	EndTime   any    `json:"end_time"`
	Errors    int64  `json:"errors"`
}

// scanKey keeps a pool's resilvers and scrubs apart.
func scanKey(function, pool string) string {
	return function + "|" + pool
}

func (s *Scans) poll(ctx context.Context) {
	raw, err := s.tn.Call(ctx, "pool.query")
	if err != nil {
		slog.Warn("scans: poll", "err", err)
		return
	}
	var pools []struct {
		Name string    `json:"name"`
		Scan scanState `json:"scan"`
	}
	if err := json.Unmarshal(raw, &pools); err != nil {
		slog.Warn("scans: decode", "err", err)
		return
	}
	for _, p := range pools {
		function := strings.ToUpper(p.Scan.Function)
		if function != "RESILVER" && function != "SCRUB" {
			continue
		}
		key := scanKey(function, p.Name)
		switch strings.ToUpper(p.Scan.State) {
		case "SCANNING":
			s.running[key] = true
		case "FINISHED":
			ended := scanTime(p.Scan.EndTime)
			if ended.IsZero() || s.noted[key] == ended.UnixMilli() {
				continue
			}
			// One seen running, or one that finished while the helper
			// was restarting; not one long over when it started.
			news := s.running[key] || s.now().Sub(ended) < scanFreshFor
			s.noted[key] = ended.UnixMilli()
			delete(s.running, key)
			if !news {
				continue
			}
			if function == "RESILVER" {
				s.noteResilver(ctx, p.Name, p.Scan, ended)
			} else if !s.assistantScrub(ctx, p.Name, ended) {
				s.noteScrub(ctx, p.Name, p.Scan, ended)
			}
		}
	}
}

func (s *Scans) noteResilver(ctx context.Context, pool string, scan scanState, ended time.Time) {
	body, _ := json.Marshal(map[string]any{"pool": pool, "scan": scan})
	ev := events.Event{
		OccurredAt: ended,
		Kind:       events.KindResilver,
		Severity:   events.SeverityInfo,
		Title:      "Resilver of " + pool + " finished",
		Summary:    pool + " has its redundancy back.",
		Body:       body,
		DedupeKey:  fmt.Sprintf("%s%s|%d", events.RebuiltKeyPrefix, pool, ended.UnixMilli()),
	}
	if scan.Errors > 0 {
		ev.Severity = events.SeverityWarning
		ev.Summary = fmt.Sprintf("%s finished rebuilding, with %d errors: check it in the Storage tab.", pool, scan.Errors)
	}
	s.ingest(ctx, ev, pool)
}

// noteScrub records a scrub that finished. One that found errors is a
// warning, which notifies.
func (s *Scans) noteScrub(ctx context.Context, pool string, scan scanState, ended time.Time) {
	body, _ := json.Marshal(map[string]any{"pool": pool, "scan": scan})
	ev := events.Event{
		OccurredAt: ended,
		Kind:       events.KindScrub,
		Severity:   events.SeverityInfo,
		Title:      "Scrub of " + pool + " finished",
		Summary:    "The scrub found no errors on " + pool + ".",
		Body:       body,
		DedupeKey:  fmt.Sprintf("scrub-finished|%s|%d", pool, ended.UnixMilli()),
	}
	if scan.Errors > 0 {
		noun := "errors"
		if scan.Errors == 1 {
			noun = "error"
		}
		ev.Severity = events.SeverityWarning
		ev.Title = fmt.Sprintf("Scrub of %s found %d %s", pool, scan.Errors, noun)
		ev.Summary = "Check the pool in the Storage tab: ZFS repairs what it can from the pool's redundancy."
	}
	s.ingest(ctx, ev, pool)
}

func (s *Scans) ingest(ctx context.Context, ev events.Event, pool string) {
	if _, err := s.store.Ingest(ctx, ev, 24*time.Hour); err != nil {
		slog.Warn("scans: ingest", "err", err, "pool", pool, "kind", ev.Kind)
	}
}

// assistantScrub reports whether the scrub of pool that ended at ended ran
// in a job the assistant started: how that went has its own entry. If the
// jobs can't be read, it's noted here regardless; two entries for one scrub
// beat none.
func (s *Scans) assistantScrub(ctx context.Context, pool string, ended time.Time) bool {
	if s.Skip == nil {
		return false
	}
	raw, err := s.tn.Call(ctx, "core.get_jobs",
		[]any{[]any{"method", "in", scrubJobMethods}},
		map[string]any{"order_by": []string{"-id"}, "limit": 50})
	if err != nil {
		slog.Warn("scans: scrub jobs", "err", err, "pool", pool)
		return false
	}
	var jobs []struct {
		ID           int64 `json:"id"`
		Arguments    []any `json:"arguments"`
		TimeStarted  any   `json:"time_started"`
		TimeFinished any   `json:"time_finished"`
	}
	if err := json.Unmarshal(raw, &jobs); err != nil {
		slog.Warn("scans: decode scrub jobs", "err", err)
		return false
	}
	for _, j := range jobs {
		if scrubJobPool(j.Arguments) != pool || !s.Skip(j.ID) {
			continue
		}
		// The scrub's own job started before it ended and lasted until
		// then: a scrub's job ends with it. One the assistant cancelled or
		// paused earlier is another scrub's.
		started, finished := scanTime(j.TimeStarted), scanTime(j.TimeFinished)
		if (started.IsZero() || !started.After(ended)) &&
			(finished.IsZero() || !finished.Before(ended.Add(-time.Minute))) {
			return true
		}
	}
	return false
}

// scrubJobPool reads which pool a scrub job is for: zpool.scrub.run takes
// {"pool_name": …}, and pool.scrub.scrub the name.
func scrubJobPool(args []any) string {
	if len(args) == 0 {
		return ""
	}
	switch a := args[0].(type) {
	case string:
		return a
	case map[string]any:
		name, _ := a["pool_name"].(string)
		return name
	}
	return ""
}

// scanTime reads a scan's time: {"$date": ms}, or RFC 3339.
func scanTime(v any) time.Time {
	switch x := v.(type) {
	case map[string]any:
		if ms, ok := x["$date"].(float64); ok {
			return time.UnixMilli(int64(ms)).UTC()
		}
	case string:
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
