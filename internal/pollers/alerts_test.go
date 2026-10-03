package pollers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

func TestAlertToEventBasic(t *testing.T) {
	raw := `{
		"uuid": "abc-123",
		"level": "WARNING",
		"klass": "ZfsDegraded",
		"formatted": "Pool tank is degraded",
		"text": "Pool %(pool)s is degraded",
		"last_occurrence": {"$date": 1714000000000},
		"dismissed": false
	}`
	var a map[string]any
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	ev, ok := alertToEvent(a)
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.Severity != events.SeverityWarning {
		t.Errorf("severity = %s, want warning", ev.Severity)
	}
	if ev.Kind != events.KindAlert {
		t.Errorf("kind = %s, want alert", ev.Kind)
	}
	if ev.Title != "Pool tank is degraded" {
		t.Errorf("title = %q", ev.Title)
	}
	want := "alert|warning|ZfsDegraded|Pool tank is degraded"
	if ev.DedupeKey != want {
		t.Errorf("key = %q, want %q", ev.DedupeKey, want)
	}
	if ev.OccurredAt.IsZero() {
		t.Error("occurred_at zero")
	}
	// Body should roundtrip to JSON identical to the input shape.
	var decoded map[string]any
	if err := json.Unmarshal(ev.Body, &decoded); err != nil {
		t.Fatalf("body not valid json: %v", err)
	}
	if decoded["klass"] != "ZfsDegraded" {
		t.Errorf("body klass = %v", decoded["klass"])
	}
}

func TestAlertToEventFallsBackToText(t *testing.T) {
	a := map[string]any{
		"level":     "INFO",
		"klass":     "CoolInfo",
		"text":      "Everything is fine",
		"formatted": "",
	}
	ev, ok := alertToEvent(a)
	if !ok {
		t.Fatal("expected ok when text is the only message")
	}
	if ev.Title != "Everything is fine" {
		t.Errorf("title = %q, want text fallback", ev.Title)
	}
}

func TestAlertToEventSkipsWhenNoMessage(t *testing.T) {
	a := map[string]any{"level": "WARNING", "klass": "Whatever"}
	if _, ok := alertToEvent(a); ok {
		t.Error("expected skip when formatted+text both empty")
	}
}

func TestAlertToEventTruncatesLongTitle(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	a := map[string]any{
		"level":     "WARNING",
		"klass":     "Long",
		"formatted": long,
	}
	ev, ok := alertToEvent(a)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(ev.Title) != 120 {
		t.Errorf("title len = %d, want 120", len(ev.Title))
	}
	if ev.Summary != long {
		t.Error("summary should retain full message")
	}
}

func TestSeverityMapping(t *testing.T) {
	cases := []struct {
		in   string
		want events.Severity
	}{
		{"CRITICAL", events.SeverityCritical},
		{"Alert", events.SeverityCritical},
		{"EMERGENCY", events.SeverityCritical},
		{"ERROR", events.SeverityWarning},
		{"warning", events.SeverityWarning},
		{"NOTICE", events.SeverityInfo},
		{"INFO", events.SeverityInfo},
		{"", events.SeverityInfo},
		{"unknown-whatever", events.SeverityInfo},
	}
	for _, c := range cases {
		if got := severityFromAlert(c.in); got != c.want {
			t.Errorf("severityFromAlert(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestParseTrueNASTime(t *testing.T) {
	if parseTrueNASTime(map[string]any{"$date": float64(1714000000000)}).IsZero() {
		t.Error("$date form: got zero")
	}
	if parseTrueNASTime("2026-04-22T10:00:00Z").IsZero() {
		t.Error("RFC3339 form: got zero")
	}
	if !parseTrueNASTime(nil).IsZero() {
		t.Error("nil: expected zero")
	}
	if !parseTrueNASTime("garbage").IsZero() {
		t.Error("garbage: expected zero")
	}
	if !parseTrueNASTime("").IsZero() {
		t.Error("empty string: expected zero")
	}
}

func TestFingerprintIgnoresTheMinutelyTouch(t *testing.T) {
	alert := func(lastMs float64, level, text string) map[string]any {
		return map[string]any{
			"datetime":        map[string]any{"$date": float64(1714000000000)},
			"last_occurrence": map[string]any{"$date": lastMs},
			"level":           level, "formatted": text,
		}
	}
	a1 := alert(1714000000000, "WARNING", "Pool tank is 85% full")
	if fingerprintAlert(a1) != fingerprintAlert(alert(1714003600000, "WARNING", "Pool tank is 85% full")) {
		t.Error("an hour's later last_occurrence counted as a change")
	}
	if fingerprintAlert(a1) == fingerprintAlert(alert(1714000000000, "CRITICAL", "Pool tank is 85% full")) {
		t.Error("a new level wasn't a change")
	}
	if fingerprintAlert(a1) == fingerprintAlert(alert(1714000000000, "WARNING", "Pool tank is 91% full")) {
		t.Error("new text wasn't a change")
	}
	raisedAgain := alert(1714100000000, "WARNING", "Pool tank is 85% full")
	raisedAgain["datetime"] = map[string]any{"$date": float64(1714100000000)}
	if fingerprintAlert(a1) == fingerprintAlert(raisedAgain) {
		t.Error("raised again wasn't a change")
	}
}

// A standing alert is one event however long it stands, and a helper
// restart merges into it rather than starting another (which would push a
// critical again).
func TestAStandingAlertIsOneEvent(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raised := float64(time.Now().Add(-3 * time.Hour).UnixMilli())
	list := func(last time.Time) map[string]any {
		return map[string]any{"alert.list": []any{map[string]any{
			"uuid": "u-1", "level": "CRITICAL", "klass": "VolumeStatus", "formatted": "Pool backup state is DEGRADED",
			"datetime": map[string]any{"$date": raised}, "last_occurrence": map[string]any{"$date": float64(last.UnixMilli())},
			"dismissed": false,
		}}}
	}
	p := NewAlerts(testClient(t, list(time.Now().Add(-3*time.Hour))), store, time.Minute, time.Hour)
	p.poll(context.Background())
	// Three hours of TrueNAS touching it every minute, polled hourly here.
	for h := 2; h >= 0; h-- {
		p.tn = testClient(t, list(time.Now().Add(-time.Duration(h)*time.Hour)))
		p.poll(context.Background())
	}
	// A restart forgets what it saw.
	restarted := NewAlerts(testClient(t, list(time.Now())), store, time.Minute, time.Hour)
	restarted.poll(context.Background())

	evs, err := store.List(context.Background(), events.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("%d events for one standing alert: %+v", len(evs), evs)
	}
	if !evs[0].OccurredAt.Equal(time.UnixMilli(int64(raised)).UTC()) {
		t.Errorf("occurred %v, raised %v", evs[0].OccurredAt, time.UnixMilli(int64(raised)).UTC())
	}
}

// An alert whose text changes is news when it changes: its new row sits at
// the top of the feed, not back at the time it was first raised.
func TestAChangedAlertIsDatedWhenItChanged(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raised := float64(time.Now().Add(-48 * time.Hour).UnixMilli())
	now := time.Now().UTC().Truncate(time.Millisecond)
	list := func(text string) map[string]any {
		return map[string]any{"alert.list": []any{map[string]any{
			"uuid": "u-1", "level": "WARNING", "klass": "ZpoolCapacityWarning", "formatted": text,
			"datetime": map[string]any{"$date": raised}, "last_occurrence": map[string]any{"$date": float64(now.UnixMilli())},
		}}}
	}
	p := NewAlerts(testClient(t, list("Space usage for pool 'tank' is 85%.")), store, time.Minute, time.Hour)
	p.poll(context.Background())
	p.tn = testClient(t, list("Space usage for pool 'tank' is 88%."))
	p.poll(context.Background())
	evs, _ := store.List(context.Background(), events.ListFilter{})
	if len(evs) != 2 || !evs[0].OccurredAt.Equal(now) || evs[0].Title != "Space usage for pool 'tank' is 88%." {
		t.Errorf("events %+v", evs)
	}
}
