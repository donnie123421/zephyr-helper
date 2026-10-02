// Package events is the helper's local record of things that have
// happened on the NAS — alerts, job outcomes, scrub completions, disk
// faults, resilvers. Events land here from background pollers (v1.1) and
// are served to the iOS client through the /events HTTP + WS API.
//
// v1.1 ships the store + HTTP surface. Pollers and correlators arrive in
// follow-up commits so the store can be exercised (and the iOS feed can
// render synthetic data) independently from the TrueNAS integration.
package events

import (
	"encoding/json"
	"strings"
	"time"
)

// Severity is a coarse classification shared with the iOS side so chips
// can colour-code rows without interpreting TrueNAS-specific strings.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// Kind is the event family. Correlators group children of the same kind
// under a parent of a different kind — e.g. three `job` rows rolling up
// into a single `snapshot` summary.
type Kind string

const (
	KindAlert    Kind = "alert"
	KindJob      Kind = "job"
	KindScrub    Kind = "scrub"
	KindResilver Kind = "resilver"
	KindDisk     Kind = "disk"
	KindPool     Kind = "pool"
	KindSnapshot Kind = "snapshot"
	KindSecurity Kind = "security"
)

// RebuiltKeyPrefix starts the key of a resilver-finished event, keyed by
// pool and end time: the news worth a notification after a disk is
// replaced. The resilver watcher (pollers.Resilvers) records them.
const RebuiltKeyPrefix = "pool-rebuilt|"

// The assistant's actions are recorded as job events, which every app
// version can show: older apps reject a feed holding a kind they don't
// know. Their dedupe keys start with AssistantKeyPrefix, which is how newer
// apps and the push policy tell them apart.
const AssistantKeyPrefix = "assistant|"

// The entries an assistant action can leave.
const (
	// AssistantRan: it ran, and was done, started a job or failed.
	AssistantRan = "ran"
	// AssistantJobDone: a job it started ended well.
	AssistantJobDone = "job_done"
	// AssistantJobFailed: a job it started failed or was cancelled.
	AssistantJobFailed = "job_failed"
	// AssistantScheduled: something booked ran, or didn't, on its own.
	AssistantScheduled = "scheduled"
	// AssistantRule: what a standing rule did on one of its runs.
	AssistantRule = "rule"
	// AssistantCheckIn: a check-in's briefing is ready.
	AssistantCheckIn = "checkin"
)

// AssistantKey is the dedupe key of one entry for one action.
func AssistantKey(actionID, entry string) string {
	return AssistantKeyPrefix + actionID + "|" + entry
}

// IsAssistantJobFailure reports whether ev is a job an assistant action
// started failing, which happens after the chat has moved on.
func IsAssistantJobFailure(ev Event) bool {
	return assistantEntry(ev) == AssistantJobFailed
}

// IsAssistantPush reports whether an assistant entry should notify: what
// happened while the user wasn't watching and went wrong (a job failing
// after the chat moved on, something booked or a rule going wrong), and a
// check-in's briefing being ready, which the user asked to be told of.
func IsAssistantPush(ev Event) bool {
	switch assistantEntry(ev) {
	case AssistantJobFailed:
		return true
	case AssistantScheduled, AssistantRule:
		return ev.Severity == SeverityWarning || ev.Severity == SeverityCritical
	case AssistantCheckIn:
		return true
	}
	return false
}

// assistantEntry is which entry for an assistant action ev is, or "".
func assistantEntry(ev Event) string {
	if !strings.HasPrefix(ev.DedupeKey, AssistantKeyPrefix) {
		return ""
	}
	i := strings.LastIndexByte(ev.DedupeKey, '|')
	return ev.DedupeKey[i+1:]
}

// Event is one row in the store. `Body` holds the original TrueNAS
// payload verbatim so the detail view can surface raw data and so we can
// re-derive summaries without a second API round-trip.
type Event struct {
	ID           string          `json:"id"`
	OccurredAt   time.Time       `json:"occurred_at"`
	FirstSeen    time.Time       `json:"first_seen"`
	LastSeen     time.Time       `json:"last_seen"`
	Count        int             `json:"count"`
	Kind         Kind            `json:"kind"`
	Severity     Severity        `json:"severity"`
	Title        string          `json:"title"`
	Summary      string          `json:"summary"`
	Body         json.RawMessage `json:"body,omitempty"`
	DedupeKey    string          `json:"dedupe_key"`
	CorrelatesTo *string         `json:"correlates_to,omitempty"`
	AckedAt      *time.Time      `json:"acked_at,omitempty"`
}

// ListFilter is the query shape for Store.List. Any zero-valued field is
// treated as unset. `Limit` is clamped by the store; callers can rely on
// the store to enforce a sensible upper bound.
type ListFilter struct {
	Since    time.Time
	Kind     Kind
	Severity Severity
	Limit    int
}
