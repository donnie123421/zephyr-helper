package push

import (
	"os"
	"path/filepath"
	"testing"

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
