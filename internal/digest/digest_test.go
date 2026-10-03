package digest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/capacity"
	"github.com/donnie123421/zephyr-helper/internal/events"
)

type fakeNAS struct {
	mu      sync.Mutex
	replies map[string]any
}

func (f *fakeNAS) set(method string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method] = v
}

// fail makes a method fail, as when TrueNAS doesn't answer.
func (f *fakeNAS) fail(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.replies, method)
}

func (f *fakeNAS) Call(_ context.Context, method string, _ ...any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.replies[method]
	if !ok {
		return nil, fmt.Errorf("unexpected call %s", method)
	}
	return json.Marshal(v)
}

type fakeSummarizer struct {
	facts string
	calls int
	err   error
}

func (f *fakeSummarizer) Summarize(_ context.Context, facts string) (string, error) {
	f.facts = facts
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return "A quiet week. tank is filling steadily.", nil
}

var now = time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

func ms(t time.Time) map[string]any { return map[string]any{"$date": t.UnixMilli()} }

type rig struct {
	nas      *fakeNAS
	events   *events.Store
	capacity *capacity.Store
	store    *Store
	settings *SettingsStore
	sum      *fakeSummarizer
	sources  Sources
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	evs, err := events.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = evs.Close() })
	cap, err := capacity.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cap.Close() })
	store, _ := OpenStore(filepath.Join(dir, "digests.json"))
	settings, _ := OpenSettings(filepath.Join(dir, "digest-settings.json"))
	nas := &fakeNAS{replies: map[string]any{}}
	tb := int64(1_000_000_000_000)
	nas.set("pool.query", []any{
		map[string]any{"name": "tank", "status": "ONLINE", "size": 10 * tb, "allocated": 6 * tb,
			"scan": map[string]any{"function": "SCRUB", "state": "FINISHED", "end_time": ms(now.AddDate(0, 0, -2)), "errors": 0}},
		map[string]any{"name": "backup", "status": "DEGRADED", "size": 4 * tb, "allocated": 1 * tb,
			"scan": map[string]any{"function": "SCRUB", "state": "FINISHED", "end_time": ms(now.AddDate(0, 0, -60)), "errors": 0}},
	})
	nas.set("alert.list", []any{map[string]any{"uuid": "active-1", "dismissed": false}, map[string]any{"uuid": "active-2", "dismissed": false},
		map[string]any{"uuid": "dismissed-1", "dismissed": true}})
	nas.set("app.query", []any{
		map[string]any{"name": "plex", "version": "1.2.5", "human_version": "1.41.3_1.2.5", "upgrade_available": false, "metadata": map[string]any{"title": "Plex"}},
		map[string]any{"name": "sonarr", "version": "2.0.0", "human_version": "4.0.9", "upgrade_available": true, "metadata": map[string]any{"title": "Sonarr"}},
	})
	// Thirty days of tank growing 50 GB a day.
	for i := 0; i < 30; i++ {
		day := now.AddDate(0, 0, i-29)
		_ = cap.Record(context.Background(), capacity.Sample{Day: day.Format("2006-01-02"), Pool: "tank",
			Size: 10 * tb, Allocated: 6*tb - int64(29-i)*tb/20, SampledAt: day})
	}
	r := &rig{nas: nas, events: evs, capacity: cap, store: store, settings: settings, sum: &fakeSummarizer{}}
	r.sources = Sources{NAS: nas, Events: evs, Capacity: cap, Summarizer: r.sum, Store: store, Now: func() time.Time { return now }}
	return r
}

func (r *rig) ingest(t *testing.T, ev events.Event) {
	t.Helper()
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = now.AddDate(0, 0, -1)
	}
	if _, err := r.events.Ingest(context.Background(), ev, 0); err != nil {
		t.Fatal(err)
	}
}

func TestADigestGathersTheWeek(t *testing.T) {
	r := newRig(t)
	job := func(method, state string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"method": method, "state": state})
		return b
	}
	r.ingest(t, events.Event{Kind: events.KindJob, Severity: events.SeverityInfo, Title: "Replication finished", Body: job("replication.run", "SUCCESS"), DedupeKey: "a"})
	r.ingest(t, events.Event{Kind: events.KindJob, Severity: events.SeverityWarning, Title: "Cloud sync failed", Summary: "Cloud sync failed: key revoked", Body: job("cloudsync.sync", "FAILED"), DedupeKey: "b"})
	alert := func(uuid string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"uuid": uuid})
		return b
	}
	poolAlert, _ := json.Marshal(map[string]any{"uuid": "active-2", "klass": "VolumeStatus", "args": map[string]any{"volume": "backup"}})
	r.ingest(t, events.Event{Kind: events.KindAlert, Severity: events.SeverityCritical, Title: "Pool backup state is DEGRADED", Body: poolAlert, DedupeKey: "f", OccurredAt: now.AddDate(0, 0, -4)})
	r.ingest(t, events.Event{Kind: events.KindAlert, Severity: events.SeverityCritical, Title: "Pool backup is DEGRADED", Body: alert("active-1"), DedupeKey: "c", OccurredAt: now.AddDate(0, 0, -3)})
	r.ingest(t, events.Event{Kind: events.KindAlert, Severity: events.SeverityWarning, Title: "Replication of tank failed", Body: alert("gone-1"), DedupeKey: "e", OccurredAt: now.AddDate(0, 0, -2)})
	r.ingest(t, events.Event{Kind: events.KindDisk, Severity: events.SeverityCritical, Title: "Disk sdb failed in backup", DedupeKey: "disk|backup|fault"})
	r.ingest(t, events.Event{Kind: events.KindJob, Severity: events.SeverityInfo, Title: "Started a scrub", DedupeKey: events.AssistantKey("x", events.AssistantRan)})
	r.ingest(t, events.Event{Kind: events.KindAlert, Severity: events.SeverityInfo, Title: "Old", DedupeKey: "d", OccurredAt: now.AddDate(0, 0, -9)})
	_ = r.store.Add(Digest{ID: "last", CreatedAt: now.AddDate(0, 0, -7), AppVersions: map[string]string{"plex": "1.40.5_1.2.4", "sonarr": "4.0.9"}})

	d, err := r.sources.Assemble(context.Background(), true)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(d.Pools) != 2 || d.Pools[0].Trend != "growing" || !d.Pools[0].ChangeKnown || d.Pools[0].Change != 7*1_000_000_000_000/20 {
		t.Errorf("pools = %+v", d.Pools)
	}
	if d.Pools[0].At100 == nil {
		t.Error("no full date for a growing pool")
	}
	if len(d.Scrubs) != 2 || d.Scrubs[0].Title != "tank was scrubbed, with no errors" || !d.Scrubs[0].Good ||
		!strings.HasPrefix(d.Scrubs[1].Title, "backup hasn't been scrubbed for 8 weeks") || !d.Scrubs[1].Warn {
		t.Errorf("scrubs = %+v", d.Scrubs)
	}
	if len(d.Disks) != 2 || d.Disks[0].Title != "backup is degraded" || d.Disks[1].Pool != "backup" {
		t.Errorf("disks = %+v", d.Disks)
	}
	if d.Backups.Succeeded != 1 || len(d.Backups.Failed) != 1 || d.Backups.Failed[0].Title != "Cloud sync failed" || !d.Backups.Failed[0].Bad {
		t.Errorf("backups %+v", d.Backups)
	}
	if d.Issues != 3 {
		t.Errorf("issues = %d", d.Issues)
	}
	// Criticals still active first, newest first; then the cleared one.
	if a := d.Alerts; a.Raised != 3 || a.Cleared != 1 || a.ActiveNow == nil || *a.ActiveNow != 2 || len(a.Notable) != 3 ||
		a.Notable[0].Title != "Pool backup is DEGRADED" || !a.Notable[0].Bad || a.Notable[1].Pool != "backup" ||
		a.Notable[2].Bad || a.Notable[2].Warn || a.Notable[2].Detail != "Since cleared" {
		t.Errorf("alerts %+v", d.Alerts)
	}
	if len(d.Apps.Updated) != 1 || d.Apps.Updated[0].Detail != "1.40.5 → 1.41.3" || fmt.Sprint(d.Apps.Available) != "[Sonarr]" {
		t.Errorf("apps = %+v", d.Apps)
	}
	if d.Assistant.Actions != 1 {
		t.Errorf("assistant = %+v", d.Assistant)
	}
	// backup's trouble counts once with its disk event and its state
	// alert; the cleared warning doesn't count; the failing cloud sync
	// does.
	if !strings.HasPrefix(d.Headline, "3 things worth a look, first: backup is degraded") {
		t.Errorf("headline = %q", d.Headline)
	}
	if !strings.Contains(r.sum.facts, "3 alerts raised this week, 1 of them since cleared; 2 active now") {
		t.Errorf("facts:\n%s", r.sum.facts)
	}
	if d.Summary != "A quiet week. tank is filling steadily." || !strings.Contains(r.sum.facts, "Pool tank: online, 60% full, +350.0 GB this week") {
		t.Errorf("summary %q from facts:\n%s", d.Summary, r.sum.facts)
	}
}

func TestADigestWithoutAModelIsJustTheFacts(t *testing.T) {
	r := newRig(t)
	r.sum.err = errors.New("no model")
	d, err := r.sources.Assemble(context.Background(), true)
	if err != nil || d.Summary != "" || d.Headline == "" {
		t.Errorf("digest %+v, %v", d, err)
	}
}

func TestTheDigestGoesOutOnSundayMorningOnce(t *testing.T) {
	r := newRig(t)
	sydney, _ := time.LoadLocation("Australia/Sydney")
	at := time.Date(2026, 10, 4, 9, 30, 0, 0, sydney) // a Sunday
	r.sources.Now = func() time.Time { return at }
	quiet := false
	runner := &Runner{Sources: r.sources, Settings: r.settings,
		Zone: func(context.Context) *time.Location { return sydney }, Quiet: func(time.Time) bool { return quiet }}

	runner.check(context.Background())
	if _, ok := r.store.Latest(); ok {
		t.Fatal("went out while it was off")
	}
	_, _ = r.settings.Set(Settings{Enabled: true, Summary: false}, at.AddDate(0, 0, -3))
	quiet = true
	runner.check(context.Background())
	if _, ok := r.store.Latest(); ok {
		t.Fatal("went out in quiet hours")
	}
	quiet = false
	runner.check(context.Background())
	runner.check(context.Background())
	if n := len(r.store.List()); n != 1 {
		t.Fatalf("%d digests, want 1", n)
	}
	evs, _ := r.events.List(context.Background(), events.ListFilter{})
	var announced *events.Event
	for i := range evs {
		if strings.HasPrefix(evs[i].DedupeKey, events.DigestKeyPrefix) {
			announced = &evs[i]
		}
	}
	if announced == nil || announced.Title != "Your NAS this week" {
		t.Errorf("feed = %+v", evs)
	}

	// A Sunday's digest missed by more than a day waits for the next.
	r2 := newRig(t)
	_, _ = r2.settings.Set(Settings{Enabled: true}, at.AddDate(0, 0, -3))
	r2.sources.Now = func() time.Time { return time.Date(2026, 10, 6, 10, 0, 0, 0, sydney) }
	(&Runner{Sources: r2.sources, Settings: r2.settings, Zone: func(context.Context) *time.Location { return sydney }}).check(context.Background())
	if _, ok := r2.store.Latest(); ok {
		t.Error("a Tuesday sent last Sunday's digest")
	}
}

func TestLastDueIsTheSundayJustGone(t *testing.T) {
	sydney, _ := time.LoadLocation("Australia/Sydney")
	cases := map[time.Time]time.Time{
		time.Date(2026, 10, 4, 9, 0, 0, 0, sydney):  time.Date(2026, 10, 4, 9, 0, 0, 0, sydney),
		time.Date(2026, 10, 4, 8, 59, 0, 0, sydney): time.Date(2026, 9, 27, 9, 0, 0, 0, sydney),
		time.Date(2026, 10, 8, 14, 0, 0, 0, sydney): time.Date(2026, 10, 4, 9, 0, 0, 0, sydney),
	}
	for in, want := range cases {
		if got := lastDue(in, sydney); !got.Equal(want) {
			t.Errorf("lastDue(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestTheDigestRoutes(t *testing.T) {
	r := newRig(t)
	mux := http.NewServeMux()
	(&Handler{Sources: r.sources, Settings: r.settings}).RegisterMux(mux, func(h http.Handler) http.Handler { return h })
	do := func(method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := do("GET", "/digest/settings", ""); code != 200 || !strings.Contains(body, `"enabled":false`) || !strings.Contains(body, `"summary":true`) {
		t.Errorf("settings = %d %s", code, body)
	}
	if code, body := do("PUT", "/digest/settings", `{"enabled":true,"summary":false}`); code != 200 || !r.settings.Get().Enabled || !strings.Contains(body, `"enabled_at"`) {
		t.Errorf("put = %d %s", code, body)
	}
	if code, _ := do("PUT", "/digest/settings", `{"enabled":true}`); code != 400 {
		t.Errorf("put without summary = %d", code)
	}
	if code, _ := do("GET", "/digest/latest", ""); code != 404 {
		t.Errorf("latest with none = %d", code)
	}
	code, body := do("POST", "/digest/preview", "")
	if code != 200 || !strings.Contains(body, `"preview":true`) || strings.Contains(body, "app_versions") {
		t.Errorf("preview = %d %s", code, body)
	}
	if r.sum.facts != "" {
		t.Error("a preview asked the model")
	}
	if len(r.store.List()) != 0 {
		t.Error("a preview was kept")
	}
}

func TestTheFirstDigestIsTheSundayAfterItsSwitchedOn(t *testing.T) {
	r := newRig(t)
	sydney, _ := time.LoadLocation("Australia/Sydney")
	sunday := time.Date(2026, 10, 4, 9, 30, 0, 0, sydney)
	now := sunday
	r.sources.Now = func() time.Time { return now }
	runner := &Runner{Sources: r.sources, Settings: r.settings, Zone: func(context.Context) *time.Location { return sydney }}

	// Switched on at 9:10, after this Sunday's was due.
	settings, _ := r.settings.Set(Settings{Enabled: true, Summary: true}, sunday.Add(-20*time.Minute))
	runner.check(context.Background())
	if _, ok := r.store.Latest(); ok {
		t.Fatal("the digest went out the moment it was switched on")
	}
	// Changing the summary setting keeps when it was switched on.
	if again, _ := r.settings.Set(Settings{Enabled: true, Summary: false}, sunday); !again.EnabledAt.Equal(*settings.EnabledAt) {
		t.Errorf("switched on at %v, then %v", settings.EnabledAt, again.EnabledAt)
	}
	now = sunday.AddDate(0, 0, 7)
	runner.check(context.Background())
	if _, ok := r.store.Latest(); !ok {
		t.Error("no digest the Sunday after")
	}
}

func TestADigestThatCouldntBeKeptIsRetriedWithoutTheModel(t *testing.T) {
	r := newRig(t)
	dir := filepath.Join(t.TempDir(), "later")
	store, _ := OpenStore(filepath.Join(dir, "digests.json"))
	r.sources.Store = store
	sydney, _ := time.LoadLocation("Australia/Sydney")
	at := time.Date(2026, 10, 4, 9, 30, 0, 0, sydney)
	r.sources.Now = func() time.Time { return at }
	_, _ = r.settings.Set(Settings{Enabled: true, Summary: true}, at.AddDate(0, 0, -3))
	runner := &Runner{Sources: r.sources, Settings: r.settings, Zone: func(context.Context) *time.Location { return sydney }}

	runner.check(context.Background()) // the directory isn't there
	if _, ok := store.Latest(); ok || r.sum.calls != 1 {
		t.Fatalf("kept %v, model asked %d times", ok, r.sum.calls)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner.check(context.Background())
	if _, ok := store.Latest(); !ok || r.sum.calls != 1 {
		t.Errorf("kept %v, model asked %d times", ok, r.sum.calls)
	}
}
