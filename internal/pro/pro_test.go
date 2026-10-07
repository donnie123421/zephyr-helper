package pro

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTheStateLastsAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pro.json")
	s, err := Open(path)
	if err != nil || s.Lapsed() {
		t.Fatalf("a new helper: lapsed %v, %v", s.Lapsed(), err)
	}
	first := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	if _, err := s.Set(true, first); err != nil {
		t.Fatal(err)
	}
	// Said again, it keeps when Pro first lapsed.
	state, _ := s.Set(true, first.Add(time.Hour))
	if !state.Lapsed || state.Since == nil || !state.Since.Equal(first) {
		t.Errorf("state = %+v", state)
	}
	again, _ := Open(path)
	if got := again.Get(); !got.Lapsed || !got.Since.Equal(first) {
		t.Errorf("after a restart: %+v", got)
	}
	if state, _ := again.Set(false, first.Add(2*time.Hour)); state.Lapsed || state.Since != nil {
		t.Errorf("back: %+v", state)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
}

// What's booked runs rather than staying held on a file nobody can read.
func TestAnUnreadableFileIsntLapsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pro.json")
	_ = os.WriteFile(path, []byte("{"), 0o600)
	s, err := Open(path)
	if err != nil || s.Lapsed() {
		t.Errorf("lapsed %v, %v", s.Lapsed(), err)
	}
	var none *Store
	if none.Lapsed() {
		t.Error("a helper without the file lapsed")
	}
}

type fakeBookings struct {
	held     bool
	released time.Time
}

func (f *fakeBookings) Hold(context.Context) (int, error) {
	f.held = true
	return 3, nil
}

func (f *fakeBookings) Release(_ context.Context, now time.Time) (int, int, error) {
	f.held, f.released = false, now
	return 2, 1, nil
}

func TestTheAppsRequests(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "pro.json"))
	bookings := &fakeBookings{}
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	(&Handler{Store: s, Bookings: bookings, now: func() time.Time { return now }}).RegisterMux(mux, func(h http.Handler) http.Handler { return h })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	do := func(method, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+"/pro", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		return resp.StatusCode, got
	}

	if code, got := do("PUT", `{"lapsed": true}`); code != 200 || got["lapsed"] != true || got["paused"] != 3.0 || !bookings.held {
		t.Errorf("lapsed = %d %v", code, got)
	}
	if code, got := do("GET", ""); code != 200 || got["lapsed"] != true || got["since"] == nil {
		t.Errorf("get = %d %v", code, got)
	}
	if code, got := do("PUT", `{"lapsed": false}`); code != 200 || got["lapsed"] != false || got["resumed"] != 2.0 ||
		got["expired"] != 1.0 || bookings.held || !bookings.released.Equal(now) {
		t.Errorf("back = %d %v", code, got)
	}
	if code, _ := do("PUT", `{}`); code != 400 {
		t.Errorf("no lapsed = %d", code)
	}

	// Without its file the routes answer, with 503, rather than going
	// missing: the app reads a 404 as a helper too old for them.
	bare := http.NewServeMux()
	(&Handler{}).RegisterMux(bare, func(h http.Handler) http.Handler { return h })
	rec := httptest.NewRecorder()
	bare.ServeHTTP(rec, httptest.NewRequest("GET", "/pro", nil))
	if rec.Code != 503 {
		t.Errorf("without the file = %d", rec.Code)
	}
}
