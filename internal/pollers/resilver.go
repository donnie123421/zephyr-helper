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

// DefaultResilverInterval is how often the pools' scans are read. A
// resilver takes hours, so a minute late is soon enough.
const DefaultResilverInterval = time.Minute

// resilverFreshFor is how recently a resilver can have finished, without
// the watcher having seen it run, and still be news: it covers the helper
// restarting during one.
const resilverFreshFor = 30 * time.Minute

// Caller is what the watcher needs of TrueNAS. *truenas.Client is one.
type Caller interface {
	Call(ctx context.Context, method string, params ...any) (json.RawMessage, error)
}

// Resilvers notes each resilver that finishes. TrueNAS raises no alert for
// it, and a resilver isn't a job, so it watches the pools' scans.
type Resilvers struct {
	tn       Caller
	store    *events.Store
	interval time.Duration
	now      func() time.Time
	// running are the pools seen resilvering; noted, each pool's last
	// finish already recorded, by its end time.
	running map[string]bool
	noted   map[string]int64
}

// NewResilvers builds a watcher polling every interval.
func NewResilvers(tn Caller, store *events.Store, interval time.Duration) *Resilvers {
	return &Resilvers{tn: tn, store: store, interval: interval, now: time.Now,
		running: map[string]bool{}, noted: map[string]int64{}}
}

// Run polls until ctx ends.
func (r *Resilvers) Run(ctx context.Context) {
	r.poll(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.poll(ctx)
		}
	}
}

type scanState struct {
	Function string `json:"function"`
	State    string `json:"state"`
	EndTime  any    `json:"end_time"`
	Errors   int64  `json:"errors"`
}

func (r *Resilvers) poll(ctx context.Context) {
	raw, err := r.tn.Call(ctx, "pool.query")
	if err != nil {
		slog.Warn("resilvers: poll", "err", err)
		return
	}
	var pools []struct {
		Name string    `json:"name"`
		Scan scanState `json:"scan"`
	}
	if err := json.Unmarshal(raw, &pools); err != nil {
		slog.Warn("resilvers: decode", "err", err)
		return
	}
	for _, p := range pools {
		if !strings.EqualFold(p.Scan.Function, "RESILVER") {
			continue
		}
		switch strings.ToUpper(p.Scan.State) {
		case "SCANNING":
			r.running[p.Name] = true
		case "FINISHED":
			ended := scanTime(p.Scan.EndTime)
			if ended.IsZero() || r.noted[p.Name] == ended.UnixMilli() {
				continue
			}
			// One seen running, or one that finished while the helper
			// was restarting; not one long over when it started.
			news := r.running[p.Name] || r.now().Sub(ended) < resilverFreshFor
			r.noted[p.Name] = ended.UnixMilli()
			delete(r.running, p.Name)
			if news {
				r.note(ctx, p.Name, p.Scan, ended)
			}
		}
	}
}

func (r *Resilvers) note(ctx context.Context, pool string, scan scanState, ended time.Time) {
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
	if _, err := r.store.Ingest(ctx, ev, 24*time.Hour); err != nil {
		slog.Warn("resilvers: ingest", "err", err, "pool", pool)
	}
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
