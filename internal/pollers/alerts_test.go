package pollers

import (
	"encoding/json"
	"testing"

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

func TestFingerprintDetectsRefire(t *testing.T) {
	a1 := map[string]any{
		"last_occurrence": map[string]any{"$date": float64(1714000000000)},
	}
	a2 := map[string]any{
		"last_occurrence": map[string]any{"$date": float64(1714000000000)},
	}
	if fingerprintAlert(a1) != fingerprintAlert(a2) {
		t.Error("same last_occurrence should match")
	}
	a3 := map[string]any{
		"last_occurrence": map[string]any{"$date": float64(1714000060000)},
	}
	if fingerprintAlert(a1) == fingerprintAlert(a3) {
		t.Error("different last_occurrence should differ")
	}
}
