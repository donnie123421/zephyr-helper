package correlators

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

func makeAlertEvent(t *testing.T, body map[string]any, severity events.Severity) events.Event {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	formatted, _ := body["formatted"].(string)
	return events.Event{
		ID:         "alert-id",
		OccurredAt: time.Unix(1714000000, 0).UTC(),
		Kind:       events.KindAlert,
		Severity:   severity,
		Title:      formatted,
		Summary:    formatted,
		Body:       raw,
		DedupeKey:  "alert|" + formatted,
	}
}

func TestDiskParentForZfsDegraded(t *testing.T) {
	child := makeAlertEvent(t, map[string]any{
		"klass":     "ZfsDegraded",
		"formatted": "Pool tank is degraded",
		"args":      map[string]any{"pool": "tank"},
	}, events.SeverityWarning)
	parent, ok := DiskParentFor(child)
	if !ok {
		t.Fatal("expected parent")
	}
	if parent.Kind != events.KindDisk {
		t.Errorf("kind = %s, want disk", parent.Kind)
	}
	if parent.Title != "Pool tank degraded" {
		t.Errorf("title = %q", parent.Title)
	}
	if parent.DedupeKey != "disk|tank|degraded" {
		t.Errorf("dedupe = %q", parent.DedupeKey)
	}
	// Degraded mirrors the child severity.
	if parent.Severity != events.SeverityWarning {
		t.Errorf("severity = %s, want warning", parent.Severity)
	}
}

func TestDiskParentForZfsFault(t *testing.T) {
	child := makeAlertEvent(t, map[string]any{
		"klass":     "ZfsFault",
		"formatted": "Disk offline in pool tank",
		"args":      map[string]any{"pool": "tank"},
	}, events.SeverityWarning)
	parent, ok := DiskParentFor(child)
	if !ok {
		t.Fatal("expected parent")
	}
	if parent.Title != "Disk fault detected on tank" {
		t.Errorf("title = %q", parent.Title)
	}
	// Fault always escalates to critical regardless of underlying alert level.
	if parent.Severity != events.SeverityCritical {
		t.Errorf("severity = %s, want critical", parent.Severity)
	}
	if parent.DedupeKey != "disk|tank|fault" {
		t.Errorf("dedupe = %q", parent.DedupeKey)
	}
}

func TestDiskParentForResilverFinished(t *testing.T) {
	child := makeAlertEvent(t, map[string]any{
		"klass":     "ZpoolResilverFinished",
		"formatted": "Resilver completed on pool tank",
		"args":      map[string]any{"pool": "tank"},
	}, events.SeverityInfo)
	parent, ok := DiskParentFor(child)
	if !ok {
		t.Fatal("expected parent")
	}
	if parent.Kind != events.KindResilver {
		t.Errorf("kind = %s, want resilver", parent.Kind)
	}
	if parent.Title != "Resilver of tank finished" {
		t.Errorf("title = %q", parent.Title)
	}
	if parent.DedupeKey != "resilver|tank|finished" {
		t.Errorf("dedupe = %q", parent.DedupeKey)
	}
}

func TestDiskParentForResilverStarted(t *testing.T) {
	child := makeAlertEvent(t, map[string]any{
		"klass":     "ZpoolResilverStart",
		"formatted": "Resilver started on pool tank",
		"args":      map[string]any{"pool": "tank"},
	}, events.SeverityInfo)
	parent, ok := DiskParentFor(child)
	if !ok {
		t.Fatal("expected parent")
	}
	if parent.Kind != events.KindResilver {
		t.Errorf("kind = %s, want resilver", parent.Kind)
	}
	if parent.DedupeKey != "resilver|tank|started" {
		t.Errorf("dedupe = %q", parent.DedupeKey)
	}
}

func TestDiskParentForFallsBackToMessage(t *testing.T) {
	// No args — pool name must be extracted from the formatted string.
	child := makeAlertEvent(t, map[string]any{
		"klass":     "VolumeStatus",
		"formatted": "Pool 'data' degraded",
	}, events.SeverityWarning)
	parent, ok := DiskParentFor(child)
	if !ok {
		t.Fatal("expected parent")
	}
	if parent.Title != "Pool data degraded" {
		t.Errorf("title = %q", parent.Title)
	}
}

func TestDiskParentForSkipsUnrelatedKlass(t *testing.T) {
	child := makeAlertEvent(t, map[string]any{
		"klass":     "RESTAPIUsage",
		"formatted": "API key used",
	}, events.SeverityWarning)
	if _, ok := DiskParentFor(child); ok {
		t.Error("expected skip for unrelated klass")
	}
}

func TestDiskParentForSkipsNonAlertKind(t *testing.T) {
	child := events.Event{
		Kind: events.KindJob,
		Body: json.RawMessage(`{"klass":"ZfsDegraded"}`),
	}
	if _, ok := DiskParentFor(child); ok {
		t.Error("expected skip for non-alert kind")
	}
}

func TestDiskParentForSkipsEmptyBody(t *testing.T) {
	child := events.Event{Kind: events.KindAlert}
	if _, ok := DiskParentFor(child); ok {
		t.Error("expected skip for empty body")
	}
}

func TestClassifyDiskPhase(t *testing.T) {
	cases := []struct {
		klass, msg, want string
	}{
		{"ZfsDegraded", "Pool tank is degraded", "degraded"},
		{"ZfsFault", "Disk unavailable", "fault"},
		{"ZpoolResilverFinished", "Resilver done", "resilver-finished"},
		{"ZpoolResilver", "Resilver started", "resilver-started"},
		{"VolumeStatus", "Pool offline", "fault"},
		{"PoolDisk", "Disk removed from pool", "fault"},
		{"ZfsCap", "Pool capacity is 90%", ""},
	}
	for _, c := range cases {
		got := classifyDiskPhase(c.klass, c.msg)
		if got != c.want {
			t.Errorf("classify(%q,%q) = %q, want %q", c.klass, c.msg, got, c.want)
		}
	}
}

func TestPoolFromMessage(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Pool tank is degraded", "tank"},
		{"pool 'data' has errors", "data"},
		{`pool "backup" resilvered`, "backup"},
		{"Device /dev/sda5 in pool my_pool_1 failed", "my_pool_1"},
		{"Something unrelated", ""},
	}
	for _, c := range cases {
		if got := poolFromMessage(c.in); got != c.want {
			t.Errorf("poolFromMessage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDiskRunEmitsParentAndLinksChild(t *testing.T) {
	dir := t.TempDir()
	store, err := events.Open(dir + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	correlator := NewDiskReplacement(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		correlator.Run(ctx)
		close(done)
	}()

	body, _ := json.Marshal(map[string]any{
		"klass":     "ZfsDegraded",
		"formatted": "Pool tank is degraded",
		"args":      map[string]any{"pool": "tank"},
	})
	child, err := store.Ingest(ctx, events.Event{
		OccurredAt: time.Unix(1714000000, 0).UTC(),
		Kind:       events.KindAlert,
		Severity:   events.SeverityWarning,
		Title:      "Pool tank is degraded",
		Summary:    "Pool tank is degraded",
		Body:       body,
		DedupeKey:  "alert|warning|ZfsDegraded|Pool tank is degraded",
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	var parent events.Event
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, err := store.List(ctx, events.ListFilter{Kind: events.KindDisk})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 {
			parent = list[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if parent.ID == "" {
		t.Fatal("correlator did not emit disk parent")
	}
	if parent.Title != "Pool tank degraded" {
		t.Errorf("parent title = %q", parent.Title)
	}

	refreshed, err := store.Get(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.CorrelatesTo == nil || *refreshed.CorrelatesTo != parent.ID {
		t.Errorf("child correlates_to = %v, want %s", refreshed.CorrelatesTo, parent.ID)
	}

	cancel()
	<-done
}
