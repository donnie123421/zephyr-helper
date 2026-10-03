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
	"html"
	"log/slog"
	"regexp"
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
	// Issues is how many things want a look, as the headline counts them.
	Issues int `json:"issues"`
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
	// AppVersions are the apps' versions as of this digest, and
	// TrueNASVersion TrueNAS's, for the next one to see what was updated
	// in between.
	AppVersions    map[string]string `json:"app_versions,omitempty"`
	TrueNASVersion string            `json:"truenas_version,omitempty"`
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

// Item is one thing that happened. Bad wants a look; Warn is worth
// knowing; Good went right.
type Item struct {
	Title  string     `json:"title"`
	Detail string     `json:"detail,omitempty"`
	Bad    bool       `json:"bad,omitempty"`
	Warn   bool       `json:"warn,omitempty"`
	Good   bool       `json:"good,omitempty"`
	At     *time.Time `json:"at,omitempty"`
	// Pool is the pool it's about, when it's about one.
	Pool string `json:"pool,omitempty"`
}

// Alerts are the alerts raised this week, how many of those have
// cleared, and how many are active now.
type Alerts struct {
	Raised  int `json:"raised"`
	Cleared int `json:"cleared"`
	// Notable are the alerts most worth knowing about, at most five:
	// criticals still active first, from this week or before it.
	Notable []Item `json:"notable"`
	// ActiveNow is left out when TrueNAS didn't say.
	ActiveNow *int `json:"active_now,omitempty"`
}

// Backups are the backup tasks that ran this week.
type Backups struct {
	Succeeded int `json:"succeeded"`
	// Failed is one item for each task that failed, however often.
	Failed []Item `json:"failed"`
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

// Sources are where a digest's facts come from. Capacity, Summarizer and
// Store may be nil.
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

const (
	// fullPercent is full enough to want a look, whatever the trend: ZFS
	// slows down well before 100%.
	fullPercent = 90
	// fillsSoon is how close a pool's full date has to be to want a look.
	fillsSoon = 60 * 24 * time.Hour
	// scrubOverdue is how long since a pool's last scrub is too long.
	// TrueNAS's default scrub runs when 35 days have passed, checked
	// weekly, so up to 42 days is normal.
	scrubOverdue = 45 * 24 * time.Hour
	// notableMax is how many alerts the digest lists.
	notableMax = 5
	// securityMax is how many security events it lists.
	securityMax = 5
)

// Assemble puts together the week to now: since the last digest kept, so
// a late or early one leaves no gap and counts nothing twice, or the last
// seven days. With summary, a connected model adds its take.
func (s Sources) Assemble(ctx context.Context, summary bool) (Digest, error) {
	now := s.now()
	start := now.AddDate(0, 0, -7)
	var kept []Digest
	if s.Store != nil {
		kept = s.Store.List()
	}
	if len(kept) > 0 {
		if end := kept[0].WeekEnd; end.Before(now) && now.Sub(end) <= 10*24*time.Hour {
			start = end
		}
	}
	d := Digest{ID: newID(), WeekStart: start, WeekEnd: now, CreatedAt: now}
	w := &week{d: &d, now: now, poolOnline: map[string]bool{}, tasks: map[string]*task{},
		diskIndex: map[string]int{}, raised: map[string]bool{}}

	pools, err := s.pools(ctx)
	if err != nil {
		return Digest{}, fmt.Errorf("pools: %w", err)
	}
	w.addPools(ctx, s.Capacity, pools)
	s.toUsable(ctx, &d, pools)
	evs, err := s.Events.Between(ctx, start, now)
	if err != nil {
		return Digest{}, fmt.Errorf("events: %w", err)
	}
	alerts := s.alerts(ctx)
	if alerts != nil {
		active := 0
		for _, a := range alerts {
			if !a.Dismissed {
				active++
			}
		}
		d.Alerts.ActiveNow = &active
	}
	w.addEvents(evs, alerts)
	w.addStandingAlerts(alerts)
	w.finish()
	s.addApps(ctx, &d, kept)
	s.addTrueNASUpdate(ctx, &d, kept)
	if w.updatedAt != nil && len(d.System) == 0 {
		d.System = append(d.System, Item{Title: "TrueNAS was updated", Good: true, At: w.updatedAt})
	}
	w.addPoolIssues()
	d.Issues = len(w.issues)
	d.Headline = headline(w.issues)

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

// week gathers a digest's facts, and what wants a look, as it goes.
type week struct {
	d          *Digest
	now        time.Time
	poolOnline map[string]bool
	issues     []issue
	// alertItems are every alert worth listing, before the cap.
	alertItems []ranked
	security   []ranked
	// tasks are the backup tasks that failed, by task.
	tasks     map[string]*task
	taskOrder []string
	// disks are the disk events, by key, the newest kept.
	diskIndex map[string]int
	// raised are the uuids of the alerts raised this week.
	raised map[string]bool
	// updatedAt is when an update job was seen to finish.
	updatedAt *time.Time
}

// issue is one thing that wants a look. key makes a pool's trouble one
// issue however many things say so.
type issue struct {
	title string
	key   string
}

func (w *week) addIssue(title, key string) {
	if key == "" {
		key = title
	}
	for _, i := range w.issues {
		if i.key == key {
			return
		}
	}
	w.issues = append(w.issues, issue{title: title, key: key})
}

// ranked is an item with what it's ordered by: worst first, then newest.
type ranked struct {
	item Item
	rank int
	at   time.Time
}

func rankOf(item Item) int {
	switch {
	case item.Bad:
		return 0
	case item.Warn:
		return 1
	}
	return 2
}

// top is the first n items, worst and newest first, one of each title.
func top(items []ranked, n int) []Item {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].rank != items[j].rank {
			return items[i].rank < items[j].rank
		}
		return items[i].at.After(items[j].at)
	})
	var out []Item
	seen := map[string]bool{}
	for _, r := range items {
		if len(out) == n {
			break
		}
		if seen[r.item.Title] {
			continue
		}
		seen[r.item.Title] = true
		out = append(out, r.item)
	}
	return out
}

// ----- pools and scrubs ----------------------------------------------------------

type poolRow struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Size      *int64 `json:"size"`
	Allocated *int64 `json:"allocated"`
	Scan      *struct {
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

func poolHealthKey(pool string) string { return "pool-health|" + pool }

func (w *week) addPools(ctx context.Context, history *capacity.Store, pools []poolRow) {
	d, now := w.d, w.now
	for _, p := range pools {
		pw := PoolWeek{Pool: p.Name, Status: strings.ToUpper(p.Status)}
		w.poolOnline[p.Name] = pw.Status == "ONLINE"
		if p.Size != nil && p.Allocated != nil && *p.Size > 0 {
			pw.Size, pw.Allocated = *p.Size, *p.Allocated
			pw.Percent = float64(pw.Allocated) / float64(pw.Size) * 100
		}
		if history != nil {
			if samples, err := history.History(ctx, p.Name, now.AddDate(0, 0, -90)); err == nil && len(samples) > 0 {
				f := capacity.ForecastFrom(p.Name, samples, now)
				pw.Trend, pw.PerDay, pw.At80, pw.At100 = f.Status, f.PerDay, f.At80, f.At100
				// A week's change needs a reading from about a week ago.
				weekAgo := now.AddDate(0, 0, -7).UTC().Format("2006-01-02")
				nearEnough := now.AddDate(0, 0, -5).UTC().Format("2006-01-02")
				for _, sample := range samples {
					if sample.Day >= weekAgo {
						if sample.Day <= nearEnough && pw.Allocated > 0 {
							pw.Change, pw.ChangeKnown = pw.Allocated-sample.Allocated, true
						}
						break
					}
				}
			}
		}
		d.Pools = append(d.Pools, pw)

		if pw.Status != "" && pw.Status != "ONLINE" {
			title := fmt.Sprintf("%s is %s", p.Name, strings.ToLower(pw.Status))
			d.Disks = append(d.Disks, Item{Title: title, Bad: true, Pool: p.Name})
			w.addIssue(title, poolHealthKey(p.Name))
		}
		if p.Scan == nil {
			continue
		}
		ended := timeOf(p.Scan.EndTime)
		inWeek := !ended.IsZero() && ended.After(d.WeekStart)
		switch fn, state := strings.ToUpper(p.Scan.Function), strings.ToUpper(p.Scan.State); {
		case fn == "SCRUB" && state == "SCANNING":
			d.Scrubs = append(d.Scrubs, Item{Title: fmt.Sprintf("%s is being scrubbed now (%.0f%%)", p.Name, p.Scan.Percentage), Pool: p.Name})
		case fn == "SCRUB" && state == "FINISHED" && inWeek:
			item := Item{Title: p.Name + " was scrubbed, with no errors", Good: true, At: &ended, Pool: p.Name}
			if p.Scan.Errors > 0 {
				item = Item{Title: fmt.Sprintf("%s's scrub found %d errors", p.Name, p.Scan.Errors), Bad: true, At: &ended, Pool: p.Name}
				w.addIssue(item.Title, "")
			}
			d.Scrubs = append(d.Scrubs, item)
		case fn == "SCRUB" && !ended.IsZero() && now.Sub(ended) > scrubOverdue:
			d.Scrubs = append(d.Scrubs, Item{Title: fmt.Sprintf("%s hasn't been scrubbed for %d weeks", p.Name, int(now.Sub(ended).Hours()/24/7)),
				Detail: "A monthly scrub finds problems while there's still redundancy to fix them.", Warn: true, Pool: p.Name})
		case fn == "RESILVER" && state == "SCANNING":
			d.Disks = append(d.Disks, Item{Title: fmt.Sprintf("%s is rebuilding onto a new disk (%.0f%%)", p.Name, p.Scan.Percentage), Pool: p.Name})
		case fn == "RESILVER" && state == "FINISHED" && inWeek:
			item := Item{Title: p.Name + " finished rebuilding onto a new disk", Good: true, At: &ended, Pool: p.Name}
			if p.Scan.Errors > 0 {
				item.Detail, item.Bad, item.Good = fmt.Sprintf("It found %d errors.", p.Scan.Errors), true, false
				w.addIssue(p.Name+" rebuilt with errors", poolHealthKey(p.Name))
			}
			d.Disks = append(d.Disks, item)
		}
	}
}

// toUsable turns each pool's weekly change and trend into usable space,
// as the app shows it: TrueNAS's pool size and use include RAIDZ parity,
// so on a 4-disk RAIDZ2 they're twice what was written. The pool's root
// dataset has the usable figures. Percentages and dates don't change.
func (s Sources) toUsable(ctx context.Context, d *Digest, pools []poolRow) {
	names := make([]string, 0, len(pools))
	for _, p := range pools {
		names = append(names, p.Name)
	}
	if len(names) == 0 {
		return
	}
	raw, err := s.NAS.Call(ctx, "pool.dataset.query", []any{[]any{"id", "in", names}},
		map[string]any{"extra": map[string]any{"retrieve_children": false, "properties": []string{"used", "available"}}})
	if err != nil {
		slog.Debug("digest: no root datasets; space stays raw", "err", err)
		return
	}
	var roots []struct {
		ID        string        `json:"id"`
		Used      datasetNumber `json:"used"`
		Available datasetNumber `json:"available"`
	}
	if json.Unmarshal(raw, &roots) != nil {
		return
	}
	usable := map[string]float64{}
	for _, r := range roots {
		usable[r.ID] = float64(r.Used.value() + r.Available.value())
	}
	for i := range d.Pools {
		p := &d.Pools[i]
		if p.Size <= 0 || usable[p.Pool] <= 0 {
			continue
		}
		ratio := usable[p.Pool] / float64(p.Size)
		if ratio < 0.05 || ratio > 1.05 {
			continue
		}
		p.Change = int64(float64(p.Change) * ratio)
		p.PerDay *= ratio
	}
}

// datasetNumber is a ZFS property as pool.dataset.query has it:
// {"parsed": 123, "rawvalue": "123", "value": "123B"}.
type datasetNumber struct {
	Parsed   any    `json:"parsed"`
	RawValue string `json:"rawvalue"`
}

func (n datasetNumber) value() int64 {
	switch v := n.Parsed.(type) {
	case float64:
		return int64(v)
	case string:
		var x int64
		if _, err := fmt.Sscan(v, &x); err == nil {
			return x
		}
	}
	var x int64
	if _, err := fmt.Sscan(n.RawValue, &x); err == nil {
		return x
	}
	return 0
}

// addPoolIssues adds pools that are nearly full, or filling soon, after
// everything that went wrong.
func (w *week) addPoolIssues() {
	for _, p := range w.d.Pools {
		key := "pool-full|" + p.Pool
		switch {
		case p.Percent >= fullPercent:
			w.addIssue(fmt.Sprintf("%s is %.0f%% full", p.Pool, p.Percent), key)
		case p.At100 != nil && p.At100.Sub(w.d.WeekEnd) < fillsSoon:
			w.addIssue(fmt.Sprintf("%s is full in about %s", p.Pool, roughly(p.At100.Sub(w.d.WeekEnd))), key)
		}
	}
}

// ----- events --------------------------------------------------------------------

// backupTasks names the jobs that are backups running.
var backupTasks = map[string]string{
	"replication.run":         "Replication",
	"replication.run_onetime": "One-off replication",
	"cloudsync.sync":          "Cloud sync",
	"cloud_backup.sync":       "Cloud backup",
	"rsynctask.run":           "Rsync",
}

// task is one backup task's failures this week.
type task struct {
	name        string
	failures    int
	lastFailure time.Time
	lastError   string
	lastSuccess time.Time
}

type jobBody struct {
	Method    string            `json:"method"`
	State     string            `json:"state"`
	Arguments []json.RawMessage `json:"arguments"`
	Error     string            `json:"error"`
}

// taskOf names the task a backup job ran: "Cloud sync task 3", or just
// "Cloud sync" when the job doesn't say which.
func taskOf(job jobBody) (key, name string) {
	name = backupTasks[job.Method]
	key = job.Method
	if len(job.Arguments) > 0 {
		var id int64
		if json.Unmarshal(job.Arguments[0], &id) == nil && id > 0 {
			return fmt.Sprintf("%s|%d", job.Method, id), fmt.Sprintf("%s task %d", name, id)
		}
	}
	return key, name
}

type alertBody struct {
	UUID  string `json:"uuid"`
	Klass string `json:"klass"`
	Args  struct {
		Volume string `json:"volume"`
	} `json:"args"`
}

// addEvents adds the week's events. alerts is what alert.list says now,
// by uuid, or nil when it couldn't be read.
func (w *week) addEvents(evs []events.Event, alerts map[string]alertRow) {
	d := w.d
	// Oldest first reads as the week went.
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].OccurredAt.Before(evs[j].OccurredAt) })
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
			w.addDiskEvent(ev)
		case ev.Kind == events.KindAlert:
			var body alertBody
			_ = json.Unmarshal(ev.Body, &body)
			// The same alert seen again later in the week is one alert.
			if body.UUID != "" {
				if w.raised[body.UUID] {
					continue
				}
				w.raised[body.UUID] = true
			}
			d.Alerts.Raised++
			// A dismissed alert stays listed until its cause clears.
			row, present := alerts[body.UUID]
			cleared := alerts != nil && body.UUID != "" && !present
			if cleared {
				d.Alerts.Cleared++
			}
			if ev.Severity != events.SeverityCritical && ev.Severity != events.SeverityWarning {
				continue
			}
			item := Item{Title: plainText(ev.Title), At: &at}
			// A pool's state alert says what its status line does.
			if body.Klass == "VolumeStatus" {
				item.Pool = body.Args.Volume
			}
			switch {
			case cleared:
				item.Detail = "Since cleared"
			case present && row.Dismissed:
				item.Detail = "Dismissed"
			default:
				item.Bad = ev.Severity == events.SeverityCritical
				item.Warn = ev.Severity == events.SeverityWarning
			}
			if item.Bad {
				key := ""
				if item.Pool != "" {
					key = poolHealthKey(item.Pool)
				}
				w.addIssue(item.Title, key)
			}
			w.alertItems = append(w.alertItems, ranked{item: item, rank: rankOf(item), at: at})
		case ev.Kind == events.KindSecurity:
			// Routine sign-ins aren't news; warnings and criticals are.
			if ev.Severity != events.SeverityCritical && ev.Severity != events.SeverityWarning {
				continue
			}
			item := Item{Title: plainText(ev.Title), Bad: ev.Severity == events.SeverityCritical,
				Warn: ev.Severity == events.SeverityWarning, At: &at}
			if item.Bad {
				w.addIssue(item.Title, "")
			}
			w.security = append(w.security, ranked{item: item, rank: rankOf(item), at: at})
		case ev.Kind == events.KindJob:
			var job jobBody
			_ = json.Unmarshal(ev.Body, &job)
			// One run per row: a row's count isn't runs, as a helper
			// restart merges the finished jobs it sees again into it.
			const runs = 1
			switch {
			case backupTasks[job.Method] != "":
				key, name := taskOf(job)
				t := w.tasks[key]
				if t == nil {
					t = &task{name: name}
					w.tasks[key] = t
					w.taskOrder = append(w.taskOrder, key)
				}
				if strings.EqualFold(job.State, "SUCCESS") {
					d.Backups.Succeeded += runs
					t.lastSuccess = ev.LastSeen
					if t.lastSuccess.IsZero() {
						t.lastSuccess = at
					}
					continue
				}
				t.failures += runs
				t.lastFailure = ev.LastSeen
				if t.lastFailure.IsZero() {
					t.lastFailure = at
				}
				if job.Error != "" {
					t.lastError = firstLine(job.Error)
				}
			case (job.Method == "update.run" || job.Method == "update.update") && strings.EqualFold(job.State, "SUCCESS"):
				// Rarely seen: the job reboots TrueNAS ten seconds after it
				// finishes. addTrueNASUpdate compares versions instead.
				w.updatedAt = &at
			}
		}
	}
}

// addDiskEvent adds a disk event, the newest of each kind. It wants a look
// only while its pool isn't healthy.
func (w *week) addDiskEvent(ev events.Event) {
	at := ev.OccurredAt
	pool := diskEventPool(ev.DedupeKey)
	item := Item{Title: plainText(ev.Title), Detail: plainText(ev.Summary), At: &at, Pool: pool}
	serious := ev.Severity == events.SeverityCritical || ev.Severity == events.SeverityWarning
	switch online, known := w.poolOnline[pool]; {
	case !serious:
	case known && online:
		item.Detail = strings.TrimSpace(item.Detail + " The pool is healthy again.")
	default:
		item.Bad = true
		key := ""
		if pool != "" {
			key = poolHealthKey(pool)
		}
		w.addIssue(item.Title, key)
	}
	if i, ok := w.diskIndex[ev.DedupeKey]; ok {
		w.d.Disks[i] = item
		return
	}
	w.diskIndex[ev.DedupeKey] = len(w.d.Disks)
	w.d.Disks = append(w.d.Disks, item)
}

// ----- alerts and apps -----------------------------------------------------------

// alertRow is an alert as alert.list has it.
type alertRow struct {
	UUID      string `json:"uuid"`
	Level     string `json:"level"`
	Klass     string `json:"klass"`
	Formatted string `json:"formatted"`
	Text      string `json:"text"`
	Dismissed bool   `json:"dismissed"`
	Args      any    `json:"args"`
	Datetime  any    `json:"datetime"`
}

// alerts is what alert.list says now, by uuid, dismissed ones included
// (TrueNAS keeps a dismissed alert listed until its cause clears), or nil
// when it couldn't be read.
func (s Sources) alerts(ctx context.Context) map[string]alertRow {
	raw, err := s.NAS.Call(ctx, "alert.list")
	if err != nil {
		return nil
	}
	var rows []alertRow
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	out := make(map[string]alertRow, len(rows))
	for _, r := range rows {
		out[r.UUID] = r
	}
	return out
}

// criticalLevels are TrueNAS's alert levels that the helper calls critical.
var criticalLevels = map[string]bool{"CRITICAL": true, "ALERT": true, "EMERGENCY": true}

// addStandingAlerts adds criticals still active that were raised before
// this week: a problem doesn't stop wanting a look because it's old.
func (w *week) addStandingAlerts(alerts map[string]alertRow) {
	uuids := make([]string, 0, len(alerts))
	for uuid := range alerts {
		uuids = append(uuids, uuid)
	}
	sort.Strings(uuids)
	for _, uuid := range uuids {
		a := alerts[uuid]
		if a.Dismissed || !criticalLevels[strings.ToUpper(a.Level)] || w.raised[uuid] {
			continue
		}
		title := plainText(firstNonEmpty(a.Formatted, a.Text))
		if title == "" {
			continue
		}
		item := Item{Title: title, Detail: "Raised before this week, still active", Bad: true}
		if a.Klass == "VolumeStatus" {
			if args, ok := a.Args.(map[string]any); ok {
				item.Pool, _ = args["volume"].(string)
			}
		}
		if raised := timeOf(a.Datetime); !raised.IsZero() {
			item.At = &raised
		}
		key := ""
		if item.Pool != "" {
			key = poolHealthKey(item.Pool)
		}
		w.addIssue(title, key)
		at := time.Time{}
		if item.At != nil {
			at = *item.At
		}
		w.alertItems = append(w.alertItems, ranked{item: item, rank: rankOf(item), at: at})
	}
}

// finish caps the lists, and turns each failing backup task into one item.
func (w *week) finish() {
	d := w.d
	d.Alerts.Notable = top(w.alertItems, notableMax)
	d.Security = top(w.security, securityMax)
	for _, key := range w.taskOrder {
		t := w.tasks[key]
		if t.failures == 0 {
			continue
		}
		times := "once"
		if t.failures > 1 {
			times = fmt.Sprintf("%d times", t.failures)
		}
		item := Item{Title: t.name + " failed", At: &t.lastFailure}
		if t.lastSuccess.After(t.lastFailure) {
			item.Detail = fmt.Sprintf("Failed %s, then ran fine.", times)
			item.Warn = true
		} else {
			item.Bad = true
			item.Detail = fmt.Sprintf("Failed %s this week.", times)
			if t.lastError != "" {
				item.Detail += " Last: " + plainText(t.lastError)
			}
			w.addIssue(t.name+" is failing", "task|"+key)
		}
		d.Backups.Failed = append(d.Backups.Failed, item)
	}
	// Still failing first, then the most recent.
	sort.SliceStable(d.Backups.Failed, func(i, j int) bool {
		a, b := d.Backups.Failed[i], d.Backups.Failed[j]
		if a.Bad != b.Bad {
			return a.Bad
		}
		return a.At.After(*b.At)
	})
}

// diskEventPool reads the pool from a disk event's key: "disk|<pool>|...".
func diskEventPool(key string) string {
	parts := strings.Split(key, "|")
	if len(parts) >= 3 && parts[0] == "disk" {
		return parts[1]
	}
	return ""
}

// addApps adds the apps updated since the last digest that knew their
// versions, and those with an update waiting. Without app.query, the last
// versions known carry forward, so the next digest still sees updates.
func (s Sources) addApps(ctx context.Context, d *Digest, kept []Digest) {
	var before map[string]string
	for _, k := range kept {
		if len(k.AppVersions) > 0 {
			before = k.AppVersions
			break
		}
	}
	raw, err := s.NAS.Call(ctx, "app.query")
	var apps []struct {
		Name             string `json:"name"`
		Version          string `json:"version"`
		HumanVersion     string `json:"human_version"`
		UpgradeAvailable bool   `json:"upgrade_available"`
		CustomApp        bool   `json:"custom_app"`
		Metadata         struct {
			Title string `json:"title"`
		} `json:"metadata"`
	}
	if err != nil || json.Unmarshal(raw, &apps) != nil {
		d.AppVersions = before
		return
	}
	d.AppVersions = map[string]string{}
	for _, a := range apps {
		version := a.HumanVersion
		if version == "" {
			version = a.Version
		}
		d.AppVersions[a.Name] = version
		// Every custom app's title is "Custom App": its name is its own.
		title := a.Metadata.Title
		if title == "" || a.CustomApp {
			title = a.Name
		}
		if was, ok := before[a.Name]; ok && was != version && version != "" {
			from, to := appVersion(was), appVersion(version)
			if from == to {
				from, to = was, version
			}
			d.Apps.Updated = append(d.Apps.Updated, Item{Title: title, Detail: from + " → " + to})
		}
		if a.UpgradeAvailable {
			d.Apps.Available = append(d.Apps.Available, title)
		}
	}
}

// appVersion is the app's own version from TrueNAS's human version, which
// adds the package's after the last underscore: "1.41.3_1.0.1" is 1.41.3.
// The app's own can have underscores of its own.
func appVersion(human string) string {
	if i := strings.LastIndexByte(human, '_'); i > 0 {
		return human[:i]
	}
	return human
}

// addTrueNASUpdate notes TrueNAS updated since the last digest, from its
// version: the update job reboots TrueNAS seconds after it finishes, so
// it's rarely seen. Without system.info, the version carries forward.
func (s Sources) addTrueNASUpdate(ctx context.Context, d *Digest, kept []Digest) {
	var before string
	for _, k := range kept {
		if k.TrueNASVersion != "" {
			before = k.TrueNASVersion
			break
		}
	}
	d.TrueNASVersion = before
	raw, err := s.NAS.Call(ctx, "system.info")
	if err != nil {
		return
	}
	var info struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &info) != nil || info.Version == "" {
		return
	}
	d.TrueNASVersion = info.Version
	if before != "" && before != info.Version {
		d.System = append(d.System, Item{Title: "TrueNAS was updated", Detail: before + " → " + info.Version, Good: true})
	}
}

// ----- the headline and the facts ------------------------------------------------

func headline(issues []issue) string {
	switch len(issues) {
	case 0:
		return "All quiet: nothing needs a look."
	case 1:
		return "Worth a look: " + issues[0].title + "."
	}
	return fmt.Sprintf("%d things worth a look, first: %s.", len(issues), issues[0].title)
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
			if item.Detail != "" {
				line("%s (%s)", item.Title, item.Detail)
			} else {
				line("%s", item.Title)
			}
		}
	}
	if d.Backups.Succeeded > 0 {
		line("%d backup runs succeeded", d.Backups.Succeeded)
	}
	alerts := fmt.Sprintf("%d alerts raised this week, %d of them since cleared", d.Alerts.Raised, d.Alerts.Cleared)
	if d.Alerts.ActiveNow != nil {
		alerts += fmt.Sprintf("; %d active now", *d.Alerts.ActiveNow)
	}
	line("%s", alerts)
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

var (
	breaks = regexp.MustCompile(`(?i)<\s*(br|/?li|/?ul|/?p|/?div)\b[^>]*>`)
	tags   = regexp.MustCompile(`<[^>]*>`)
	spaces = regexp.MustCompile(`\s+`)
)

// plainText is TrueNAS's alert text, which can carry HTML, as one line.
func plainText(s string) string {
	s = breaks.ReplaceAllString(s, " ")
	s = tags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.TrimSpace(spaces.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > 200 {
		s = string(r[:199]) + "…"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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
