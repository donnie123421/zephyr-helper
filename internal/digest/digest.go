// Package digest puts together the weekly digest: a week on the NAS, what
// changed and what happened, with what wants a look first. It's assembled
// each Sunday morning in the NAS's time zone, kept, and announced with a
// notification. A connected model can add a short summary on top; without
// one, it's just the facts.
package digest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/capacity"
	"github.com/donnie123421/zephyr-helper/internal/events"
)

// Digest is one week on the NAS.
type Digest struct {
	ID        string    `json:"id"`
	WeekStart time.Time `json:"week_start"`
	WeekEnd   time.Time `json:"week_end"`
	CreatedAt time.Time `json:"created_at"`
	// Preview: the week so far, put together on request, not kept.
	Preview bool `json:"preview,omitempty"`
	// Headline is one line from the facts, for the notification.
	Headline string `json:"headline"`
	// Summary is the model's short take, when one is connected.
	Summary   string     `json:"summary,omitempty"`
	Pools     []PoolWeek `json:"pools"`
	Scrubs    []Item     `json:"scrubs"`
	Disks     []Item     `json:"disks"`
	Alerts    Alerts     `json:"alerts"`
	Backups   Backups    `json:"backups"`
	Apps      Apps       `json:"apps"`
	Assistant Assistant  `json:"assistant"`
	Security  []Item     `json:"security"`
	System    []Item     `json:"system"`
	// AppVersions are the apps' versions as of this digest, for the next
	// one to see what was updated in between.
	AppVersions map[string]string `json:"app_versions,omitempty"`
}

// PoolWeek is a pool's space, how it changed this week, and its forecast.
type PoolWeek struct {
	Pool      string  `json:"pool"`
	Status    string  `json:"status"`
	Size      int64   `json:"size"`
	Allocated int64   `json:"allocated"`
	Percent   float64 `json:"percent"`
	// Change is the space used now less a week ago, when that's known.
	Change      int64      `json:"change"`
	ChangeKnown bool       `json:"change_known"`
	Trend       string     `json:"trend"`
	PerDay      float64    `json:"per_day"`
	At80        *time.Time `json:"at_80,omitempty"`
	At100       *time.Time `json:"at_100,omitempty"`
}

// Item is one thing that happened.
type Item struct {
	Title  string     `json:"title"`
	Detail string     `json:"detail,omitempty"`
	Bad    bool       `json:"bad,omitempty"`
	At     *time.Time `json:"at,omitempty"`
	// Pool is the pool it's about, when it's about one.
	Pool string `json:"pool,omitempty"`
}

// Alerts are the alerts raised this week, how many of those have
// cleared, and how many are active now.
type Alerts struct {
	Raised  int `json:"raised"`
	Cleared int `json:"cleared"`
	// Notable are the warnings and criticals raised, at most five.
	Notable   []Item `json:"notable"`
	ActiveNow int    `json:"active_now"`
}

// Backups are the backup tasks that ran this week.
type Backups struct {
	Succeeded int    `json:"succeeded"`
	Failed    []Item `json:"failed"`
}

// Apps are the apps updated this week, and those with an update waiting.
type Apps struct {
	Updated   []Item   `json:"updated"`
	Available []string `json:"available"`
}

// Assistant is what the assistant did this week.
type Assistant struct {
	Actions   int `json:"actions"`
	Scheduled int `json:"scheduled"`
	Failed    int `json:"failed"`
	CheckIns  int `json:"check_ins"`
}

// Caller is what the digest needs of TrueNAS. *truenas.Client is one.
type Caller interface {
	Call(ctx context.Context, method string, params ...any) (json.RawMessage, error)
}

// Summarizer has a model write a few sentences from the facts.
type Summarizer interface {
	Summarize(ctx context.Context, facts string) (string, error)
}

// Sources are where a digest's facts come from. Capacity and Summarizer
// may be nil.
type Sources struct {
	NAS        Caller
	Events     *events.Store
	Capacity   *capacity.Store
	Summarizer Summarizer
	Store      *Store
	Now        func() time.Time
}

func (s Sources) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Assemble puts together the week to now. With summary, a connected model
// adds its take.
func (s Sources) Assemble(ctx context.Context, summary bool) (Digest, error) {
	now := s.now()
	d := Digest{ID: newID(), WeekStart: now.AddDate(0, 0, -7), WeekEnd: now, CreatedAt: now}

	pools, err := s.pools(ctx)
	if err != nil {
		return Digest{}, fmt.Errorf("pools: %w", err)
	}
	s.addPools(ctx, &d, pools, now)
	evs, err := s.Events.List(ctx, events.ListFilter{Since: d.WeekStart})
	if err != nil {
		return Digest{}, fmt.Errorf("events: %w", err)
	}
	active := s.activeAlerts(ctx)
	if active != nil {
		d.Alerts.ActiveNow = len(active)
	}
	addEvents(&d, evs, active)
	s.addApps(ctx, &d)
	d.Headline = headline(d)

	if summary && s.Summarizer != nil {
		summaryCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		text, err := s.Summarizer.Summarize(summaryCtx, Facts(d))
		cancel()
		if err != nil {
			slog.Warn("digest: no summary", "err", err)
		} else {
			d.Summary = strings.TrimSpace(text)
		}
	}
	return d, nil
}

// ----- pools and scrubs ----------------------------------------------------------

type poolRow struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Size      *int64 `json:"size"`
	Allocated *int64 `json:"allocated"`
	Scan      struct {
		Function   string  `json:"function"`
		State      string  `json:"state"`
		EndTime    any     `json:"end_time"`
		Errors     int64   `json:"errors"`
		Percentage float64 `json:"percentage"`
	} `json:"scan"`
}

func (s Sources) pools(ctx context.Context) ([]poolRow, error) {
	raw, err := s.NAS.Call(ctx, "pool.query")
	if err != nil {
		return nil, err
	}
	var rows []poolRow
	err = json.Unmarshal(raw, &rows)
	return rows, err
}

func (s Sources) addPools(ctx context.Context, d *Digest, pools []poolRow, now time.Time) {
	for _, p := range pools {
		week := PoolWeek{Pool: p.Name, Status: strings.ToUpper(p.Status)}
		if p.Size != nil && p.Allocated != nil && *p.Size > 0 {
			week.Size, week.Allocated = *p.Size, *p.Allocated
			week.Percent = float64(week.Allocated) / float64(week.Size) * 100
		}
		if s.Capacity != nil {
			if history, err := s.Capacity.History(ctx, p.Name, now.AddDate(0, 0, -90)); err == nil && len(history) > 0 {
				f := capacity.ForecastFrom(p.Name, history, now)
				week.Trend, week.PerDay, week.At80, week.At100 = f.Status, f.PerDay, f.At80, f.At100
				// A week's change needs a reading from about a week ago.
				weekAgo := now.AddDate(0, 0, -7).UTC().Format("2006-01-02")
				nearEnough := now.AddDate(0, 0, -5).UTC().Format("2006-01-02")
				for _, sample := range history {
					if sample.Day >= weekAgo {
						if sample.Day <= nearEnough && week.Allocated > 0 {
							week.Change, week.ChangeKnown = week.Allocated-sample.Allocated, true
						}
						break
					}
				}
			}
		}
		d.Pools = append(d.Pools, week)

		if week.Status != "" && week.Status != "ONLINE" {
			d.Disks = append(d.Disks, Item{Title: fmt.Sprintf("%s is %s", p.Name, strings.ToLower(week.Status)), Bad: true, Pool: p.Name})
		}
		ended := timeOf(p.Scan.EndTime)
		inWeek := !ended.IsZero() && ended.After(d.WeekStart)
		switch fn, state := strings.ToUpper(p.Scan.Function), strings.ToUpper(p.Scan.State); {
		case fn == "SCRUB" && state == "SCANNING":
			d.Scrubs = append(d.Scrubs, Item{Title: fmt.Sprintf("%s is being scrubbed now (%.0f%%)", p.Name, p.Scan.Percentage), Pool: p.Name})
		case fn == "SCRUB" && state == "FINISHED" && inWeek:
			item := Item{Title: p.Name + " was scrubbed, with no errors", At: &ended, Pool: p.Name}
			if p.Scan.Errors > 0 {
				item = Item{Title: fmt.Sprintf("%s's scrub found %d errors", p.Name, p.Scan.Errors), Bad: true, At: &ended, Pool: p.Name}
			}
			d.Scrubs = append(d.Scrubs, item)
		case fn == "SCRUB" && !ended.IsZero() && now.Sub(ended) > 35*24*time.Hour:
			d.Scrubs = append(d.Scrubs, Item{Title: fmt.Sprintf("%s hasn't been scrubbed for %d weeks", p.Name, int(now.Sub(ended).Hours()/24/7)),
				Detail: "A monthly scrub finds problems while there's still redundancy to fix them.", Pool: p.Name})
		case fn == "RESILVER" && state == "SCANNING":
			d.Disks = append(d.Disks, Item{Title: fmt.Sprintf("%s is rebuilding onto a new disk (%.0f%%)", p.Name, p.Scan.Percentage), Pool: p.Name})
		case fn == "RESILVER" && state == "FINISHED" && inWeek:
			item := Item{Title: p.Name + " finished rebuilding onto a new disk", At: &ended, Pool: p.Name}
			if p.Scan.Errors > 0 {
				item.Detail, item.Bad = fmt.Sprintf("It found %d errors.", p.Scan.Errors), true
			}
			d.Disks = append(d.Disks, item)
		}
	}
}

// ----- events --------------------------------------------------------------------

// backupMethods are the jobs that are backups running.
var backupMethods = map[string]bool{
	"replication.run": true, "replication.run_onetime": true, "cloudsync.sync": true,
	"cloud_backup.sync": true, "rsynctask.run": true,
}

// addEvents adds the week's events. active is the uuids of the alerts
// active now, or nil when they couldn't be read.
func addEvents(d *Digest, evs []events.Event, active map[string]bool) {
	// Oldest first reads as the week went.
	sort.Slice(evs, func(i, j int) bool { return evs[i].OccurredAt.Before(evs[j].OccurredAt) })
	for _, ev := range evs {
		at := ev.OccurredAt
		switch {
		case strings.HasPrefix(ev.DedupeKey, events.AssistantKeyPrefix):
			entry := ev.DedupeKey[strings.LastIndexByte(ev.DedupeKey, '|')+1:]
			switch entry {
			case events.AssistantRan:
				d.Assistant.Actions++
			case events.AssistantScheduled, events.AssistantRule:
				d.Assistant.Scheduled++
			case events.AssistantCheckIn:
				d.Assistant.CheckIns++
			}
			if events.IsAssistantPush(ev) && entry != events.AssistantCheckIn {
				d.Assistant.Failed++
			}
		case ev.Kind == events.KindDisk:
			d.Disks = append(d.Disks, Item{Title: ev.Title, Detail: ev.Summary, Bad: true, At: &at, Pool: diskEventPool(ev.DedupeKey)})
		case ev.Kind == events.KindAlert:
			d.Alerts.Raised++
			var alert struct {
				UUID string `json:"uuid"`
			}
			_ = json.Unmarshal(ev.Body, &alert)
			cleared := active != nil && alert.UUID != "" && !active[alert.UUID]
			if cleared {
				d.Alerts.Cleared++
			}
			if (ev.Severity == events.SeverityCritical || ev.Severity == events.SeverityWarning) && len(d.Alerts.Notable) < 5 {
				item := Item{Title: ev.Title, Bad: ev.Severity == events.SeverityCritical && !cleared, At: &at}
				if cleared {
					item.Detail = "Since cleared"
				}
				d.Alerts.Notable = append(d.Alerts.Notable, item)
			}
		case ev.Kind == events.KindSecurity:
			if len(d.Security) < 5 {
				d.Security = append(d.Security, Item{Title: ev.Title, Bad: ev.Severity == events.SeverityCritical, At: &at})
			}
		case ev.Kind == events.KindJob:
			var job struct {
				Method string `json:"method"`
				State  string `json:"state"`
			}
			_ = json.Unmarshal(ev.Body, &job)
			switch {
			case backupMethods[job.Method] && strings.EqualFold(job.State, "SUCCESS"):
				d.Backups.Succeeded++
			case backupMethods[job.Method]:
				d.Backups.Failed = append(d.Backups.Failed, Item{Title: ev.Title, Detail: ev.Summary, Bad: true, At: &at})
			case (job.Method == "update.run" || job.Method == "update.update") && strings.EqualFold(job.State, "SUCCESS"):
				d.System = append(d.System, Item{Title: "TrueNAS was updated", At: &at})
			}
		}
	}
}

// ----- alerts and apps -----------------------------------------------------------

// activeAlerts is the uuids of the alerts active now (not dismissed), or
// nil when TrueNAS doesn't say.
func (s Sources) activeAlerts(ctx context.Context) map[string]bool {
	raw, err := s.NAS.Call(ctx, "alert.list")
	if err != nil {
		return nil
	}
	var alerts []struct {
		UUID      string `json:"uuid"`
		Dismissed bool   `json:"dismissed"`
	}
	if json.Unmarshal(raw, &alerts) != nil {
		return nil
	}
	active := map[string]bool{}
	for _, a := range alerts {
		if !a.Dismissed {
			active[a.UUID] = true
		}
	}
	return active
}

// diskEventPool reads the pool from a disk event's key: "disk|<pool>|...".
func diskEventPool(key string) string {
	parts := strings.Split(key, "|")
	if len(parts) >= 3 && parts[0] == "disk" {
		return parts[1]
	}
	return ""
}

func (s Sources) addApps(ctx context.Context, d *Digest) {
	raw, err := s.NAS.Call(ctx, "app.query")
	if err != nil {
		return
	}
	var apps []struct {
		Name             string `json:"name"`
		Version          string `json:"version"`
		HumanVersion     string `json:"human_version"`
		UpgradeAvailable bool   `json:"upgrade_available"`
		Metadata         struct {
			Title string `json:"title"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &apps) != nil {
		return
	}
	var before map[string]string
	if s.Store != nil {
		if last, ok := s.Store.Latest(); ok {
			before = last.AppVersions
		}
	}
	d.AppVersions = map[string]string{}
	for _, a := range apps {
		version := a.HumanVersion
		if version == "" {
			version = a.Version
		}
		d.AppVersions[a.Name] = version
		title := a.Metadata.Title
		if title == "" {
			title = a.Name
		}
		if was, ok := before[a.Name]; ok && was != version && version != "" {
			d.Apps.Updated = append(d.Apps.Updated, Item{Title: title, Detail: was + " → " + version})
		}
		if a.UpgradeAvailable {
			d.Apps.Available = append(d.Apps.Available, title)
		}
	}
}

// ----- the headline and the facts ------------------------------------------------

// bad are the things that want a look, worst first.
func bad(d Digest) []string {
	var out []string
	// A pool's disk trouble is one thing, however many events it made.
	diskPools := map[string]bool{}
	for _, item := range d.Disks {
		if !item.Bad {
			continue
		}
		if item.Pool != "" {
			if diskPools[item.Pool] {
				continue
			}
			diskPools[item.Pool] = true
		}
		out = append(out, item.Title)
	}
	for _, group := range [][]Item{d.Scrubs, d.Backups.Failed, d.Alerts.Notable, d.Security} {
		for _, item := range group {
			if item.Bad {
				out = append(out, item.Title)
			}
		}
	}
	for _, p := range d.Pools {
		if p.At100 != nil && p.At100.Sub(d.WeekEnd) < 60*24*time.Hour {
			out = append(out, fmt.Sprintf("%s is full in about %s", p.Pool, roughly(p.At100.Sub(d.WeekEnd))))
		}
	}
	return out
}

func headline(d Digest) string {
	issues := bad(d)
	switch len(issues) {
	case 0:
		return "All quiet: nothing needs a look."
	case 1:
		return "Worth a look: " + issues[0] + "."
	}
	return fmt.Sprintf("%d things worth a look, first: %s.", len(issues), issues[0])
}

// roughly says a duration in days or weeks: "about 6 weeks".
func roughly(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case days < 2:
		return "a day"
	case days < 14:
		return fmt.Sprintf("%d days", days)
	}
	return fmt.Sprintf("%d weeks", days/7)
}

// Facts is the digest as plain lines, for a model to summarise.
func Facts(d Digest) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, "- "+format+"\n", args...) }
	for _, p := range d.Pools {
		text := fmt.Sprintf("Pool %s: %s, %.0f%% full", p.Pool, strings.ToLower(p.Status), p.Percent)
		if p.ChangeKnown {
			text += fmt.Sprintf(", %+.1f GB this week", float64(p.Change)/1e9)
		}
		if p.At100 != nil {
			text += ", full in about " + roughly(p.At100.Sub(d.WeekEnd))
		}
		line("%s", text)
	}
	for _, group := range [][]Item{d.Disks, d.Scrubs, d.Backups.Failed, d.System} {
		for _, item := range group {
			line("%s%s", item.Title, map[bool]string{true: " (" + item.Detail + ")", false: ""}[item.Detail != ""])
		}
	}
	if d.Backups.Succeeded > 0 {
		line("%d backup runs succeeded", d.Backups.Succeeded)
	}
	line("%d alerts raised this week, %d of them since cleared; %d active now", d.Alerts.Raised, d.Alerts.Cleared, d.Alerts.ActiveNow)
	for _, item := range d.Alerts.Notable {
		if item.Detail != "" {
			line("Alert: %s (%s)", item.Title, strings.ToLower(item.Detail))
		} else {
			line("Alert: %s", item.Title)
		}
	}
	for _, item := range d.Apps.Updated {
		line("App updated: %s (%s)", item.Title, item.Detail)
	}
	if len(d.Apps.Available) > 0 {
		line("Updates available: %s", strings.Join(d.Apps.Available, ", "))
	}
	for _, item := range d.Security {
		line("Security: %s", item.Title)
	}
	if a := d.Assistant; a.Actions+a.Scheduled+a.CheckIns > 0 {
		line("The assistant made %d changes, ran %d booked jobs (%d went wrong) and wrote %d check-ins", a.Actions, a.Scheduled, a.Failed, a.CheckIns)
	}
	return b.String()
}

func timeOf(v any) time.Time {
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

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
