package digest

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// keep is how many weeks are kept.
const keep = 12

// Store keeps the last few digests in a JSON file, newest first.
type Store struct {
	mu      sync.Mutex
	path    string
	digests []Digest
}

// OpenStore reads the file at path. A missing one is an empty store; an
// unreadable one is set aside, so the digest carries on without it.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &s.digests); err != nil {
		setAside(path, err)
		s.digests = nil
	}
	return s, nil
}

// setAside renames a file that can't be read, keeping it for a look, and
// logs why.
func setAside(path string, why error) {
	aside := fmt.Sprintf("%s.unreadable-%d", path, time.Now().Unix())
	if err := os.Rename(path, aside); err != nil {
		slog.Error("digest: couldn't set aside an unreadable file", "path", path, "err", err)
		return
	}
	slog.Warn("digest: set aside an unreadable file, starting afresh", "path", path, "aside", aside, "err", why)
}

// Add keeps a digest, dropping the oldest past keep.
func (s *Store) Add(d Digest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]Digest{d}, s.digests...)
	if len(next) > keep {
		next = next[:keep]
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := writeFile(s.path, data); err != nil {
		return err
	}
	s.digests = next
	return nil
}

// Latest is the newest digest kept.
func (s *Store) Latest() (Digest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.digests) == 0 {
		return Digest{}, false
	}
	return s.digests[0], true
}

// List is the digests kept, newest first: empty, not nil, when there are
// none.
func (s *Store) List() []Digest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Digest{}, s.digests...)
}

// Get finds one by id.
func (s *Store) Get(id string) (Digest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.digests {
		if d.ID == id {
			return d, true
		}
	}
	return Digest{}, false
}

// writeFile writes owner-only, through a temporary file, so a crash
// mid-write can't leave half a file.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Settings are whether the weekly digest is on, and whether a connected
// model writes its summary. Off until the app turns it on: the digest is
// part of Zephyr Pro, which the helper can't see.
type Settings struct {
	Enabled bool `json:"enabled"`
	Summary bool `json:"summary"`
	// EnabledAt is when it was switched on: the first digest is the
	// Sunday after.
	EnabledAt *time.Time `json:"enabled_at,omitempty"`
}

// SettingsStore keeps Settings in a JSON file.
type SettingsStore struct {
	mu       sync.Mutex
	path     string
	settings Settings
}

// OpenSettings reads the file at path. A missing one is off, with the
// summary on for when it's switched on; an unreadable one is set aside, and
// off.
func OpenSettings(path string) (*SettingsStore, error) {
	s := &SettingsStore{path: path, settings: Settings{Summary: true}}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &s.settings); err != nil {
		setAside(path, err)
		s.settings = Settings{Summary: true}
	}
	return s, nil
}

// Get is the current settings.
func (s *SettingsStore) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// Set saves new settings, noting when the digest is switched on, and
// returns them.
func (s *SettingsStore) Set(next Settings, now time.Time) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !next.Enabled:
		next.EnabledAt = nil
	case s.settings.Enabled && s.settings.EnabledAt != nil:
		next.EnabledAt = s.settings.EnabledAt
	default:
		at := now.UTC().Truncate(time.Second)
		next.EnabledAt = &at
	}
	data, err := json.Marshal(next)
	if err != nil {
		return Settings{}, err
	}
	if err := writeFile(s.path, data); err != nil {
		return Settings{}, err
	}
	s.settings = next
	return next, nil
}
