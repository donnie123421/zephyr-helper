package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/tools"
)

// The app books through the same cards a chat does: it asks for the card,
// shows it, and books it only as shown.
func TestTheAppBooksAndEditsThroughTheSameCards(t *testing.T) {
	r := newRig(t)
	reg := tools.NewWith(r.action.tool("scrub_pool"))
	b := &Booking{Store: r.store, Registry: reg, NAS: r.nas, Now: func() time.Time { return r.now }}
	reg.Add(b.Tools()...)
	mux := http.NewServeMux()
	(&Handler{Store: r.store, Scheduler: r.sched, Booking: b}).RegisterMux(mux, func(h http.Handler) http.Handler { return h })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	do := func(method, path string, body any) (int, map[string]any) {
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(data))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		return resp.StatusCode, got
	}
	scrub := func(when map[string]any) map[string]any {
		args := map[string]any{"action": "scrub_pool", "arguments_json": `{"pool":"tank"}`}
		for k, v := range when {
			args[k] = v
		}
		return map[string]any{"tool": "schedule_action", "arguments": args}
	}
	weekly := map[string]any{"repeat": "weekly", "weekday": "sunday", "time": "04:00"}

	code, card := do("POST", "/schedules/preview", scrub(weekly))
	if code != 200 || card["title"] != "Start a scrub on tank every Sunday at 4:00 am" || card["zone"] != "Australia/Sydney" ||
		card["first_run"] == nil || !strings.Contains(card["detail"].(string), "First runs at 4:00 am on Sun 4 Oct") {
		t.Fatalf("preview = %d %v", code, card)
	}
	if jobs, _ := r.store.List(context.Background()); len(jobs) != 0 {
		t.Fatal("a preview booked it")
	}

	// Booked only as it was shown.
	req := scrub(weekly)
	req["title"], req["detail"] = "Start a scrub on tank every day", card["detail"]
	if code, got := do("POST", "/schedules", req); code != 409 || got["card"] == nil {
		t.Errorf("a card not shown = %d %v", code, got)
	}
	req["title"] = card["title"]
	code, job := do("POST", "/schedules", req)
	if code != 201 || job["title"] != card["title"] || job["tool"] != "scrub_pool" ||
		job["arguments"].(map[string]any)["pool"] != "tank" || job["when"].(map[string]any)["weekday"] != "sunday" {
		t.Fatalf("book = %d %v", code, job)
	}
	id := job["id"].(string)

	// Edited on its new card: the same booking, at its new time.
	daily := scrub(map[string]any{"repeat": "daily", "time": "05:00"})
	_, edited := do("POST", "/schedules/preview", daily)
	daily["title"], daily["detail"] = edited["title"], edited["detail"]
	code, got := do("PUT", "/schedules/"+id, daily)
	sydney, _ := time.LoadLocation("Australia/Sydney")
	if code != 200 || got["id"] != id || got["title"] != "Start a scrub on tank every day at 5:00 am" {
		t.Fatalf("edit = %d %v", code, got)
	}
	if after, _ := r.store.Get(context.Background(), id); after.NextRun == nil ||
		!after.NextRun.Equal(time.Date(2026, 10, 2, 5, 0, 0, 0, sydney)) || after.When.Repeat != "daily" {
		t.Errorf("after the edit: %+v", after)
	}
	if code, _ := do("PUT", "/schedules/nope", daily); code != 404 {
		t.Errorf("edit a booking that's gone = %d", code)
	}
	// Done with, it isn't brought back by an edit.
	done := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Start a scrub on tank at 2:00 am on Thu 1 Oct",
		When: When{Repeat: "once", Date: "2026-10-01", Time: "02:00"}})
	if code, got := do("PUT", "/schedules/"+done.ID, daily); code != 409 || !strings.Contains(fmt.Sprint(got["error"]), "already done") {
		t.Errorf("edit a one-off that's done = %d %v", code, got)
	}

	// A rule and a check-in, as the presets book them.
	for tool, want := range map[string]string{
		"create_update_rule": "Update apps automatically every day at 3:00 am",
		"create_checkin":     "Check in on the NAS every day at 8:00 am",
	} {
		args := map[string]any{"repeat": "daily", "time": "08:00"}
		if tool == "create_update_rule" {
			args = map[string]any{"apps": []string{}, "repeat": "daily", "time": "03:00"}
		}
		if code, got := do("POST", "/schedules/preview", map[string]any{"tool": tool, "arguments": args}); code != 200 || got["title"] != want {
			t.Errorf("%s preview = %d %v", tool, code, got)
		}
	}

	// Why it can't, in words: for people, not the model.
	r.nas.returns("app.query", []any{map[string]any{"name": "plex", "metadata": map[string]any{"title": "Plex"}}})
	if code, got := do("POST", "/schedules/preview", map[string]any{"tool": "create_update_rule",
		"arguments": map[string]any{"apps": []string{"ghost"}, "repeat": "daily", "time": "03:00"}}); code != 400 ||
		got["error"] != `There's no app named "ghost".` {
		t.Errorf("an app that isn't there = %d %v", code, got)
	}
	for body, want := range map[string]string{
		`{"tool": "list_schedules", "arguments": {}}`: "isn't a way to book",
		`{"tool": "schedule_action", "arguments": {"action": "scrub_pool", "arguments_json": "{}", "repeat": "once", "date": "2026-09-30", "time": "02:00"}}`: "That time has already passed",
		`{"tool": "schedule_action"}`: "Send tool and arguments",
	} {
		resp, err := http.Post(srv.URL+"/schedules/preview", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if resp.StatusCode != 400 || !strings.Contains(got["error"], want) {
			t.Errorf("%s = %d %v, want %q", body, resp.StatusCode, got, want)
		}
	}
}

func TestBookingNeedsTrueNAS(t *testing.T) {
	r := newRig(t)
	mux := http.NewServeMux()
	(&Handler{Store: r.store, Scheduler: r.sched}).RegisterMux(mux, func(h http.Handler) http.Handler { return h })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/schedules/preview", strings.NewReader(`{"tool": "create_checkin", "arguments": {}}`)))
	if rec.Code != 503 {
		t.Errorf("without TrueNAS = %d", rec.Code)
	}
}
