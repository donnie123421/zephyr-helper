package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/actions"
	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/tools"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// fakeNAS answers the scheduler's calls from a script.
type fakeNAS struct {
	mu      sync.Mutex
	replies map[string]func(params []any) (any, error)
	calls   []string
	jobs    map[int64]truenas.Job
	nextJob int64
}

func newFakeNAS() *fakeNAS {
	return &fakeNAS{replies: map[string]func([]any) (any, error){}, jobs: map[int64]truenas.Job{}, nextJob: 100}
}

func (f *fakeNAS) on(method string, reply func(params []any) (any, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method] = reply
}

func (f *fakeNAS) returns(method string, v any) {
	f.on(method, func([]any) (any, error) { return v, nil })
}

// job makes method start a job that ends in state.
func (f *fakeNAS) job(method, state, failure string, then func()) {
	f.on(method, func([]any) (any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.nextJob++
		f.jobs[f.nextJob] = truenas.Job{ID: f.nextJob, State: state, Error: failure}
		if then != nil {
			then()
		}
		return f.nextJob, nil
	})
}

func (f *fakeNAS) Call(_ context.Context, method string, params ...any) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	reply, ok := f.replies[method]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unexpected call %s", method)
	}
	v, err := reply(params)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func (f *fakeNAS) WaitJob(_ context.Context, id int64, _ time.Duration) (truenas.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobs[id], nil
}

func (f *fakeNAS) called(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

// fakeAction is an action tool that records its runs.
type fakeAction struct {
	mu          sync.Mutex
	runs        []string
	describeErr error
}

func (f *fakeAction) tool(name string) tools.Tool {
	return tools.Tool{
		Def:        tools.Definition{Name: name, Parameters: json.RawMessage(`{}`)},
		StatusLine: "Working…",
		Action: &tools.Action{
			Describe: func(_ context.Context, args json.RawMessage) (tools.Card, error) {
				if f.describeErr != nil {
					return tools.Card{}, f.describeErr
				}
				return tools.Card{Title: "Start a scrub on tank", Detail: "tank will be slower.", Risk: tools.RiskRoutine}, nil
			},
			Run: func(_ context.Context, args json.RawMessage) (tools.Outcome, error) {
				f.mu.Lock()
				f.runs = append(f.runs, string(args))
				f.mu.Unlock()
				return tools.Outcome{Message: "Started a scrub on tank."}, nil
			},
		},
	}
}

func (f *fakeAction) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

type fakeCheckIns struct {
	err error
}

func (c fakeCheckIns) CheckIn(_ context.Context, title string) (CheckInResult, error) {
	if c.err != nil {
		return CheckInResult{}, c.err
	}
	return CheckInResult{ConversationID: "conv1", Summary: "All three pools are healthy."}, nil
}

type rig struct {
	store  *Store
	events *events.Store
	nas    *fakeNAS
	action *fakeAction
	sched  *Scheduler
	now    time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	evs, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	t.Cleanup(func() { _ = evs.Close() })
	r := &rig{store: store, events: evs, nas: newFakeNAS(), action: &fakeAction{}}
	r.nas.returns("system.general.config", map[string]any{"timezone": "Australia/Sydney"})
	reg := tools.NewWith(r.action.tool("scrub_pool"))
	log := actions.New(context.Background(), evs, r.nas)
	sydney, _ := time.LoadLocation("Australia/Sydney")
	r.now = time.Date(2026, 10, 1, 14, 30, 0, 0, sydney)
	r.sched = New(store, reg, log, evs, r.nas, fakeCheckIns{})
	r.sched.now = func() time.Time { return r.now }
	r.sched.ruleTiming = ruleTiming{upgradeFor: time.Second, runningFor: 50 * time.Millisecond, poll: 5 * time.Millisecond}
	return r
}

func (r *rig) book(t *testing.T, job Job) Job {
	t.Helper()
	if job.When.Zone == "" {
		job.When.Zone = "Australia/Sydney"
	}
	job, err := r.store.Add(context.Background(), job)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	return job
}

func (r *rig) feed(t *testing.T) []events.Event {
	t.Helper()
	r.sched.Wait()
	evs, err := r.events.List(context.Background(), events.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return evs
}

func at(t time.Time) *time.Time { return &t }

func TestADueActionRunsAndBooksItsNextRun(t *testing.T) {
	r := newRig(t)
	job := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Arguments: json.RawMessage(`{"pool":"tank"}`),
		Title: "Start a scrub on tank every Sunday at 2:00 am", Detail: "…", Risk: "routine",
		When: When{Repeat: "weekly", Weekday: "sunday", Time: "02:00"}, NextRun: at(r.now.Add(-time.Minute))})
	r.sched.runDue(context.Background())
	evs := r.feed(t)

	if r.action.count() != 1 {
		t.Fatalf("ran %d times", r.action.count())
	}
	if len(evs) != 1 || evs[0].DedupeKey[len(evs[0].DedupeKey)-len("|scheduled"):] != "|scheduled" ||
		!strings.Contains(evs[0].Summary, "ran on schedule") {
		t.Errorf("feed = %+v", evs)
	}
	got, _ := r.store.Get(context.Background(), job.ID)
	sydney, _ := time.LoadLocation("Australia/Sydney")
	if got.LastStatus != "done" || got.NextRun == nil || !got.NextRun.Equal(time.Date(2026, 10, 4, 2, 0, 0, 0, sydney)) {
		t.Errorf("after the run: %+v", got)
	}
}

func TestAOneOffRunsOnceAndAPausedJobWaits(t *testing.T) {
	r := newRig(t)
	once := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Start a scrub on tank at 2:00 pm on Thu 1 Oct",
		When: When{Repeat: "once", Date: "2026-10-01", Time: "14:00"}, NextRun: at(r.now.Add(-30 * time.Minute))})
	r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "paused", Paused: true,
		When: When{Repeat: "daily", Time: "14:00"}, NextRun: at(r.now.Add(-30 * time.Minute))})
	r.sched.runDue(context.Background())
	r.sched.Wait()
	r.sched.runDue(context.Background())
	r.sched.Wait()
	if r.action.count() != 1 {
		t.Fatalf("ran %d times, want the one-off once", r.action.count())
	}
	got, _ := r.store.Get(context.Background(), once.ID)
	if got.NextRun != nil || got.LastStatus != "done" {
		t.Errorf("one-off after its run: %+v", got)
	}
}

func TestAJobDueLongAgoIsMissedNotRun(t *testing.T) {
	r := newRig(t)
	once := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Update Plex at 3:00 am on Thu 1 Oct",
		When: When{Repeat: "once", Date: "2026-10-01", Time: "03:00"}, NextRun: at(r.now.Add(-11 * time.Hour))})
	r.sched.runDue(context.Background())
	evs := r.feed(t)
	if r.action.count() != 0 {
		t.Fatal("a job half a day late ran")
	}
	got, _ := r.store.Get(context.Background(), once.ID)
	if got.LastStatus != "missed" || got.NextRun != nil {
		t.Errorf("job = %+v", got)
	}
	// Nobody was told otherwise: it notifies.
	if len(evs) != 1 || !events.IsAssistantPush(evs[0]) || !strings.HasPrefix(evs[0].Title, "Missed:") {
		t.Errorf("feed = %+v", evs)
	}
}

func TestAnActionWithNothingToDoIsSkipped(t *testing.T) {
	r := newRig(t)
	r.action.describeErr = errors.New("tank is already running a scrub")
	r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Start a scrub on tank every day at 2:00 pm",
		When: When{Repeat: "daily", Time: "14:00"}, NextRun: at(r.now.Add(-time.Minute))})
	r.sched.runDue(context.Background())
	evs := r.feed(t)
	if r.action.count() != 0 {
		t.Fatal("ran anyway")
	}
	if len(evs) != 1 || evs[0].Summary != "Tank is already running a scrub." || events.IsAssistantPush(evs[0]) {
		t.Errorf("feed = %+v", evs)
	}
}

func TestRunNowLeavesTheScheduleAlone(t *testing.T) {
	r := newRig(t)
	next := r.now.Add(48 * time.Hour)
	job := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "scrub",
		When: When{Repeat: "weekly", Weekday: "saturday", Time: "14:30"}, NextRun: &next})
	if err := r.sched.RunNow(context.Background(), job.ID); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	r.sched.Wait()
	got, _ := r.store.Get(context.Background(), job.ID)
	if r.action.count() != 1 || got.NextRun == nil || !got.NextRun.Equal(next) {
		t.Errorf("after Run Now: ran %d, next %v", r.action.count(), got.NextRun)
	}
}

// apps scripts app.query: plex RUNNING with an update, sonarr likewise,
// and a custom app with an update the rule leaves alone.
func (r *rig) apps(sonarrState func() string) {
	r.nas.on("app.query", func(params []any) (any, error) {
		all := []map[string]any{
			{"name": "plex", "state": "RUNNING", "version": "1.2.0", "upgrade_available": true, "metadata": map[string]any{"title": "Plex"}},
			{"name": "sonarr", "state": sonarrState(), "version": "2.0.0", "upgrade_available": true, "metadata": map[string]any{"title": "Sonarr"}},
			{"name": "mine", "state": "RUNNING", "version": "1.0.0", "upgrade_available": true, "custom_app": true},
		}
		if len(params) > 0 {
			filters, _ := json.Marshal(params[0])
			for _, a := range all {
				if strings.Contains(string(filters), `"`+a["name"].(string)+`"`) {
					return []any{a}, nil
				}
			}
		}
		return all, nil
	})
	r.nas.on("app.upgrade_summary", func(params []any) (any, error) {
		return map[string]any{"upgrade_version": "9.9.9"}, nil
	})
}

func TestTheUpdateRuleUpdatesAndRollsBackWhatDoesntComeBack(t *testing.T) {
	r := newRig(t)
	r.nas.returns("pool.query", []any{map[string]any{"name": "tank", "status": "ONLINE", "healthy": true}})
	var mu sync.Mutex
	sonarr := "RUNNING"
	r.apps(func() string { mu.Lock(); defer mu.Unlock(); return sonarr })
	var upgrades []string
	r.nas.on("app.upgrade", func(params []any) (any, error) {
		name := params[0].(string)
		mu.Lock()
		upgrades = append(upgrades, name)
		if name == "sonarr" {
			sonarr = "CRASHED"
		}
		mu.Unlock()
		r.nas.mu.Lock()
		defer r.nas.mu.Unlock()
		r.nas.nextJob++
		r.nas.jobs[r.nas.nextJob] = truenas.Job{ID: r.nas.nextJob, State: "SUCCESS"}
		return r.nas.nextJob, nil
	})
	r.nas.job("app.rollback", "SUCCESS", "", func() { mu.Lock(); sonarr = "RUNNING"; mu.Unlock() })
	settings, _ := json.Marshal(UpdateRule{})
	job := r.book(t, Job{Kind: KindUpdateRule, Arguments: settings, Title: "Update apps automatically every Sunday at 3:00 am",
		When: When{Repeat: "weekly", Weekday: "sunday", Time: "03:00"}, NextRun: at(r.now.Add(-time.Minute))})
	r.sched.runDue(context.Background())
	evs := r.feed(t)

	if fmt.Sprint(upgrades) != "[plex sonarr]" {
		t.Errorf("updated %v; the custom app is left alone", upgrades)
	}
	if r.nas.called("app.rollback") != 1 {
		t.Errorf("rolled back %d times, want sonarr once", r.nas.called("app.rollback"))
	}
	if len(evs) != 1 || evs[0].Title != "Updated Plex; 1 didn't go to plan" || !events.IsAssistantPush(evs[0]) ||
		!strings.Contains(evs[0].Summary, "Sonarr wasn't running after its update, so it was rolled back to 2.0.0.") {
		t.Errorf("feed = %+v", evs)
	}
	got, _ := r.store.Get(context.Background(), job.ID)
	if got.LastStatus != "failed" {
		t.Errorf("job = %+v", got)
	}
}

func TestTheUpdateRuleWaitsForHealthyPools(t *testing.T) {
	r := newRig(t)
	r.nas.returns("pool.query", []any{map[string]any{"name": "backup", "status": "DEGRADED", "healthy": false}})
	r.apps(func() string { return "RUNNING" })
	settings, _ := json.Marshal(UpdateRule{Apps: []string{"plex"}})
	r.book(t, Job{Kind: KindUpdateRule, Arguments: settings, Title: "Update Plex automatically every day at 3:00 am",
		When: When{Repeat: "daily", Time: "03:00"}, NextRun: at(r.now.Add(-time.Minute))})
	r.sched.runDue(context.Background())
	evs := r.feed(t)
	if r.nas.called("app.upgrade") != 0 {
		t.Fatal("updated while a pool was degraded")
	}
	if len(evs) != 1 || evs[0].Summary != "Pool backup is degraded, and nothing is updated while a pool isn't healthy." {
		t.Errorf("feed = %+v", evs)
	}
}

func TestACheckInLeavesABriefingAndSaysSo(t *testing.T) {
	r := newRig(t)
	r.book(t, Job{Kind: KindCheckIn, Title: "Check in on the NAS every day at 2:00 pm",
		When: When{Repeat: "daily", Time: "14:00"}, NextRun: at(r.now.Add(-time.Minute))})
	r.sched.runDue(context.Background())
	evs := r.feed(t)
	if len(evs) != 1 || evs[0].Title != "Your check-in is ready" || evs[0].Summary != "All three pools are healthy." ||
		!events.IsAssistantPush(evs[0]) || !strings.Contains(string(evs[0].Body), `"conversation_id":"conv1"`) {
		t.Errorf("feed = %+v", evs)
	}
}

func TestBookingAnActionWritesItsCardFromTheAction(t *testing.T) {
	r := newRig(t)
	reg := tools.NewWith(r.action.tool("scrub_pool"))
	b := Booking{Store: r.store, Registry: reg, NAS: r.nas, Now: func() time.Time { return r.now }}
	reg.Add(b.Tools()...)
	action := reg.Action("schedule_action")

	card, err := action.Describe(context.Background(), json.RawMessage(
		`{"action":"scrub_pool","arguments_json":"{\"pool\":\"tank\"}","repeat":"weekly","weekday":"sunday","time":"04:00"}`))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if card.Title != "Start a scrub on tank every Sunday at 4:00 am" ||
		!strings.Contains(card.Detail, "First runs at 4:00 am on Sun 4 Oct, NAS time.") {
		t.Errorf("card = %+v", card)
	}
	outcome, err := action.Run(context.Background(), card.Args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	jobs, _ := r.store.List(context.Background())
	if len(jobs) != 1 || jobs[0].Title != card.Title || string(jobs[0].Arguments) != `{"pool":"tank"}` ||
		jobs[0].When.Zone != "Australia/Sydney" || outcome.Message != "Booked: "+card.Title+"." {
		t.Errorf("booked %+v, outcome %+v", jobs, outcome)
	}

	for args, want := range map[string]string{
		`{"action":"update_app","arguments_json":"{}","repeat":"weekly","weekday":"sunday","time":"02:00"}`:        "create_update_rule",
		`{"action":"acknowledge_alerts","arguments_json":"{}","repeat":"once","date":"2026-10-02","time":"02:00"}`: "can't be booked",
		`{"action":"scrub_pool","arguments_json":"{}","repeat":"once","date":"2026-09-30","time":"02:00"}`:         "already passed",
		`{"action":"scrub_pool","arguments_json":"{}","repeat":"once","date":"2028-01-01","time":"02:00"}`:         "more than a year",
	} {
		if _, err := action.Describe(context.Background(), json.RawMessage(args)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Describe(%s) = %v, want %q", args, err, want)
		}
	}
}

func TestBookingARuleAndACheckIn(t *testing.T) {
	r := newRig(t)
	r.apps(func() string { return "RUNNING" })
	reg := tools.NewWith()
	b := Booking{Store: r.store, Registry: reg, NAS: r.nas, Now: func() time.Time { return r.now },
		Model: func() (string, bool) { return "Claude (claude-opus-5)", true }}
	reg.Add(b.Tools()...)

	rule, err := reg.Action("create_update_rule").Describe(context.Background(),
		json.RawMessage(`{"apps":["plex","sonarr"],"repeat":"weekly","weekday":"sunday","time":"03:00"}`))
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	if rule.Title != "Update Plex and Sonarr automatically every Sunday at 3:00 am" || rule.Risk != tools.RiskDisruptive ||
		!strings.Contains(rule.Detail, "rolled back") {
		t.Errorf("rule card = %+v", rule)
	}
	if _, err := reg.Action("create_update_rule").Describe(context.Background(),
		json.RawMessage(`{"apps":["mine"],"repeat":"daily","time":"03:00"}`)); err == nil {
		t.Error("a custom app was let into the rule")
	}

	checkIn, err := reg.Action("create_checkin").Describe(context.Background(), json.RawMessage(`{"repeat":"daily","time":"08:00"}`))
	if err != nil {
		t.Fatalf("check-in: %v", err)
	}
	if checkIn.Title != "Check in on the NAS every day at 8:00 am" ||
		!strings.Contains(checkIn.Detail, "Claude (claude-opus-5) with your own API key") {
		t.Errorf("check-in card = %+v", checkIn)
	}
	if _, err := reg.Action("create_checkin").Run(context.Background(), checkIn.Args); err != nil {
		t.Fatalf("book check-in: %v", err)
	}
	jobs, _ := r.store.List(context.Background())
	if len(jobs) != 1 || jobs[0].Kind != KindCheckIn {
		t.Errorf("jobs = %+v", jobs)
	}
}

func TestTheScheduledScreensAPI(t *testing.T) {
	r := newRig(t)
	job := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Start a scrub on tank every Sunday at 2:00 am",
		When: When{Repeat: "weekly", Weekday: "sunday", Time: "02:00"}, NextRun: at(r.now.Add(time.Hour))})
	mux := http.NewServeMux()
	(&Handler{Store: r.store, Scheduler: r.sched}).RegisterMux(mux, func(h http.Handler) http.Handler { return h })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	do := func(method, path string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	if code, body := do("GET", "/schedules"); code != 200 || !strings.Contains(fmt.Sprint(body), "every Sunday at 2:00 am") {
		t.Errorf("list = %d %v", code, body)
	}
	if code, body := do("POST", "/schedules/"+job.ID+"/pause"); code != 200 || body["paused"] != true {
		t.Errorf("pause = %d %v", code, body)
	}
	if code, body := do("POST", "/schedules/"+job.ID+"/resume"); code != 200 || body["paused"] != false {
		t.Errorf("resume = %d %v", code, body)
	}
	if code, _ := do("DELETE", "/schedules/"+job.ID); code != 204 {
		t.Errorf("delete = %d", code)
	}
	if code, _ := do("POST", "/schedules/"+job.ID+"/run"); code != 404 {
		t.Errorf("run after delete = %d", code)
	}
}
