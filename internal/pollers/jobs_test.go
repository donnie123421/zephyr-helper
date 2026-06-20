package pollers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

func TestJobToEventSuccess(t *testing.T) {
	raw := `{
		"id": 123,
		"method": "pool.scrub",
		"state": "SUCCESS",
		"time_finished": {"$date": 1714000000000}
	}`
	var j map[string]any
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		t.Fatal(err)
	}
	ev, ok := jobToEvent(j)
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.Severity != events.SeverityInfo {
		t.Errorf("severity = %s, want info", ev.Severity)
	}
	if ev.Kind != events.KindJob {
		t.Errorf("kind = %s, want job", ev.Kind)
	}
	if ev.Title != "pool.scrub succeeded" {
		t.Errorf("title = %q", ev.Title)
	}
	if ev.DedupeKey != "job|pool.scrub|success" {
		t.Errorf("dedupe = %q", ev.DedupeKey)
	}
	if ev.OccurredAt.IsZero() {
		t.Error("occurred_at zero")
	}
}

func TestJobToEventSuccessFiltering(t *testing.T) {
	cases := []struct {
		method  string
		surface bool
	}{
		// Routine internal / boot successes — dropped.
		{"smb.configure", false},
		{"smb.synchronize_passdb", false},
		{"service.control", false},
		{"directoryservices.setup", false},
		{"initshutdownscript.execute_init_tasks", false},
		{"pool.dataset.unlock", false},
		{"pool.scrub_impl", false}, // private impl never surfaces
		{"core.bulk", false},
		// Correlator-fed — kept so the parent narrative row can form.
		{"pool.scrub", true},
		{"pool.snapshottask.run", true},
		{"zettarepl.replicate", true},
		// User-meaningful standalone operations — kept.
		{"update.run", true},
		{"replication.run", true},
		{"cloudsync.sync", true},
		{"pool.create", true},
	}
	for _, tc := range cases {
		j := map[string]any{
			"id":            float64(1),
			"method":        tc.method,
			"state":         "SUCCESS",
			"time_finished": map[string]any{"$date": float64(1714000000000)},
		}
		if _, ok := jobToEvent(j); ok != tc.surface {
			t.Errorf("jobToEvent(SUCCESS %s) surfaced=%v, want %v", tc.method, ok, tc.surface)
		}
	}
}

// A failure surfaces no matter how routine the method is — the success filter
// must not swallow actionable failures.
func TestJobToEventFailureAlwaysSurfaces(t *testing.T) {
	for _, method := range []string{"smb.configure", "service.control", "pool.dataset.unlock"} {
		j := map[string]any{
			"id":            float64(1),
			"method":        method,
			"state":         "FAILED",
			"error":         "boom",
			"time_finished": map[string]any{"$date": float64(1714000000000)},
		}
		if _, ok := jobToEvent(j); !ok {
			t.Errorf("jobToEvent(FAILED %s) should surface", method)
		}
	}
}

func TestJobToEventFailedIncludesError(t *testing.T) {
	raw := `{
		"id": 456,
		"method": "replication.run",
		"state": "FAILED",
		"error": "SSH connection refused\nat layer X",
		"time_finished": {"$date": 1714000000000}
	}`
	var j map[string]any
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		t.Fatal(err)
	}
	ev, ok := jobToEvent(j)
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.Severity != events.SeverityWarning {
		t.Errorf("severity = %s, want warning", ev.Severity)
	}
	if !strings.Contains(ev.Summary, "SSH connection refused") {
		t.Errorf("summary %q missing error detail", ev.Summary)
	}
	// Stack traces are noisy; we keep just the first line.
	if strings.Contains(ev.Summary, "at layer X") {
		t.Errorf("summary should have trimmed multi-line error: %q", ev.Summary)
	}
}

func TestJobToEventAborted(t *testing.T) {
	j := map[string]any{
		"id":            float64(789),
		"method":        "cloud_sync.sync",
		"state":         "ABORTED",
		"time_finished": map[string]any{"$date": float64(1714000000000)},
	}
	ev, ok := jobToEvent(j)
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.Severity != events.SeverityWarning {
		t.Errorf("severity = %s, want warning", ev.Severity)
	}
	if ev.Title != "cloud_sync.sync aborted" {
		t.Errorf("title = %q", ev.Title)
	}
}

func TestJobToEventSkipsRunning(t *testing.T) {
	j := map[string]any{
		"id":     float64(1),
		"method": "pool.scrub",
		"state":  "RUNNING",
	}
	if _, ok := jobToEvent(j); ok {
		t.Error("expected skip for non-terminal state")
	}
}

func TestJobToEventSkipsWhenMethodMissing(t *testing.T) {
	j := map[string]any{"id": float64(1), "state": "SUCCESS"}
	if _, ok := jobToEvent(j); ok {
		t.Error("expected skip when method missing")
	}
}

func TestJobToEventTruncatesLongSummary(t *testing.T) {
	long := strings.Repeat("x", 500)
	j := map[string]any{
		"id":     float64(1),
		"method": "m",
		"state":  "FAILED",
		"error":  long,
	}
	ev, ok := jobToEvent(j)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(ev.Summary) != 240 {
		t.Errorf("summary len = %d, want 240", len(ev.Summary))
	}
}

func TestIsTerminalJobState(t *testing.T) {
	cases := map[string]bool{
		"SUCCESS": true,
		"FAILED":  true,
		"ABORTED": true,
		"RUNNING": false,
		"WAITING": false,
		"":        false,
	}
	for k, want := range cases {
		if isTerminalJobState(k) != want {
			t.Errorf("isTerminalJobState(%q) != %v", k, want)
		}
	}
}

func TestReadJobID(t *testing.T) {
	// JSON numbers decode to float64 through interface{}.
	j := map[string]any{"id": float64(42)}
	if id, ok := readJobID(j); !ok || id != 42 {
		t.Errorf("got %d, %v; want 42, true", id, ok)
	}
	if _, ok := readJobID(map[string]any{"id": "oops"}); ok {
		t.Error("expected false for string id")
	}
	if _, ok := readJobID(map[string]any{}); ok {
		t.Error("expected false when id missing")
	}
}

func TestJobSeverity(t *testing.T) {
	cases := map[string]events.Severity{
		"SUCCESS": events.SeverityInfo,
		"FAILED":  events.SeverityWarning,
		"ABORTED": events.SeverityWarning,
	}
	for state, want := range cases {
		if got := jobSeverity(state); got != want {
			t.Errorf("jobSeverity(%q) = %s, want %s", state, got, want)
		}
	}
}
