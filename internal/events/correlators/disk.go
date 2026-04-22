package correlators

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

// DefaultDiskMergeWindow is long because a real disk-replacement story
// plays out over hours: fault → operator swaps the drive → resilver
// starts → resilver completes. A 24h merge window keeps re-fires of
// the same phase on the same pool folded onto one row. Distinct phases
// (fault / degraded / resilver-finished) have different dedupe keys so
// they stay as separate feed entries that together tell the narrative.
const DefaultDiskMergeWindow = 24 * time.Hour

// DiskReplacement watches the alert stream for ZFS fault / pool
// degraded / resilver completion signals and promotes them into
// narrative parent rows. Each alert child links to its parent via
// correlates_to so the feed can show one-row-per-phase for a pool
// without burying the original alerts.
//
// The correlator is stateless across restarts — matching is done per
// alert, not via cross-alert bookkeeping. If the helper restarts mid
// resilver, the finish alert still gets promoted correctly when it
// arrives, it just won't retroactively link to faults that predate
// the subscription.
type DiskReplacement struct {
	store *events.Store
	merge time.Duration
}

// NewDiskReplacement builds a correlator using DefaultDiskMergeWindow.
func NewDiskReplacement(store *events.Store) *DiskReplacement {
	return &DiskReplacement{store: store, merge: DefaultDiskMergeWindow}
}

// Run blocks until ctx is cancelled.
func (d *DiskReplacement) Run(ctx context.Context) {
	ch, cancel := d.store.Subscribe(64)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			d.handle(ctx, ev)
		}
	}
}

func (d *DiskReplacement) handle(ctx context.Context, child events.Event) {
	parent, ok := DiskParentFor(child)
	if !ok {
		return
	}
	stored, err := d.store.Ingest(ctx, parent, d.merge)
	if err != nil {
		slog.Warn("disk correlator: ingest", "err", err)
		return
	}
	if err := d.store.SetCorrelation(ctx, child.ID, stored.ID); err != nil {
		slog.Warn("disk correlator: link", "err", err, "child", child.ID, "parent", stored.ID)
	}
}

// DiskParentFor classifies an alert event as disk-fault, pool-degraded,
// or resilver-finished and returns the corresponding parent row to
// ingest. Returns ok=false for anything unrelated to disk health.
// Exported so the classification can be unit-tested without a store.
func DiskParentFor(child events.Event) (events.Event, bool) {
	if child.Kind != events.KindAlert || len(child.Body) == 0 {
		return events.Event{}, false
	}
	var raw map[string]any
	if err := json.Unmarshal(child.Body, &raw); err != nil {
		return events.Event{}, false
	}
	klass, _ := raw["klass"].(string)
	if !isZfsOrPoolKlass(klass) {
		return events.Event{}, false
	}
	formatted, _ := raw["formatted"].(string)
	phase := classifyDiskPhase(klass, formatted)
	if phase == "" {
		return events.Event{}, false
	}
	pool := extractPoolFromAlert(raw)

	kind := events.KindDisk
	if phase == "resilver-finished" || phase == "resilver-started" {
		kind = events.KindResilver
	}

	return events.Event{
		OccurredAt: child.OccurredAt,
		Kind:       kind,
		Severity:   diskSeverity(phase, child.Severity),
		Title:      diskTitle(phase, pool),
		Summary:    child.Summary,
		Body:       child.Body,
		DedupeKey:  diskDedupeKey(phase, pool),
	}, true
}

// isZfsOrPoolKlass gates the correlator to alerts that could plausibly
// be about a disk or pool. Matching on the full alert space would
// misfire on unrelated alerts whose messages happen to contain words
// like "offline" or "failed".
func isZfsOrPoolKlass(klass string) bool {
	k := strings.ToLower(klass)
	prefixes := []string{"zfs", "zpool", "volume", "pool", "disk", "smart"}
	for _, p := range prefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// classifyDiskPhase inspects klass + formatted message and returns
// one of: "fault", "degraded", "resilver-started", "resilver-finished",
// or "" for unrecognized.
func classifyDiskPhase(klass, message string) string {
	k := strings.ToLower(klass)
	m := strings.ToLower(message)

	// Resilver wins over other keywords — a resilver alert often
	// mentions "fault" incidentally. Check most-specific first.
	if strings.Contains(k, "resilver") || strings.Contains(m, "resilver") {
		if strings.Contains(m, "finish") || strings.Contains(m, "complete") || strings.Contains(k, "finished") {
			return "resilver-finished"
		}
		return "resilver-started"
	}
	switch {
	case strings.Contains(k, "degraded"), strings.Contains(m, "degraded"):
		return "degraded"
	case strings.Contains(k, "fault"), strings.Contains(m, "fault"):
		return "fault"
	case strings.Contains(k, "unavail"), strings.Contains(m, "unavail"):
		return "fault"
	case strings.Contains(k, "offline"), strings.Contains(m, "offline"):
		return "fault"
	case strings.Contains(k, "removed"), strings.Contains(m, "removed"):
		return "fault"
	}
	return ""
}

// extractPoolFromAlert pulls the pool name from a TrueNAS alert. Tries
// `args.pool` first (structured, reliable on newer TrueNAS), then falls
// back to regex-matching the formatted message. Returns "" when neither
// path yields a usable name.
func extractPoolFromAlert(raw map[string]any) string {
	if args, ok := raw["args"].(map[string]any); ok {
		for _, key := range []string{"pool", "vol", "volume", "name"} {
			if v, ok := args[key].(string); ok && v != "" {
				return v
			}
		}
	}
	if formatted, ok := raw["formatted"].(string); ok && formatted != "" {
		if pool := poolFromMessage(formatted); pool != "" {
			return pool
		}
	}
	if text, ok := raw["text"].(string); ok && text != "" {
		if pool := poolFromMessage(text); pool != "" {
			return pool
		}
	}
	return ""
}

// poolMessageRegex matches "Pool <name>" / "pool '<name>'" / 'pool "<name>"'.
// RE2 doesn't support backreferences so the optional outer quotes are
// matched independently — in practice the name charset excludes quotes
// so we never mis-capture them.
var poolMessageRegex = regexp.MustCompile(`(?i)\bpool\s+['"]?([A-Za-z0-9_\-]+)['"]?`)

func poolFromMessage(msg string) string {
	m := poolMessageRegex.FindStringSubmatch(msg)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

func diskSeverity(phase string, childSeverity events.Severity) events.Severity {
	switch phase {
	case "fault":
		return events.SeverityCritical
	case "degraded":
		// Degraded is serious but the pool still serves reads. Mirror
		// the underlying alert's severity rather than force critical —
		// TrueNAS sometimes emits a "still degraded after resilver"
		// at info level.
		return childSeverity
	case "resilver-finished":
		return events.SeverityInfo
	case "resilver-started":
		return events.SeverityInfo
	}
	return childSeverity
}

func diskTitle(phase, pool string) string {
	suffix := ""
	if pool != "" {
		suffix = " on " + pool
	}
	switch phase {
	case "fault":
		return "Disk fault detected" + suffix
	case "degraded":
		if pool != "" {
			return "Pool " + pool + " degraded"
		}
		return "Pool degraded"
	case "resilver-finished":
		if pool != "" {
			return "Resilver of " + pool + " finished"
		}
		return "Resilver finished"
	case "resilver-started":
		if pool != "" {
			return "Resilver of " + pool + " started"
		}
		return "Resilver started"
	}
	return "Disk event" + suffix
}

func diskDedupeKey(phase, pool string) string {
	if phase == "resilver-finished" || phase == "resilver-started" {
		return "resilver|" + pool + "|" + strings.TrimPrefix(phase, "resilver-")
	}
	return "disk|" + pool + "|" + phase
}
