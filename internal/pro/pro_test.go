package pro

import (
	"context"
	"encoding/json"
	"fmt"
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
	if _, err := s.Set("phone", Lapsed, first); err != nil {
		t.Fatal(err)
	}
	// Said again, it keeps when Pro first lapsed.
	state, _ := s.Set("phone", Lapsed, first.Add(time.Hour))
	if !state.Lapsed || state.Since == nil || !state.Since.Equal(first) {
		t.Errorf("state = %+v", state)
	}
	again, _ := Open(path)
	if got := again.Get(); !got.Lapsed || !got.Since.Equal(first) {
		t.Errorf("after a restart: %+v", got)
	}
	if state, _ := again.Set("phone", Active, first.Add(2*time.Hour)); state.Lapsed || state.Since != nil {
		t.Errorf("back: %+v", state)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
}

// One household member's lapsed trial doesn't pause what another pays
// for: it's lapsed while some phone says so and none says it's active.
func TestEachPhoneSaysForItsOwnAccount(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "pro.json"))
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	steps := []struct {
		device, pro string
		lapsed      bool
	}{
		{"never", Never, false},
		{"trial", Lapsed, true},
		{"payer", Active, false},
		{"trial", Lapsed, false},
		{"payer", Lapsed, true},
		{"never", Never, true},
		{"payer", Active, false},
	}
	for i, step := range steps {
		state, err := s.Set(step.device, step.pro, now.Add(time.Duration(i)*time.Minute))
		if err != nil || state.Lapsed != step.lapsed {
			t.Errorf("step %d (%s says %s): lapsed %v, %v", i, step.device, step.pro, state.Lapsed, err)
		}
	}
}

func TestOnlySoManyPhonesAreRemembered(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "pro.json"))
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	_, _ = s.Set("first", Lapsed, now)
	for i := 0; i < maxDevices; i++ {
		_, _ = s.Set(fmt.Sprint("phone", i), Never, now.Add(time.Duration(i+1)*time.Minute))
	}
	state := s.Get()
	if len(state.Devices) != maxDevices || state.Lapsed {
		t.Errorf("%d phones, lapsed %v: the oldest goes first", len(state.Devices), state.Lapsed)
	}
}

// With the file unwritable (a full /data, say), what a phone says takes
// effect anyway, until the helper restarts; the error has the phone say it
// again.
func TestAnUnwritableFileStillTakesEffect(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "pro.json"))
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	_, _ = s.Set("phone", Lapsed, now)
	s.path = filepath.Join(dir, "missing", "pro.json")
	state, err := s.Set("phone", Active, now.Add(time.Minute))
	if err == nil || state.Lapsed || s.Lapsed() {
		t.Errorf("back with the file unwritable: lapsed %v (now %v), %v", state.Lapsed, s.Lapsed(), err)
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

func (f *fakeBookings) Converge(_ context.Context, lapsed func() bool, now time.Time) (int, int, int, error) {
	if lapsed() {
		f.held = true
		return 3, 0, 0, nil
	}
	f.held, f.released = false, now
	return 0, 2, 1, nil
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

	if code, got := do("PUT", `{"device": "phone", "pro": "lapsed"}`); code != 200 || got["lapsed"] != true || got["paused"] != 3.0 || !bookings.held {
		t.Errorf("lapsed = %d %v", code, got)
	}
	if code, got := do("GET", ""); code != 200 || got["lapsed"] != true || got["since"] == nil || got["devices"] != nil {
		t.Errorf("get = %d %v", code, got)
	}
	if code, got := do("PUT", `{"device": "phone", "pro": "active"}`); code != 200 || got["lapsed"] != false || got["resumed"] != 2.0 ||
		got["expired"] != 1.0 || bookings.held || !bookings.released.Equal(now) {
		t.Errorf("back = %d %v", code, got)
	}
	for _, bad := range []string{`{}`, `{"device": "phone"}`, `{"device": "phone", "pro": "maybe"}`, `{"pro": "active"}`,
		`{"device": "` + strings.Repeat("x", 65) + `", "pro": "active"}`} {
		if code, _ := do("PUT", bad); code != 400 {
			t.Errorf("%s = %d", bad, code)
		}
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
