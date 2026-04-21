// Package pollers ingests NAS state into the events store. Each poller
// runs as a goroutine spawned at server startup, hitting the relevant
// TrueNAS REST endpoint on a fixed cadence and translating results into
// store-shaped rows. Dedupe is handled inside events.Store.Ingest, so a
// poller's job is limited to fetch + convert + rate.
package pollers

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// DefaultAlertInterval controls how often /alert/list is polled. 60s
// matches the v1.1 plan — frequent enough that the iOS feed feels
// live, gentle enough to not dent TrueNAS CPU.
const DefaultAlertInterval = 60 * time.Second

// DefaultAlertMergeWindow is the events.Store merge window used for
// alert ingests. Two alerts with the same dedupe key occurring within
// this window collapse onto the same row.
const DefaultAlertMergeWindow = time.Hour

// Alerts polls TrueNAS /alert/list on an interval and ingests newly
// seen alerts into the events store. An in-memory set tracks alerts
// already observed by uuid + last_occurrence so a persistently-firing
// alert doesn't keep bumping count every tick — only a re-fire (bumped
// last_occurrence) or a fresh uuid causes a new ingest.
type Alerts struct {
	tn       *truenas.Client
	store    *events.Store
	interval time.Duration
	merge    time.Duration

	// seen maps alert uuid -> occurrence fingerprint. Reset on restart;
	// Store.Ingest's merge window handles the re-ingest-after-restart
	// case by folding it into the existing row.
	seen map[string]string
}

// NewAlerts builds a poller. Use DefaultAlertInterval +
// DefaultAlertMergeWindow for plan-recommended defaults.
func NewAlerts(tn *truenas.Client, store *events.Store, interval, merge time.Duration) *Alerts {
	return &Alerts{
		tn:       tn,
		store:    store,
		interval: interval,
		merge:    merge,
		seen:     make(map[string]string),
	}
}

// Run blocks until ctx is cancelled. Polls once immediately so the
// store has a populated view by the time the iOS client connects, then
// on every tick thereafter.
func (a *Alerts) Run(ctx context.Context) {
	a.poll(ctx)
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.poll(ctx)
		}
	}
}

func (a *Alerts) poll(ctx context.Context) {
	raw, err := a.tn.GetRaw(ctx, "/alert/list")
	if err != nil {
		slog.Warn("alerts: poll", "err", err)
		return
	}
	var alerts []map[string]any
	if err := json.Unmarshal(raw, &alerts); err != nil {
		slog.Warn("alerts: decode", "err", err)
		return
	}

	current := make(map[string]string, len(alerts))
	ingested := 0

	for _, al := range alerts {
		if d, _ := al["dismissed"].(bool); d {
			continue
		}
		uuid, _ := al["uuid"].(string)
		if uuid == "" {
			continue
		}
		fp := fingerprintAlert(al)
		current[uuid] = fp

		if prev, ok := a.seen[uuid]; ok && prev == fp {
			continue
		}

		ev, ok := alertToEvent(al)
		if !ok {
			continue
		}
		if _, err := a.store.Ingest(ctx, ev, a.merge); err != nil {
			slog.Warn("alerts: ingest", "err", err, "uuid", uuid)
			continue
		}
		ingested++
	}

	a.seen = current
	if ingested > 0 {
		slog.Info("alerts: ingested", "new", ingested, "active_total", len(current))
	}
}

// fingerprintAlert returns a string that changes when the alert
// re-fires. Keyed on last_occurrence, falling back to datetime.
func fingerprintAlert(a map[string]any) string {
	if t := parseTrueNASTime(a["last_occurrence"]); !t.IsZero() {
		return t.Format(time.RFC3339Nano)
	}
	if t := parseTrueNASTime(a["datetime"]); !t.IsZero() {
		return t.Format(time.RFC3339Nano)
	}
	return ""
}

// alertToEvent converts a TrueNAS /alert/list entry into an Event ready
// for Ingest. Returns ok=false when the alert lacks the fields needed
// to build a meaningful feed row (no message text at all).
func alertToEvent(a map[string]any) (events.Event, bool) {
	level, _ := a["level"].(string)
	klass, _ := a["klass"].(string)
	formatted, _ := a["formatted"].(string)
	text, _ := a["text"].(string)

	msg := firstNonEmpty(formatted, text)
	if msg == "" {
		return events.Event{}, false
	}

	title := msg
	if len(title) > 120 {
		title = title[:117] + "…"
	}

	occurred := parseTrueNASTime(a["last_occurrence"])
	if occurred.IsZero() {
		occurred = parseTrueNASTime(a["datetime"])
	}
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}

	body, err := json.Marshal(a)
	if err != nil {
		body = []byte("{}")
	}

	return events.Event{
		OccurredAt: occurred,
		Kind:       events.KindAlert,
		Severity:   severityFromAlert(level),
		Title:      title,
		Summary:    msg,
		Body:       body,
		DedupeKey:  alertDedupeKey(level, klass, formatted),
	}, true
}

// alertDedupeKey returns a plain-text key identifying "this specific
// problem on this specific object." Not hashed so the DB stays easy to
// eyeball during debugging — matches the style of the store tests.
func alertDedupeKey(level, klass, formatted string) string {
	return "alert|" + strings.ToLower(level) + "|" + klass + "|" + formatted
}

// severityFromAlert maps TrueNAS alert levels onto the helper's 3-tier
// severity. Unknown levels default to info.
func severityFromAlert(level string) events.Severity {
	switch strings.ToUpper(level) {
	case "CRITICAL", "ALERT", "EMERGENCY":
		return events.SeverityCritical
	case "ERROR", "WARNING":
		return events.SeverityWarning
	default:
		return events.SeverityInfo
	}
}

// parseTrueNASTime handles the two shapes TrueNAS actually returns:
//   - {"$date": <unix_millis>} — the 24+ Scale format
//   - RFC3339 string — older/legacy endpoints
//
// Unknown shapes return the zero Time so callers can fall back.
func parseTrueNASTime(v any) time.Time {
	switch t := v.(type) {
	case map[string]any:
		if ms, ok := t["$date"].(float64); ok {
			return time.UnixMilli(int64(ms)).UTC()
		}
	case float64:
		return time.UnixMilli(int64(t)).UTC()
	case string:
		if t == "" {
			return time.Time{}
		}
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func firstNonEmpty(xs ...string) string {
	for _, s := range xs {
		if s != "" {
			return s
		}
	}
	return ""
}
