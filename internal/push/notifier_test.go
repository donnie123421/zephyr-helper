package push

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

func TestShouldPush(t *testing.T) {
	cases := []struct {
		name string
		ev   events.Event
		want bool
	}{
		// Critical of any kind pushes.
		{"critical alert", events.Event{Kind: events.KindAlert, Severity: events.SeverityCritical}, true},
		{"critical pool", events.Event{Kind: events.KindPool, Severity: events.SeverityCritical}, true},
		{"critical security", events.Event{Kind: events.KindSecurity, Severity: events.SeverityCritical}, true},
		{"critical job", events.Event{Kind: events.KindJob, Severity: events.SeverityCritical}, true},

		// Warning pushes only for hardware / pool integrity kinds.
		{"warning pool", events.Event{Kind: events.KindPool, Severity: events.SeverityWarning}, true},
		{"warning disk", events.Event{Kind: events.KindDisk, Severity: events.SeverityWarning}, true},
		{"warning resilver", events.Event{Kind: events.KindResilver, Severity: events.SeverityWarning}, true},
		{"warning scrub", events.Event{Kind: events.KindScrub, Severity: events.SeverityWarning}, true},

		// The pool rebuilt after a disk replacement: worth knowing.
		{"resilver finished", events.Event{Kind: events.KindResilver, Severity: events.SeverityInfo,
			DedupeKey: events.RebuiltKeyPrefix + "backup|1791090720000"}, true},
		{"resilver started", events.Event{Kind: events.KindResilver, Severity: events.SeverityInfo,
			DedupeKey: "resilver|backup|started"}, false},
		// The weekly digest notifies.
		{"weekly digest", events.Event{Kind: events.KindJob, Severity: events.SeverityInfo,
			DedupeKey: events.DigestKeyPrefix + "abc123"}, true},

		// Warning for noisy kinds stays feed-only.
		{"warning alert", events.Event{Kind: events.KindAlert, Severity: events.SeverityWarning}, false},
		{"warning job", events.Event{Kind: events.KindJob, Severity: events.SeverityWarning}, false},
		{"warning security", events.Event{Kind: events.KindSecurity, Severity: events.SeverityWarning}, false},

		// A job the user approved in a chat pushes when it fails later;
		// failing in the chat, in front of them, doesn't.
		{"assistant job failed", events.Event{Kind: events.KindJob, Severity: events.SeverityWarning,
			DedupeKey: events.AssistantKey("a1", events.AssistantJobFailed)}, true},
		{"assistant action failed in the chat", events.Event{Kind: events.KindJob, Severity: events.SeverityWarning,
			DedupeKey: events.AssistantKey("a1", events.AssistantRan)}, false},
		{"assistant job done", events.Event{Kind: events.KindJob, Severity: events.SeverityInfo,
			DedupeKey: events.AssistantKey("a1", events.AssistantJobDone)}, false},
		// Booked work and rules run unwatched: failures notify, the rest
		// stays in the feed. A check-in's briefing always notifies.
		{"scheduled run failed", events.Event{Kind: events.KindJob, Severity: events.SeverityWarning,
			DedupeKey: events.AssistantKey("j1-a", events.AssistantScheduled)}, true},
		{"scheduled run done", events.Event{Kind: events.KindJob, Severity: events.SeverityInfo,
			DedupeKey: events.AssistantKey("j1-a", events.AssistantScheduled)}, false},
		{"rule rolled an app back", events.Event{Kind: events.KindJob, Severity: events.SeverityWarning,
			DedupeKey: events.AssistantKey("r1-a", events.AssistantRule)}, true},
		{"check-in ready", events.Event{Kind: events.KindJob, Severity: events.SeverityInfo,
			DedupeKey: events.AssistantKey("c1-a", events.AssistantCheckIn)}, true},

		// Info never pushes.
		{"info alert", events.Event{Kind: events.KindAlert, Severity: events.SeverityInfo}, false},
		{"info pool", events.Event{Kind: events.KindPool, Severity: events.SeverityInfo}, false},
		{"info security", events.Event{Kind: events.KindSecurity, Severity: events.SeverityInfo}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldPush(c.ev); got != c.want {
				t.Errorf("shouldPush(%s/%s) = %v, want %v",
					c.ev.Kind, c.ev.Severity, got, c.want)
			}
		})
	}
}

func TestCategoryFor(t *testing.T) {
	cases := []struct {
		name string
		ev   events.Event
		want string
	}{
		{"critical pool", events.Event{Kind: events.KindPool, Severity: events.SeverityCritical}, "POOL_DEGRADED"},
		{"critical security", events.Event{Kind: events.KindSecurity, Severity: events.SeverityCritical}, "SECURITY_CRITICAL"},
		{"critical alert", events.Event{Kind: events.KindAlert, Severity: events.SeverityCritical}, "ALERT_CRITICAL"},
		{"critical disk", events.Event{Kind: events.KindDisk, Severity: events.SeverityCritical}, "ALERT_CRITICAL"},
		{"warning pool", events.Event{Kind: events.KindPool, Severity: events.SeverityWarning}, "ALERT_WARNING"},
		{"warning disk", events.Event{Kind: events.KindDisk, Severity: events.SeverityWarning}, "ALERT_WARNING"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := categoryFor(c.ev); got != c.want {
				t.Errorf("categoryFor(%s/%s) = %q, want %q",
					c.ev.Kind, c.ev.Severity, got, c.want)
			}
		})
	}
}

// markIfFresh must suppress a second push of the same id within the
// dedupe window, and the timestamp record must be atomic with the check.
func TestMarkIfFresh(t *testing.T) {
	n := New(nil, "https://relay.test", "sub", "tok-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, nil)

	if !n.markIfFresh("event-1") {
		t.Fatal("first markIfFresh should return true")
	}
	if n.markIfFresh("event-1") {
		t.Error("second markIfFresh within window should return false")
	}
	if !n.markIfFresh("event-2") {
		t.Error("a different id should still be fresh")
	}

	// forget clears the entry so a retry can push again.
	n.forget("event-1")
	if !n.markIfFresh("event-1") {
		t.Error("markIfFresh after forget should return true")
	}
}

// Ready closes once the notifier is listening, or straight away when push
// isn't set up, so the scheduler never waits on it for nothing.
func TestReadyClosesWhenListeningOrDisabled(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	disabled := New(store, "", "", "", nil, nil)
	go disabled.Run(ctx)
	select {
	case <-disabled.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("a disabled notifier never said it was ready")
	}

	listening := New(store, "http://127.0.0.1:1", "sub", "token", nil, nil)
	go listening.Run(ctx)
	select {
	case <-listening.Ready():
		if store.SubscriberCount() == 0 {
			t.Error("ready before subscribing")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never ready")
	}
}
