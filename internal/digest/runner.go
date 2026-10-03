package digest

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
	"unicode"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

// The digest goes out on Sunday at 9 am, in the NAS's time zone.
const (
	digestWeekday = time.Sunday
	digestHour    = 9
	// lateBy is how long after its time a digest still goes out, as when
	// the helper was down at 9 am, or quiet hours ran late.
	lateBy = 24 * time.Hour
	// minGap is the least time between two digests.
	minGap = 6 * 24 * time.Hour
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
	// unsaved is a digest put together that couldn't be kept, so a retry
	// doesn't ask the model again.
	unsaved *Digest
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
	quiet := r.Quiet != nil && r.Quiet(now)
	// A digest kept whose notification waited for quiet hours to end, or
	// for the helper to come back, goes first: the flag is kept with it.
	if last, ok := r.Sources.Store.Latest(); ok && !last.Announced && now.Sub(last.CreatedAt) < lateBy {
		if !quiet {
			r.announce(ctx, last)
		}
		return
	}
	// Without the NAS's time zone there's no knowing when Sunday 9 am is:
	// wait for it, rather than guess.
	loc := r.Zone(ctx)
	if loc == nil {
		return
	}
	due := lastDue(now, loc)
	if now.Sub(due) > lateBy {
		return
	}
	// Switched on after this Sunday's: the first is next Sunday's.
	if settings.EnabledAt != nil && settings.EnabledAt.After(due) {
		return
	}
	// One a week, even if the time zone changes under it.
	if last, ok := r.Sources.Store.Latest(); ok && (!last.CreatedAt.Before(due) || now.Sub(last.CreatedAt) < minGap) {
		return
	}
	if quiet {
		return
	}
	var d Digest
	if r.unsaved != nil && !r.unsaved.CreatedAt.Before(due) {
		d = *r.unsaved
	} else {
		var err error
		if d, err = r.Sources.Assemble(ctx, settings.Summary); err != nil {
			slog.Warn("digest: couldn't put it together", "err", err)
			return
		}
	}
	if err := r.Sources.Store.Add(d); err != nil {
		// Logged once, not every minute of the day it's retried.
		if r.unsaved == nil || r.unsaved.ID != d.ID {
			slog.Error("digest: couldn't keep it", "err", err)
		}
		r.unsaved = &d
		return
	}
	r.unsaved = nil
	// Putting it together can take a couple of minutes, with a model: if
	// quiet hours began meanwhile, the notification waits for them.
	if r.Quiet != nil && r.Quiet(r.Sources.now()) {
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
		return
	}
	if err := r.Sources.Store.MarkAnnounced(d.ID); err != nil {
		slog.Error("digest: couldn't note it was announced", "err", err)
	}
}

// firstSentence is the summary's first sentence, for the notification: it
// ends at a full stop, question or exclamation mark followed by a space
// or the end, so "grew 350.0 GB" and "1.41.3" stay whole.
func firstSentence(text string) string {
	runes := []rune(text)
	for i, r := range runes {
		if (r == '.' || r == '!' || r == '?') && i > 20 && (i+1 == len(runes) || unicode.IsSpace(runes[i+1])) {
			return string(runes[:i+1])
		}
	}
	if len(runes) > 160 {
		return string(runes[:159]) + "…"
	}
	return text
}

// NASZone reads the NAS's time zone, at most hourly. It keeps the last
// one read when TrueNAS doesn't answer, and is nil until one has been.
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
		raw, err := nas.Call(ctx, "system.general.config")
		if err != nil {
			return loc
		}
		var general struct {
			Timezone string `json:"timezone"`
		}
		if json.Unmarshal(raw, &general) != nil || general.Timezone == "" {
			return loc
		}
		zone, err := time.LoadLocation(general.Timezone)
		if err != nil {
			slog.Warn("digest: unknown time zone", "zone", general.Timezone, "err", err)
			return loc
		}
		loc, read = zone, time.Now()
		return loc
	}
}
