// Package pro keeps whether Zephyr Pro has lapsed, as the app reports it.
//
// The helper can't see Pro, so the app tells it: once the App Store
// confirms Pro has ended (a refund, or a plan that expired, not a payment
// it's still retrying), and again when it's back. While it's lapsed, what
// Pro switched on here is held: nothing booked runs (schedule), and no
// weekly digest goes out (digest). Nothing is switched off or forgotten,
// so all of it carries on when Pro is back.
package pro

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is whether Pro has lapsed, and since when.
type State struct {
	Lapsed bool       `json:"lapsed"`
	Since  *time.Time `json:"since,omitempty"`
}

// Store keeps the State in a JSON file.
type Store struct {
	mu    sync.Mutex
	path  string
	state State
}

// Open reads the file at path. A missing one hasn't lapsed; nor has an
// unreadable one, which is logged: what's booked runs rather than staying
// held on a file nobody can read.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		slog.Warn("pro: unreadable state; taking Pro as not lapsed", "path", path, "err", err)
		s.state = State{}
	}
	return s, nil
}

// Lapsed says whether Pro has lapsed. A nil Store's never has.
func (s *Store) Lapsed() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Lapsed
}

// Get is the current state.
func (s *Store) Get() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Set records whether Pro has lapsed, keeping when it first did.
func (s *Store) Set(lapsed bool, now time.Time) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := State{Lapsed: lapsed}
	if lapsed {
		next.Since = s.state.Since
		if !s.state.Lapsed || next.Since == nil {
			at := now.UTC().Truncate(time.Second)
			next.Since = &at
		}
	}
	data, err := json.Marshal(next)
	if err != nil {
		return State{}, err
	}
	if err := writeFile(s.path, data); err != nil {
		return State{}, err
	}
	s.state = next
	return next, nil
}

// writeFile replaces path with data, readable only by the helper, so a
// crash partway leaves the old file.
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

// Bookings is what a lapse holds in the schedule. *schedule.Store is one.
type Bookings interface {
	// Hold pauses everything booked, as the lapse's, and says how many.
	Hold(ctx context.Context) (int, error)
	// Release lifts the lapse's pauses: how many carry on, and how many
	// one-offs' times passed meanwhile.
	Release(ctx context.Context, now time.Time) (resumed, expired int, err error)
}

// Handler serves the state to the app: GET /pro, and PUT /pro, which the
// app sends when the App Store confirms Pro has lapsed, or that it's back.
type Handler struct {
	Store *Store
	// Bookings is nil when the helper has no schedule.
	Bookings Bookings
	now      func() time.Time
}

// RegisterMux adds the routes, each wrapped (in auth). Without its file
// (Store nil) they answer 503, rather than the 404 the app reads as a
// helper too old for them.
func (h *Handler) RegisterMux(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	route := func(pattern string, f http.HandlerFunc) {
		mux.Handle(pattern, wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h.Store == nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The helper couldn't open its Pro file. Its log says why."})
				return
			}
			f(w, r)
		})))
	}
	route("GET /pro", h.get)
	route("PUT /pro", h.put)
}

func (h *Handler) get(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.Store.Get())
}

// putResult is the state, with what the change did to what's booked:
// paused, carried on, or done with (a one-off whose time passed while Pro
// had lapsed).
type putResult struct {
	State
	Paused  int `json:"paused"`
	Resumed int `json:"resumed"`
	Expired int `json:"expired"`
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Lapsed *bool `json:"lapsed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.Lapsed == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Send lapsed."})
		return
	}
	now := time.Now
	if h.now != nil {
		now = h.now
	}
	state, err := h.Store.Set(*body.Lapsed, now())
	if err != nil {
		slog.Error("pro: couldn't save", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "The helper couldn't save that."})
		return
	}
	result := putResult{State: state}
	if h.Bookings != nil {
		// The scheduler does the same on its next tick, so a failure here
		// is caught up with in half a minute.
		if state.Lapsed {
			result.Paused, err = h.Bookings.Hold(r.Context())
		} else {
			result.Resumed, result.Expired, err = h.Bookings.Release(r.Context(), now())
		}
		if err != nil {
			slog.Error("pro: couldn't change what's booked; the scheduler will", "lapsed", state.Lapsed, "err", err)
		}
	}
	slog.Info("pro: the app says", "lapsed", state.Lapsed, "paused", result.Paused, "resumed", result.Resumed, "expired", result.Expired)
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
