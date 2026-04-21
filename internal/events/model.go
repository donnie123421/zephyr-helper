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
)

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
