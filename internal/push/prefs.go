package push

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// PrefsStore holds the iOS-synced notification preferences — today just
// the set of push categories the user muted in the app's Settings.
// Backed by a small JSON file so the choice survives helper restarts
// (the iOS app also re-syncs on every foreground, which covers deploys
// that predate the /data volume).
//
// Why this exists: the iOS per-category toggles used to gate only the
// app's *local* polling notifications — server-side APNs pushes ignored
// them entirely. The notifier consults this store so muting a category
// actually mutes it.
type PrefsStore struct {
	path string

	mu    sync.RWMutex
	muted map[string]struct{}
}

// prefsFile is the on-disk shape. Categories are stored as a sorted
// list for stable diffs when eyeballing the file.
type prefsFile struct {
	MutedCategories []string `json:"muted_categories"`
}

// MaxMutedCategories bounds a POST body so a buggy client can't grow
// the file without limit. Far above the real category count (~6).
const MaxMutedCategories = 32

// NewPrefsStore loads existing prefs from `path`. A missing file is a
// valid state (nothing muted); a corrupt file is treated the same and
// overwritten on the next save rather than wedging push entirely.
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
	return s
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

// SetMuted replaces the muted set and persists it. The write is
// tmp+rename so a crash mid-save can't leave a half-written file.
func (s *PrefsStore) SetMuted(categories []string) error {
	next := make(map[string]struct{}, len(categories))
	for _, c := range categories {
		next[c] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.muted = next

	f := prefsFile{MutedCategories: make([]string, 0, len(next))}
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
