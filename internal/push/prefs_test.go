package push

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

func TestPrefsStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push_prefs.json")

	s := NewPrefsStore(path)
	if s.IsMuted("ALERT_WARNING") {
		t.Fatal("fresh store should have nothing muted")
	}

	if err := s.SetMuted([]string{"ALERT_WARNING", "POOL_DEGRADED"}); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}
	if !s.IsMuted("ALERT_WARNING") || !s.IsMuted("POOL_DEGRADED") {
		t.Error("categories should be muted after SetMuted")
	}
	if s.IsMuted("ALERT_CRITICAL") {
		t.Error("unmentioned category must not be muted")
	}

	// A new store over the same file must see the persisted choice.
	reloaded := NewPrefsStore(path)
	if !reloaded.IsMuted("ALERT_WARNING") {
		t.Error("mute must survive a reload")
	}

	// Replacing the set drops previous entries.
	if err := reloaded.SetMuted([]string{"ALERT_WARNING"}); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}
	if reloaded.IsMuted("POOL_DEGRADED") {
		t.Error("SetMuted replaces, not merges")
	}
}

func TestPrefsStoreToleratesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push_prefs.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewPrefsStore(path)
	if s.IsMuted("ALERT_WARNING") {
		t.Error("corrupt file must decode as nothing-muted")
	}
	// And a save must recover the file.
	if err := s.SetMuted([]string{"ALERT_WARNING"}); err != nil {
		t.Fatalf("SetMuted over corrupt file: %v", err)
	}
	if !NewPrefsStore(path).IsMuted("ALERT_WARNING") {
		t.Error("save must repair the corrupt file")
	}
}

func TestQuietHoursWindow(t *testing.T) {
	s := NewPrefsStore(filepath.Join(t.TempDir(), "prefs.json"))

	// 22:00 → 07:00 UTC, wrapping past midnight.
	if err := s.Set(nil, QuietHours{
		Enabled: true, StartMinutes: 22 * 60, EndMinutes: 7 * 60, Timezone: "UTC",
	}); err != nil {
		t.Fatal(err)
	}

	at := func(hour, min int) time.Time {
		return time.Date(2026, 7, 7, hour, min, 0, 0, time.UTC)
	}
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"before window", at(21, 59), false},
		{"window start", at(22, 0), true},
		{"across midnight", at(2, 30), true},
		{"last minute", at(6, 59), true},
		{"window end", at(7, 0), false},
		{"midday", at(12, 0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.InQuietHours(c.t); got != c.want {
				t.Errorf("InQuietHours(%s) = %v, want %v", c.t, got, c.want)
			}
		})
	}

	// Disabled or unknown-timezone windows never match.
	if err := s.Set(nil, QuietHours{Enabled: false, StartMinutes: 0, EndMinutes: 1439, Timezone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	if s.InQuietHours(at(12, 0)) {
		t.Error("disabled window must never match")
	}
	if err := s.Set(nil, QuietHours{Enabled: true, StartMinutes: 0, EndMinutes: 1439, Timezone: "Not/AZone"}); err != nil {
		t.Fatal(err)
	}
	if s.InQuietHours(at(12, 0)) {
		t.Error("unresolvable timezone must disable the window")
	}

	// The window must survive a reload, and a categories-only SetMuted
	// must not clobber it.
	if err := s.Set([]string{"ALERT_WARNING"}, QuietHours{
		Enabled: true, StartMinutes: 22 * 60, EndMinutes: 7 * 60, Timezone: "UTC",
	}); err != nil {
		t.Fatal(err)
	}
	reloaded := NewPrefsStore(s.path)
	if !reloaded.InQuietHours(at(23, 0)) {
		t.Error("quiet hours must survive a reload")
	}
	if err := reloaded.SetMuted([]string{"POOL_DEGRADED"}); err != nil {
		t.Fatal(err)
	}
	if !reloaded.InQuietHours(at(23, 0)) {
		t.Error("SetMuted must preserve the quiet-hours window")
	}
}

func TestInterruptionLevelFor(t *testing.T) {
	critical := events.Event{Kind: events.KindPool, Severity: events.SeverityCritical}
	warning := events.Event{Kind: events.KindPool, Severity: events.SeverityWarning}

	if got := interruptionLevelFor(critical, false); got != "time-sensitive" {
		t.Errorf("critical outside quiet hours = %q, want time-sensitive", got)
	}
	if got := interruptionLevelFor(critical, true); got != "passive" {
		t.Errorf("critical during quiet hours = %q, want passive", got)
	}
	if got := interruptionLevelFor(warning, false); got != "" {
		t.Errorf("warning = %q, want empty (default level)", got)
	}
}

// A muted category must return before the dedupe mark, so un-muting
// lets the next re-broadcast push normally.
func TestHandleMutedSkipsDedupe(t *testing.T) {
	prefs := NewPrefsStore(filepath.Join(t.TempDir(), "prefs.json"))
	if err := prefs.SetMuted([]string{"ALERT_CRITICAL"}); err != nil {
		t.Fatal(err)
	}
	n := New(nil, "https://relay.test", "sub", "tok-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", prefs, nil)

	ev := events.Event{
		ID:       "ev-1",
		Kind:     events.KindAlert,
		Severity: events.SeverityCritical,
		Title:    "t",
		Summary:  "s",
	}
	n.handle(t.Context(), ev)

	if !n.markIfFresh(ev.ID) {
		t.Error("muted handle must not record a dedupe entry")
	}
}
