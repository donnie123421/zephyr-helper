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
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

func alertBodyJSON(uuid string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"uuid": uuid})
	return b
}

func jobJSON(method, state string, args ...any) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"method": method, "state": state, "arguments": args, "error": "[EFAULT] the application key was revoked\nTraceback..."})
	return b
}

func TestABusyWeekKeepsItsCriticals(t *testing.T) {
	r := newRig(t)
	monday := now.AddDate(0, 0, -6)
	r.ingest(t, events.Event{Kind: events.KindDisk, Severity: events.SeverityCritical, Title: "A disk failed in backup", DedupeKey: "disk|backup|fault", OccurredAt: monday})
	r.ingest(t, events.Event{Kind: events.KindAlert, Severity: events.SeverityCritical, Title: "Pool backup is DEGRADED", Body: alertBodyJSON("active-1"), DedupeKey: "c", OccurredAt: monday})
	// 250 routine rows after it: more than a page of the feed.
	for i := 0; i < 250; i++ {
		r.ingest(t, events.Event{Kind: events.KindJob, Severity: events.SeverityInfo, Title: "replication.run succeeded",
			Body: jobJSON("replication.run", "SUCCESS", 1), DedupeKey: fmt.Sprintf("job|%d", i), OccurredAt: monday.Add(time.Duration(i+1) * 30 * time.Minute)})
	}
	d, err := r.sources.Assemble(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Backups.Succeeded != 250 || d.Alerts.Raised != 1 || strings.HasPrefix(d.Headline, "All quiet") {
		t.Errorf("succeeded %d, raised %d, headline %q", d.Backups.Succeeded, d.Alerts.Raised, d.Headline)
	}
	found := false
	for _, item := range d.Disks {
		found = found || item.Title == "A disk failed in backup"
	}
	if !found {
		t.Errorf("disks = %+v", d.Disks)
	}
}

func TestALaterCriticalIsntCrowdedOut(t *testing.T) {
	r := newRig(t)
	var listed []any
	for i := 1; i <= 5; i++ {
		uuid := fmt.Sprintf("w%d", i)
		listed = append(listed, map[string]any{"uuid": uuid, "level": "WARNING"})
		r.ingest(t, events.Event{Kind: events.KindAlert, Severity: events.SeverityWarning, Title: fmt.Sprintf("Warning %d", i),
			Body: alertBodyJSON(uuid), DedupeKey: uuid, OccurredAt: now.AddDate(0, 0, -6)})
	}
	listed = append(listed, map[string]any{"uuid": "c1", "level": "CRITICAL"})
	r.nas.set("alert.list", listed)
	r.ingest(t, events.Event{Kind: events.KindAlert, Severity: events.SeverityCritical, Title: "Pool tank is offline",
		Body: alertBodyJSON("c1"), DedupeKey: "c1", OccurredAt: now.AddDate(0, 0, -2)})
	// Routine sign-ins don't push out what matters.
	for i := 0; i < 6; i++ {
		r.ingest(t, events.Event{Kind: events.KindSecurity, Severity: events.SeverityInfo, Title: "admin signed in",
			DedupeKey: fmt.Sprintf("login|%d", i), OccurredAt: now.AddDate(0, 0, -5).Add(time.Duration(i) * time.Hour)})
	}
	r.ingest(t, events.Event{Kind: events.KindSecurity, Severity: events.SeverityCritical, Title: "Audit log unreachable",
		DedupeKey: "audit", OccurredAt: now.AddDate(0, 0, -1)})

	d, _ := r.sources.Assemble(context.Background(), false)
	if len(d.Alerts.Notable) != 5 || d.Alerts.Notable[0].Title != "Pool tank is offline" || !d.Alerts.Notable[0].Bad {
		t.Errorf("notable = %+v", d.Alerts.Notable)
	}
	if len(d.Security) != 1 || d.Security[0].Title != "Audit log unreachable" {
		t.Errorf("security = %+v", d.Security)
	}
	if !strings.Contains(d.Headline, "things worth a look") {
		t.Errorf("headline = %q", d.Headline)
	}
}

func TestAFailingBackupIsOneThing(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 48; i++ {
		r.ingest(t, events.Event{Kind: events.KindJob, Severity: events.SeverityWarning, Title: "cloudsync.sync failed",
			Body: jobJSON("cloudsync.sync", "FAILED", 3), DedupeKey: fmt.Sprintf("job|%d", i), OccurredAt: now.Add(-time.Duration(48-i) * time.Hour)})
	}
	// Task 4 failed once, then ran fine.
	r.ingest(t, events.Event{Kind: events.KindJob, Severity: events.SeverityWarning, Title: "cloudsync.sync failed",
		Body: jobJSON("cloudsync.sync", "FAILED", 4), DedupeKey: "t4f", OccurredAt: now.AddDate(0, 0, -3)})
	r.ingest(t, events.Event{Kind: events.KindJob, Severity: events.SeverityInfo, Title: "cloudsync.sync succeeded",
		Body: jobJSON("cloudsync.sync", "SUCCESS", 4), DedupeKey: "t4s", OccurredAt: now.AddDate(0, 0, -2)})

	d, _ := r.sources.Assemble(context.Background(), false)
	if len(d.Backups.Failed) != 2 {
		t.Fatalf("failed = %+v", d.Backups.Failed)
	}
	three, four := d.Backups.Failed[0], d.Backups.Failed[1]
	if three.Title != "Cloud sync task 3 failed" || !three.Bad || three.Detail != "Failed 48 times this week. Last: [EFAULT] the application key was revoked" {
		t.Errorf("task 3 = %+v", three)
	}
	if four.Title != "Cloud sync task 4 failed" || four.Bad || !four.Warn || four.Detail != "Failed once, then ran fine." {
		t.Errorf("task 4 = %+v", four)
	}
	// backup is degraded; task 3 is failing. Task 4 is fine now.
	if d.Issues != 2 || strings.Count(Facts(d), "Cloud sync task 3") != 1 {
		t.Errorf("issues %d, facts:\n%s", d.Issues, Facts(d))
	}
}

func TestTheNotificationKeepsNumbersWhole(t *testing.T) {
	cases := map[string]string{
		"A quiet week: tank grew 350.0 GB. Plex has an update.":     "A quiet week: tank grew 350.0 GB.",
		"Plex moved to 1.41.3 and Sonarr to 4.0.9! Nothing else.":   "Plex moved to 1.41.3 and Sonarr to 4.0.9!",
		"Short. But then the first full stop after twenty letters.": "Short. But then the first full stop after twenty letters.",
		"No full stop at all in this one":                           "No full stop at all in this one",
	}
	for in, want := range cases {
		if got := firstSentence(in); got != want {
			t.Errorf("firstSentence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStandingProblemsStillWantALook(t *testing.T) {
	r := newRig(t)
	tb := int64(1_000_000_000_000)
	r.nas.set("pool.query", []any{map[string]any{"name": "media", "status": "ONLINE", "size": 10 * tb, "allocated": 97 * tb / 10,
		"scan": map[string]any{"function": "SCRUB", "state": "FINISHED", "end_time": ms(now.AddDate(0, 0, -3)), "errors": 0}}})
	r.nas.set("alert.list", []any{map[string]any{"uuid": "old-1", "level": "CRITICAL", "klass": "SMARTFailure",
		"formatted": "Device: /dev/sdc, <b>failed</b> SMART self-check.", "dismissed": false, "datetime": ms(now.AddDate(0, 0, -21))}})
	d, _ := r.sources.Assemble(context.Background(), false)
	if d.Issues != 2 || !strings.HasPrefix(d.Headline, "2 things worth a look, first: Device: /dev/sdc, failed SMART self-check") {
		t.Errorf("issues %d, headline %q", d.Issues, d.Headline)
	}
	if len(d.Alerts.Notable) != 1 || d.Alerts.Notable[0].Detail != "Raised before this week, still active" || !d.Alerts.Notable[0].Bad {
		t.Errorf("notable = %+v", d.Alerts.Notable)
	}
	if !strings.Contains(Facts(d), "media is 97% full") && !strings.Contains(d.Headline, "media is 97% full") {
		// The issue is in the count; the pool's line carries the percentage.
		if !strings.Contains(Facts(d), "Pool media: online, 97% full") {
			t.Errorf("facts:\n%s", Facts(d))
		}
	}
}

func TestTheWeekStartsWhereTheLastOneEnded(t *testing.T) {
	r := newRig(t)
	lastEnd := now.AddDate(0, 0, -8) // last week's went out on time; this one's a day late
	_ = r.store.Add(Digest{ID: "last", WeekStart: lastEnd.AddDate(0, 0, -7), WeekEnd: lastEnd, CreatedAt: lastEnd})
	r.ingest(t, events.Event{Kind: events.KindDisk, Severity: events.SeverityCritical, Title: "A disk failed in backup",
		DedupeKey: "disk|backup|fault", OccurredAt: lastEnd.Add(3 * time.Hour)})
	d, _ := r.sources.Assemble(context.Background(), false)
	if !d.WeekStart.Equal(lastEnd) || len(d.Disks) != 2 {
		t.Errorf("week from %v, disks %+v", d.WeekStart, d.Disks)
	}

	// After weeks switched off, it's the last seven days.
	r2 := newRig(t)
	_ = r2.store.Add(Digest{ID: "old", WeekEnd: now.AddDate(0, 0, -30), CreatedAt: now.AddDate(0, 0, -30)})
	d2, _ := r2.sources.Assemble(context.Background(), false)
	if !d2.WeekStart.Equal(now.AddDate(0, 0, -7)) {
		t.Errorf("week from %v", d2.WeekStart)
	}
}

func TestADismissedAlertHasntCleared(t *testing.T) {
	r := newRig(t)
	r.nas.set("alert.list", []any{map[string]any{"uuid": "smart", "level": "CRITICAL", "dismissed": true}})
	r.ingest(t, events.Event{Kind: events.KindAlert, Severity: events.SeverityCritical, Title: "sdc failed its SMART test",
		Body: alertBodyJSON("smart"), DedupeKey: "smart"})
	d, _ := r.sources.Assemble(context.Background(), false)
	a := d.Alerts
	if a.Cleared != 0 || a.ActiveNow == nil || *a.ActiveNow != 0 || len(a.Notable) != 1 || a.Notable[0].Detail != "Dismissed" || a.Notable[0].Bad {
		t.Errorf("alerts = %+v", a)
	}
}

func TestWithoutAlertListActiveIsntClaimed(t *testing.T) {
	r := newRig(t)
	r.nas.fail("alert.list")
	d, _ := r.sources.Assemble(context.Background(), false)
	if d.Alerts.ActiveNow != nil || strings.Contains(Facts(d), "active now") {
		t.Errorf("active %v, facts:\n%s", d.Alerts.ActiveNow, Facts(d))
	}
	data, _ := json.Marshal(d.Alerts)
	if strings.Contains(string(data), "active_now") {
		t.Errorf("alerts = %s", data)
	}
}

func TestAppVersionsCarryOverAFailedQuery(t *testing.T) {
	r := newRig(t)
	_ = r.store.Add(Digest{ID: "1", WeekEnd: now.AddDate(0, 0, -14), CreatedAt: now.AddDate(0, 0, -14), AppVersions: map[string]string{"plex": "1.40.5_1.2.4"}})
	r.nas.fail("app.query")
	d, _ := r.sources.Assemble(context.Background(), false)
	if d.AppVersions["plex"] != "1.40.5_1.2.4" {
		t.Fatalf("versions = %v", d.AppVersions)
	}
	_ = r.store.Add(d)
	r.nas.set("app.query", []any{map[string]any{"name": "plex", "human_version": "1.41.3_1.2.5", "metadata": map[string]any{"title": "Plex"}}})
	next, _ := r.sources.Assemble(context.Background(), false)
	if len(next.Apps.Updated) != 1 || next.Apps.Updated[0].Detail != "1.40.5 → 1.41.3" {
		t.Errorf("updated = %+v", next.Apps.Updated)
	}
}

func TestTheWeeksChangeIsInUsableSpace(t *testing.T) {
	r := newRig(t)
	tb := int64(1_000_000_000_000)
	// tank's raw 10 TB is 5 TB usable (RAIDZ2 of four).
	r.nas.set("pool.dataset.query", []any{map[string]any{"id": "tank",
		"used": map[string]any{"parsed": 3 * tb}, "available": map[string]any{"rawvalue": fmt.Sprint(2 * tb)}}})
	d, _ := r.sources.Assemble(context.Background(), false)
	if d.Pools[0].Change != 7*tb/20/2 || d.Pools[0].Percent != 60 {
		t.Errorf("tank = %+v", d.Pools[0])
	}
}

func TestADiskFaultSinceRebuiltNoLongerWantsALook(t *testing.T) {
	r := newRig(t)
	r.ingest(t, events.Event{Kind: events.KindDisk, Severity: events.SeverityCritical, Title: "A disk failed in tank",
		Summary: "sdb faulted.", DedupeKey: "disk|tank|fault"})
	d, _ := r.sources.Assemble(context.Background(), false)
	var item Item
	for _, i := range d.Disks {
		if i.Pool == "tank" {
			item = i
		}
	}
	if item.Bad || item.Detail != "sdb faulted. The pool is healthy again." {
		t.Errorf("tank's disk = %+v", item)
	}
}

func TestQuietHoursStartingMeanwhileHoldTheNotification(t *testing.T) {
	r := newRig(t)
	sydney, _ := time.LoadLocation("Australia/Sydney")
	at := time.Date(2026, 10, 4, 9, 30, 0, 0, sydney)
	r.sources.Now = func() time.Time { return at }
	_, _ = r.settings.Set(Settings{Enabled: true}, at.AddDate(0, 0, -3))
	calls := 0
	quiet := func(time.Time) bool { calls++; return calls > 1 && calls < 4 } // quiet from just after it starts
	runner := &Runner{Sources: r.sources, Settings: r.settings, Zone: func(context.Context) *time.Location { return sydney }, Quiet: quiet}

	announced := func() int {
		evs, _ := r.events.List(context.Background(), events.ListFilter{})
		n := 0
		for _, ev := range evs {
			if strings.HasPrefix(ev.DedupeKey, events.DigestKeyPrefix) {
				n++
			}
		}
		return n
	}
	runner.check(context.Background()) // assembled and kept; quiet by then
	if _, ok := r.store.Latest(); !ok || announced() != 0 {
		t.Fatalf("kept %v, announced %d", ok, announced())
	}
	runner.check(context.Background()) // still quiet
	runner.check(context.Background()) // quiet over
	runner.check(context.Background())
	if announced() != 1 || len(r.store.List()) != 1 {
		t.Errorf("announced %d, kept %d", announced(), len(r.store.List()))
	}
}

type flakyNAS struct {
	calls  int
	failAt map[int]bool
	zone   string
}

func (f *flakyNAS) Call(context.Context, string, ...any) (json.RawMessage, error) {
	f.calls++
	if f.failAt[f.calls] {
		return nil, errors.New("timed out")
	}
	return json.Marshal(map[string]string{"timezone": f.zone})
}

func TestTheZoneIsntGuessed(t *testing.T) {
	never := NASZone(&flakyNAS{failAt: map[int]bool{1: true}, zone: "Australia/Sydney"})
	if never(context.Background()) != nil {
		t.Error("a zone without ever reading one")
	}
	nas := &flakyNAS{failAt: map[int]bool{2: true}, zone: "Australia/Sydney"}
	zone := NASZone(nas)
	first := zone(context.Background())
	if first == nil || first.String() != "Australia/Sydney" {
		t.Fatalf("zone = %v", first)
	}
}

func TestOneDigestAWeekWhateverTheZone(t *testing.T) {
	r := newRig(t)
	sydney, _ := time.LoadLocation("Australia/Sydney")
	at := time.Date(2026, 10, 4, 9, 0, 30, 0, sydney)
	r.sources.Now = func() time.Time { return at }
	_, _ = r.settings.Set(Settings{Enabled: true}, at.AddDate(0, 0, -3))
	loc := sydney
	runner := &Runner{Sources: r.sources, Settings: r.settings, Zone: func(context.Context) *time.Location { return loc }}
	runner.check(context.Background())
	// That evening the zone reads as UTC, where it's now Sunday 9 am.
	at = time.Date(2026, 10, 4, 20, 0, 30, 0, sydney)
	loc = time.UTC
	runner.check(context.Background())
	// No zone at all: nothing happens.
	runner.Zone = func(context.Context) *time.Location { return nil }
	runner.check(context.Background())
	if n := len(r.store.List()); n != 1 {
		t.Errorf("%d digests", n)
	}
}

func TestAnUnreadableFileIsSetAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "digests.json")
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	store, err := OpenStore(path)
	if err != nil || len(store.List()) != 0 {
		t.Fatalf("store %v, %v", store, err)
	}
	aside, _ := filepath.Glob(path + ".unreadable-*")
	if len(aside) != 1 {
		t.Errorf("set aside: %v", aside)
	}
	settingsPath := filepath.Join(dir, "settings.json")
	_ = os.WriteFile(settingsPath, []byte("[]"), 0o600)
	settings, err := OpenSettings(settingsPath)
	if err != nil || settings.Get().Enabled || !settings.Get().Summary {
		t.Errorf("settings %+v, %v", settings.Get(), err)
	}
}

func TestTheRoutesAnswerWithoutTheirFiles(t *testing.T) {
	mux := http.NewServeMux()
	(&Handler{}).RegisterMux(mux, func(h http.Handler) http.Handler { return h })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/digest/settings", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "couldn't open its digest files") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestNoDigestsIsAnEmptyList(t *testing.T) {
	r := newRig(t)
	mux := http.NewServeMux()
	(&Handler{Sources: r.sources, Settings: r.settings}).RegisterMux(mux, func(h http.Handler) http.Handler { return h })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/digests", nil))
	if strings.TrimSpace(rec.Body.String()) != `{"digests":[]}` {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestPlainText(t *testing.T) {
	in := "Pool <b>backup</b> state is DEGRADED: One or more devices are faulted.<br>The following devices are not healthy:<ul><li>Disk WDC &amp; serial X is FAULTED</li></ul>"
	want := "Pool backup state is DEGRADED: One or more devices are faulted. The following devices are not healthy: Disk WDC & serial X is FAULTED"
	if got := plainText(in); got != want {
		t.Errorf("plainText = %q", got)
	}
}

func TestTrueNASUpdatesAreSeenFromItsVersion(t *testing.T) {
	r := newRig(t)
	r.nas.set("system.info", map[string]any{"version": "25.04.2.1"})
	first, _ := r.sources.Assemble(context.Background(), false)
	if first.TrueNASVersion != "25.04.2.1" || len(first.System) != 0 {
		t.Fatalf("first: %q %+v", first.TrueNASVersion, first.System)
	}
	_ = r.store.Add(first)
	r.nas.set("system.info", map[string]any{"version": "25.10.0"})
	next, _ := r.sources.Assemble(context.Background(), false)
	if len(next.System) != 1 || next.System[0].Detail != "25.04.2.1 → 25.10.0" {
		t.Errorf("system = %+v", next.System)
	}
}

func TestCustomAppsGoByName(t *testing.T) {
	r := newRig(t)
	r.nas.set("app.query", []any{map[string]any{"name": "zephyr-helper", "custom_app": true, "human_version": "1.0.0_custom",
		"upgrade_available": true, "metadata": map[string]any{"title": "Custom App"}}})
	d, _ := r.sources.Assemble(context.Background(), false)
	if fmt.Sprint(d.Apps.Available) != "[zephyr-helper]" {
		t.Errorf("available = %v", d.Apps.Available)
	}
	if appVersion("2024_10_1_1.2.3") != "2024_10_1" {
		t.Errorf("appVersion = %q", appVersion("2024_10_1_1.2.3"))
	}
}

func TestAFullPoolIsOneThing(t *testing.T) {
	r := newRig(t)
	tb := int64(1_000_000_000_000)
	r.nas.set("pool.query", []any{map[string]any{"name": "tank", "status": "ONLINE", "size": 10 * tb, "allocated": 96 * tb / 10}})
	r.nas.set("alert.list", []any{map[string]any{"uuid": "cap", "level": "CRITICAL", "klass": "ZpoolCapacityCritical"}})
	body, _ := json.Marshal(map[string]any{"uuid": "cap", "klass": "ZpoolCapacityCritical", "args": map[string]any{"volume": "tank", "capacity": 96}})
	r.ingest(t, events.Event{Kind: events.KindAlert, Severity: events.SeverityCritical, Title: "Space usage for pool 'tank' is 95%.", Body: body, DedupeKey: "cap95", OccurredAt: now.AddDate(0, 0, -3)})
	// The same alert's text changed later in the week.
	r.ingest(t, events.Event{Kind: events.KindAlert, Severity: events.SeverityCritical, Title: "Space usage for pool 'tank' is 96%.", Body: body, DedupeKey: "cap96", OccurredAt: now.AddDate(0, 0, -1)})
	d, _ := r.sources.Assemble(context.Background(), false)
	if d.Issues != 1 || d.Headline != "Worth a look: Space usage for pool 'tank' is 96%." {
		t.Errorf("issues %d, headline %q", d.Issues, d.Headline)
	}
	if d.Alerts.Raised != 1 || len(d.Alerts.Notable) != 1 || d.Alerts.Notable[0].Title != "Space usage for pool 'tank' is 96%." {
		t.Errorf("alerts = %+v", d.Alerts)
	}
}

func TestACloudSyncFailureIsOneThing(t *testing.T) {
	r := newRig(t)
	r.nas.set("pool.query", []any{})
	r.nas.set("alert.list", []any{map[string]any{"uuid": "cs", "level": "CRITICAL", "klass": "CloudSyncTaskFailed"}})
	body, _ := json.Marshal(map[string]any{"uuid": "cs", "klass": "CloudSyncTaskFailed", "args": map[string]any{"id": 3, "name": "Offsite"}})
	r.ingest(t, events.Event{Kind: events.KindAlert, Severity: events.SeverityCritical, Title: "Cloud sync task \"Offsite\" failed.", Body: body, DedupeKey: "cs"})
	r.ingest(t, events.Event{Kind: events.KindJob, Severity: events.SeverityWarning, Title: "cloudsync.sync failed", Body: jobJSON("cloudsync.sync", "FAILED", 3), DedupeKey: "j"})
	d, _ := r.sources.Assemble(context.Background(), false)
	if d.Issues != 1 {
		t.Errorf("issues %d, headline %q", d.Issues, d.Headline)
	}
}

func TestAHeldNotificationSurvivesARestart(t *testing.T) {
	r := newRig(t)
	sydney, _ := time.LoadLocation("Australia/Sydney")
	at := time.Date(2026, 10, 4, 9, 30, 0, 0, sydney)
	r.sources.Now = func() time.Time { return at }
	_, _ = r.settings.Set(Settings{Enabled: true}, at.AddDate(0, 0, -3))
	quiet := true
	calls := 0
	runner := &Runner{Sources: r.sources, Settings: r.settings, Zone: func(context.Context) *time.Location { return sydney },
		Quiet: func(time.Time) bool { calls++; return calls > 1 && quiet }}
	runner.check(context.Background()) // kept, held for quiet hours
	if last, ok := r.store.Latest(); !ok || last.Announced {
		t.Fatalf("kept %v, announced %v", ok, last.Announced)
	}
	// The helper restarts; quiet hours end.
	quiet = false
	reopened, _ := OpenStore(r.store.path)
	r.sources.Store = reopened
	again := &Runner{Sources: r.sources, Settings: r.settings, Zone: func(context.Context) *time.Location { return sydney },
		Quiet: func(time.Time) bool { return false }}
	again.check(context.Background())
	again.check(context.Background())
	evs, _ := r.events.List(context.Background(), events.ListFilter{})
	announced := 0
	for _, ev := range evs {
		if strings.HasPrefix(ev.DedupeKey, events.DigestKeyPrefix) {
			announced++
		}
	}
	if last, _ := reopened.Latest(); announced != 1 || !last.Announced || len(reopened.List()) != 1 {
		t.Errorf("announced %d, flag %v, kept %d", announced, last.Announced, len(reopened.List()))
	}
}
