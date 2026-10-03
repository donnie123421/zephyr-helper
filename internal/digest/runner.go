package digest

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

// The digest goes out on Sunday at 9 am, in the NAS's time zone.
const (
	digestWeekday = time.Sunday
	digestHour    = 9
	// lateBy is how long after its time a digest still goes out, as when
	// the helper was down at 9 am, or quiet hours ran late.
	lateBy = 24 * time.Hour
)

// Runner puts the digest together each week, keeps it and announces it.
type Runner struct {
	Sources  Sources
	Settings *SettingsStore
	// Zone is the NAS's time zone.
	Zone func(ctx context.Context) *time.Location
	// Quiet reports quiet hours, when the digest waits.
	Quiet func(time.Time) bool
	tick  time.Duration
}

// Run checks every minute until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	tick := r.tick
	if tick == 0 {
		tick = time.Minute
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		r.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// lastDue is the most recent Sunday 9 am at or before now, in loc.
func lastDue(now time.Time, loc *time.Location) time.Time {
	local := now.In(loc)
	back := (int(local.Weekday()) - int(digestWeekday) + 7) % 7
	due := time.Date(local.Year(), local.Month(), local.Day()-back, digestHour, 0, 0, 0, loc)
	if due.After(now) {
		due = time.Date(local.Year(), local.Month(), local.Day()-back-7, digestHour, 0, 0, 0, loc)
	}
	return due
}

func (r *Runner) check(ctx context.Context) {
	settings := r.Settings.Get()
	if !settings.Enabled {
		return
	}
	now := r.Sources.now()
	due := lastDue(now, r.Zone(ctx))
	if now.Sub(due) > lateBy {
		return
	}
	if last, ok := r.Sources.Store.Latest(); ok && !last.CreatedAt.Before(due) {
		return
	}
	if r.Quiet != nil && r.Quiet(now) {
		return
	}
	d, err := r.Sources.Assemble(ctx, settings.Summary)
	if err != nil {
		slog.Warn("digest: couldn't put it together", "err", err)
		return
	}
	if err := r.Sources.Store.Add(d); err != nil {
		slog.Error("digest: couldn't keep it", "err", err)
		return
	}
	r.announce(ctx, d)
}

// announce puts the digest in the feed, which notifies.
func (r *Runner) announce(ctx context.Context, d Digest) {
	summary := d.Headline
	if d.Summary != "" {
		summary = firstSentence(d.Summary)
	}
	body, _ := json.Marshal(map[string]any{"digest": map[string]any{"id": d.ID, "week_start": d.WeekStart, "week_end": d.WeekEnd}})
	_, err := r.Sources.Events.Ingest(ctx, events.Event{
		OccurredAt: d.CreatedAt,
		Kind:       events.KindJob,
		Severity:   events.SeverityInfo,
		Title:      "Your NAS this week",
		Summary:    summary,
		Body:       body,
		DedupeKey:  events.DigestKeyPrefix + d.ID,
	}, time.Hour)
	if err != nil {
		slog.Error("digest: couldn't announce it", "err", err)
	}
}

func firstSentence(text string) string {
	for i, r := range text {
		if (r == '.' || r == '!' || r == '?') && i > 20 {
			return text[:i+1]
		}
	}
	if len([]rune(text)) > 160 {
		return string([]rune(text)[:159]) + "…"
	}
	return text
}

// NASZone reads the NAS's time zone, at most hourly; UTC when it can't.
func NASZone(nas Caller) func(context.Context) *time.Location {
	var (
		mu   sync.Mutex
		loc  *time.Location
		read time.Time
	)
	return func(ctx context.Context) *time.Location {
		mu.Lock()
		defer mu.Unlock()
		if loc != nil && time.Since(read) < time.Hour {
			return loc
		}
		read = time.Now()
		loc = time.UTC
		raw, err := nas.Call(ctx, "system.general.config")
		if err != nil {
			return loc
		}
		var general struct {
			Timezone string `json:"timezone"`
		}
		if json.Unmarshal(raw, &general) == nil && general.Timezone != "" {
			if zone, err := time.LoadLocation(general.Timezone); err == nil {
				loc = zone
			}
		}
		return loc
	}
}
