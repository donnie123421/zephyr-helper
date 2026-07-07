package push

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// PrefsStore holds the iOS-synced notification preferences: the set of
// push categories the user muted in the app's Settings, plus an
// optional quiet-hours window. Backed by a small JSON file so the
// choices survive helper restarts (the iOS app also re-syncs on every
// foreground, which covers deploys that predate the /data volume).
//
// Why this exists: the iOS per-category toggles used to gate only the
// app's *local* polling notifications — server-side APNs pushes ignored
// them entirely. The notifier consults this store so muting a category
// actually mutes it, and so overnight pushes respect quiet hours.
type PrefsStore struct {
	path string

	mu    sync.RWMutex
	muted map[string]struct{}
	quiet QuietHours
	// loc is the resolved quiet-hours timezone, cached because
	// time.LoadLocation reads (embedded) tzdata. Nil when quiet hours
	// are disabled or the zone failed to resolve.
	loc *time.Location
}

// QuietHours is a daily do-not-disturb window in the user's local
// time. During the window the notifier drops warning-level pushes
// entirely and delivers critical ones with a passive interruption
// level (visible in Notification Center, no wake/sound). Minutes are
// since local midnight; a start after the end means the window wraps
// past midnight (the normal 22:00 → 07:00 case).
type QuietHours struct {
	Enabled      bool   `json:"enabled"`
	StartMinutes int    `json:"start_minutes"`
	EndMinutes   int    `json:"end_minutes"`
	Timezone     string `json:"timezone"`
}

// prefsFile is the on-disk shape. Categories are stored as a sorted
// list for stable diffs when eyeballing the file.
type prefsFile struct {
	MutedCategories []string   `json:"muted_categories"`
	QuietHours      QuietHours `json:"quiet_hours"`
}

// MaxMutedCategories bounds a POST body so a buggy client can't grow
// the file without limit. Far above the real category count (~6).
const MaxMutedCategories = 32

// NewPrefsStore loads existing prefs from `path`. A missing file is a
// valid state (nothing muted, no quiet hours); a corrupt file is
// treated the same and overwritten on the next save rather than
// wedging push entirely.
func NewPrefsStore(path string) *PrefsStore {
	s := &PrefsStore{path: path, muted: make(map[string]struct{})}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var f prefsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return s
	}
	for _, c := range f.MutedCategories {
		s.muted[c] = struct{}{}
	}
	s.quiet = f.QuietHours
	s.loc = resolveLocation(f.QuietHours)
	return s
}

// resolveLocation loads the quiet-hours timezone. An unknown zone
// disables the window rather than silently evaluating in the wrong
// one — a mis-timed do-not-disturb is worse than none.
func resolveLocation(q QuietHours) *time.Location {
	if !q.Enabled || q.Timezone == "" {
		return nil
	}
	loc, err := time.LoadLocation(q.Timezone)
	if err != nil {
		return nil
	}
	return loc
}

// IsMuted reports whether pushes of this category should be suppressed.
func (s *PrefsStore) IsMuted(category string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.muted[category]
	return ok
}

// Muted returns the muted categories, sorted.
func (s *PrefsStore) Muted() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.muted))
	for c := range s.muted {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// QuietHours returns the configured window.
func (s *PrefsStore) QuietHours() QuietHours {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.quiet
}

// InQuietHours reports whether `now` falls inside the configured
// window, evaluated in the window's own timezone. Always false when
// disabled, when the timezone didn't resolve, or for a zero-length
// window.
func (s *PrefsStore) InQuietHours(now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.quiet.Enabled || s.loc == nil || s.quiet.StartMinutes == s.quiet.EndMinutes {
		return false
	}
	local := now.In(s.loc)
	minutes := local.Hour()*60 + local.Minute()
	start, end := s.quiet.StartMinutes, s.quiet.EndMinutes
	if start < end {
		return minutes >= start && minutes < end
	}
	// Window wraps past midnight (e.g. 22:00 → 07:00).
	return minutes >= start || minutes < end
}

// SetMuted replaces only the muted set, preserving the quiet-hours
// window. Kept for callers (and tests) that don't care about the
// window.
func (s *PrefsStore) SetMuted(categories []string) error {
	return s.Set(categories, s.QuietHours())
}

// Set replaces both the muted set and the quiet-hours window and
// persists them. The write is tmp+rename so a crash mid-save can't
// leave a half-written file.
func (s *PrefsStore) Set(categories []string, quiet QuietHours) error {
	next := make(map[string]struct{}, len(categories))
	for _, c := range categories {
		next[c] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.muted = next
	s.quiet = quiet
	s.loc = resolveLocation(quiet)

	f := prefsFile{
		MutedCategories: make([]string, 0, len(next)),
		QuietHours:      quiet,
	}
	for c := range next {
		f.MutedCategories = append(f.MutedCategories, c)
	}
	sort.Strings(f.MutedCategories)

	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal prefs: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".push_prefs-*")
	if err != nil {
		return fmt.Errorf("prefs temp file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write prefs: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("close prefs: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("rename prefs: %w", err)
	}
	return nil
}
