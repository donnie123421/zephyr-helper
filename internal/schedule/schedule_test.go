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

func (f *fakeNAS) WaitJob(ctx context.Context, id int64, _ time.Duration) (truenas.Job, error) {
	f.mu.Lock()
	job := f.jobs[id]
	f.mu.Unlock()
	if job.State == "RUNNING" {
		// Never ends: the wait does.
		<-ctx.Done()
		return job, ctx.Err()
	}
	return job, nil
}

// startJob starts a job that ends in state.
func (f *fakeNAS) startJob(state, failure string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextJob++
	f.jobs[f.nextJob] = truenas.Job{ID: f.nextJob, State: state, Error: failure}
	return f.nextJob
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
	// block, when set, holds each run until it's closed.
	block chan struct{}
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
				if f.block != nil {
					<-f.block
				}
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

func (c fakeCheckIns) CheckIn(ctx context.Context, title string) (CheckInResult, error) {
	if c.err != nil {
		return CheckInResult{}, c.err
	}
	// A model takes a while; a run whose context ends meanwhile fails.
	select {
	case <-ctx.Done():
		return CheckInResult{}, ctx.Err()
	case <-time.After(20 * time.Millisecond):
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
	r.sched.ruleTiming = ruleTiming{upgradeFor: time.Second, runningFor: 50 * time.Millisecond, settle: 10 * time.Millisecond, poll: 5 * time.Millisecond}
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

// waitFor waits for ch to close, or fails the test.
func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

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

// Run Now is asked for over HTTP, and the request is answered before the
// run ends: the run mustn't end with it.
func TestRunNowOutlivesItsRequest(t *testing.T) {
	r := newRig(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go r.sched.Run(ctx)
	job := r.book(t, Job{Kind: KindCheckIn, Title: "Check in on the NAS every day at 8:00 am",
		When: When{Repeat: "daily", Time: "08:00"}, NextRun: at(r.now.Add(17 * time.Hour))})
	for r.sched.baseContext() == nil {
		time.Sleep(time.Millisecond)
	}
	request, answered := context.WithCancel(context.Background())
	if err := r.sched.RunNow(request, job.ID); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	answered()
	time.Sleep(5 * time.Millisecond)
	r.sched.Wait()
	got, _ := r.store.Get(context.Background(), job.ID)
	if got.LastStatus != "done" {
		t.Errorf("after Run Now: %s, %s", got.LastStatus, got.LastMessage)
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

// fakeApp is an app on the fake NAS, with an update to 9.9.9.
type fakeApp struct {
	name, title, state, version string
	custom                      bool
	// afterUpdate is its state once updated; failUpdate leaves it on its
	// version, in afterFailure; hangUpdate never ends.
	afterUpdate, afterFailure string
	failUpdate, hangUpdate    bool
}

// apps puts apps on the fake NAS. updated lists the apps updated, and
// onUpdate is called as each update starts.
func (r *rig) apps(apps ...*fakeApp) (updated func() []string, onUpdate *func(name string)) {
	var mu sync.Mutex
	var names []string
	hook := func(string) {}
	find := func(name string) *fakeApp {
		for _, a := range apps {
			if a.name == name {
				return a
			}
		}
		return nil
	}
	r.nas.on("app.query", func(params []any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		out := []any{}
		for _, a := range apps {
			if len(params) > 0 {
				filters, _ := json.Marshal(params[0])
				if !strings.Contains(string(filters), `"`+a.name+`"`) {
					continue
				}
			}
			out = append(out, map[string]any{"name": a.name, "state": a.state, "version": a.version,
				"upgrade_available": a.version != "9.9.9", "custom_app": a.custom, "metadata": map[string]any{"title": a.title}})
		}
		return out, nil
	})
	r.nas.returns("app.upgrade_summary", map[string]any{"upgrade_version": "9.9.9"})
	r.nas.on("app.upgrade", func(params []any) (any, error) {
		name := params[0].(string)
		hook(name)
		mu.Lock()
		defer mu.Unlock()
		names = append(names, name)
		a := find(name)
		switch {
		case a.hangUpdate:
			return r.nas.startJob("RUNNING", ""), nil
		case a.failUpdate:
			if a.afterFailure != "" {
				a.state = a.afterFailure
			}
			return r.nas.startJob("FAILED", "[EFAULT] Failed to pull the image"), nil
		}
		a.version = "9.9.9"
		if a.afterUpdate != "" {
			a.state = a.afterUpdate
		}
		return r.nas.startJob("SUCCESS", ""), nil
	})
	r.nas.on("app.rollback", func(params []any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		a := find(params[0].(string))
		a.version = params[1].(map[string]any)["app_version"].(string)
		a.state = "RUNNING"
		return r.nas.startJob("SUCCESS", ""), nil
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), names...)
	}, &hook
}

// standardApps are Plex and Sonarr, running with updates, and a custom app.
func standardApps() []*fakeApp {
	return []*fakeApp{
		{name: "mine", title: "mine", state: "RUNNING", version: "1.0.0", custom: true},
		{name: "plex", title: "Plex", state: "RUNNING", version: "1.2.0"},
		{name: "sonarr", title: "Sonarr", state: "RUNNING", version: "2.0.0"},
	}
}

func TestTheUpdateRuleUpdatesAndRollsBackWhatDoesntComeBack(t *testing.T) {
	r := newRig(t)
	r.nas.returns("pool.query", []any{map[string]any{"name": "tank", "status": "ONLINE", "healthy": true}})
	apps := standardApps()
	apps[2].afterUpdate = "CRASHED"
	updated, _ := r.apps(apps...)
	settings, _ := json.Marshal(UpdateRule{})
	job := r.book(t, Job{Kind: KindUpdateRule, Arguments: settings, Title: "Update apps automatically every Sunday at 3:00 am",
		When: When{Repeat: "weekly", Weekday: "sunday", Time: "03:00"}, NextRun: at(r.now.Add(-time.Minute))})
	r.sched.runDue(context.Background())
	evs := r.feed(t)

	if fmt.Sprint(updated()) != "[plex sonarr]" {
		t.Errorf("updated %v; the custom app is left alone", updated())
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

// A failed update leaves the app on its version: there's nothing to roll
// back, even though it isn't running.
func TestTheUpdateRuleRollsBackOnlyANewVersion(t *testing.T) {
	r := newRig(t)
	r.nas.returns("pool.query", []any{map[string]any{"name": "tank", "status": "ONLINE", "healthy": true}})
	apps := standardApps()[1:2]
	apps[0].failUpdate, apps[0].afterFailure = true, "CRASHED"
	r.apps(apps...)
	settings, _ := json.Marshal(UpdateRule{})
	r.book(t, Job{Kind: KindUpdateRule, Arguments: settings, Title: "Update apps automatically every day at 3:00 am",
		When: When{Repeat: "daily", Time: "03:00"}, NextRun: at(r.now.Add(-time.Minute))})
	r.sched.runDue(context.Background())
	evs := r.feed(t)
	if r.nas.called("app.rollback") != 0 {
		t.Error("rolled back an app still on its own version")
	}
	if len(evs) != 1 || !strings.Contains(evs[0].Summary, "Plex didn't update, and isn't running:") {
		t.Errorf("feed = %+v", evs)
	}
}

// The helper stopping mid-rule says nothing it doesn't know: the run is
// left unfinished, to show as interrupted when the helper starts again.
func TestAShutdownMidRuleWritesNothingFalse(t *testing.T) {
	r := newRig(t)
	r.nas.returns("pool.query", []any{map[string]any{"name": "tank", "status": "ONLINE", "healthy": true}})
	apps := standardApps()
	apps[1].hangUpdate = true
	_, onUpdate := r.apps(apps...)
	updating := make(chan struct{})
	*onUpdate = func(name string) {
		if name == "plex" {
			close(updating)
		}
	}
	settings, _ := json.Marshal(UpdateRule{})
	due := r.now.Add(-time.Minute)
	job := r.book(t, Job{Kind: KindUpdateRule, Arguments: settings, Title: "Update apps automatically every day at 2:29 pm",
		When: When{Repeat: "daily", Time: "14:29"}, NextRun: &due})
	ctx, stop := context.WithCancel(context.Background())
	r.sched.runDue(ctx)
	waitFor(t, updating)
	stop()
	evs := r.feed(t)
	if len(evs) != 0 || r.nas.called("app.rollback") != 0 {
		t.Errorf("feed = %+v, rollbacks %d", evs, r.nas.called("app.rollback"))
	}
	got, _ := r.store.Get(context.Background(), job.ID)
	if got.LastStatus != "running" {
		t.Errorf("job = %+v, want it left unfinished", got)
	}
}

// Pausing a rule partway stops it before the next app.
func TestPausingARuleStopsItBeforeTheNextApp(t *testing.T) {
	r := newRig(t)
	r.nas.returns("pool.query", []any{map[string]any{"name": "tank", "status": "ONLINE", "healthy": true}})
	updated, onUpdate := r.apps(standardApps()...)
	settings, _ := json.Marshal(UpdateRule{})
	job := r.book(t, Job{Kind: KindUpdateRule, Arguments: settings, Title: "Update apps automatically every day at 2:29 pm",
		When: When{Repeat: "daily", Time: "14:29"}, NextRun: at(r.now.Add(-time.Minute))})
	*onUpdate = func(string) { _ = r.store.Pause(context.Background(), job.ID) }
	r.sched.runDue(context.Background())
	evs := r.feed(t)
	if fmt.Sprint(updated()) != "[plex]" {
		t.Errorf("updated %v after the pause", updated())
	}
	if len(evs) != 1 || !strings.Contains(evs[0].Summary, "paused or removed while it ran, so Sonarr was left alone") {
		t.Errorf("feed = %+v", evs)
	}
}

func TestTheUpdateRuleWaitsForHealthyPools(t *testing.T) {
	r := newRig(t)
	r.nas.returns("pool.query", []any{map[string]any{"name": "backup", "status": "DEGRADED", "healthy": false}})
	r.apps(standardApps()...)
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

// 2:00 am doesn't happen in Sydney on Sun 4 Oct 2026: the clocks go
// forward through it. The run is at 3:00 am, and the card says why.
func TestACardSaysWhenTheClocksMoveARun(t *testing.T) {
	r := newRig(t)
	reg := tools.NewWith(r.action.tool("scrub_pool"))
	b := Booking{Store: r.store, Registry: reg, NAS: r.nas, Now: func() time.Time { return r.now }}
	reg.Add(b.Tools()...)
	card, err := reg.Action("schedule_action").Describe(context.Background(), json.RawMessage(
		`{"action":"scrub_pool","arguments_json":"{\"pool\":\"tank\"}","repeat":"weekly","weekday":"sunday","time":"02:00"}`))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if card.Title != "Start a scrub on tank every Sunday at 2:00 am" ||
		!strings.Contains(card.Detail, "First runs at 3:00 am on Sun 4 Oct (the clocks go forward that night), NAS time.") {
		t.Errorf("card = %+v", card)
	}
}

func TestBookingARuleAndACheckIn(t *testing.T) {
	r := newRig(t)
	r.apps(standardApps()...)
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

// A restart partway through a run doesn't run it again: its next run was
// booked before it started, and it shows as interrupted.
func TestARestartDoesntRunARunAgain(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	due := r.now.Add(-time.Minute)
	job := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "Start a scrub on tank every day at 2:29 pm",
		When: When{Repeat: "daily", Time: "14:29"}, NextRun: &due})
	next := due.Add(24 * time.Hour)
	if claimed, err := r.store.Claim(ctx, job.ID, due, r.now, &next); !claimed || err != nil {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	// The helper stopped there; now it's starting again.
	r.sched.interrupted(ctx)
	r.sched.runDue(ctx)
	evs := r.feed(t)
	if r.action.count() != 0 {
		t.Error("ran again after the restart")
	}
	got, _ := r.store.Get(ctx, job.ID)
	if got.LastStatus != "interrupted" || got.NextRun == nil || !got.NextRun.Equal(next) {
		t.Errorf("job = %+v", got)
	}
	if len(evs) != 1 || !strings.HasPrefix(evs[0].Title, "Interrupted:") || !events.IsAssistantPush(evs[0]) {
		t.Errorf("feed = %+v", evs)
	}
}

// A job whose Run Now run is still going isn't missed, or run again.
func TestAJobStillRunningIsntMissed(t *testing.T) {
	r := newRig(t)
	long := r.now.Add(-3 * time.Hour)
	job := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "scrub",
		When: When{Repeat: "daily", Time: "11:30"}, NextRun: &long})
	r.sched.reserve(job.ID)
	defer r.sched.release(job.ID)
	r.sched.runDue(context.Background())
	evs := r.feed(t)
	got, _ := r.store.Get(context.Background(), job.ID)
	if len(evs) != 0 || got.LastStatus != "" || !got.NextRun.Equal(long) {
		t.Errorf("feed = %+v, job = %+v", evs, got)
	}
}

// Run Now's run ending doesn't put back the schedule it started with: a
// resume meanwhile stands, and the job doesn't run twice.
func TestRunNowKeepsAScheduleChangedDuringIt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.action.block = make(chan struct{})
	sydney, _ := time.LoadLocation("Australia/Sydney")
	today := time.Date(2026, 10, 1, 14, 0, 0, 0, sydney)
	job := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "scrub", Paused: true,
		When: When{Repeat: "daily", Time: "14:00"}, NextRun: &today})
	if err := r.sched.RunNow(ctx, job.ID); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	tomorrow := today.Add(24 * time.Hour)
	if err := r.store.Resume(ctx, job.ID, tomorrow); err != nil {
		t.Fatalf("resume: %v", err)
	}
	close(r.action.block)
	r.sched.Wait()
	got, _ := r.store.Get(ctx, job.ID)
	if got.Paused || !got.NextRun.Equal(tomorrow) || got.LastStatus != "done" {
		t.Errorf("job = %+v", got)
	}
	r.sched.runDue(ctx)
	r.sched.Wait()
	if r.action.count() != 1 {
		t.Errorf("ran %d times", r.action.count())
	}
}

// One-offs that are done with don't count towards MaxJobs, and go after a
// while.
func TestFinishedOneOffsDontFillTheSchedule(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	old := r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: "old", CreatedAt: time.Now().Add(-40 * 24 * time.Hour),
		When: When{Repeat: "once", Date: "2026-08-01", Time: "02:00"}})
	for i := 0; i < MaxJobs; i++ {
		r.book(t, Job{Kind: KindAction, Tool: "scrub_pool", Title: fmt.Sprint(i), When: When{Repeat: "once", Date: "2026-09-30", Time: "02:00"}})
	}
	if _, err := r.store.Add(ctx, Job{Kind: KindAction, Tool: "scrub_pool", Title: "new",
		When: When{Repeat: "once", Date: "2026-10-02", Time: "02:00", Zone: "Australia/Sydney"}, NextRun: at(r.now.Add(time.Hour))}); err != nil {
		t.Fatalf("booking past %d finished one-offs: %v", MaxJobs, err)
	}
	if _, err := r.store.Get(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a one-off finished long ago is still kept: %v", err)
	}
}

func bookingRig(t *testing.T) (*rig, *tools.Registry) {
	r := newRig(t)
	reg := tools.NewWith(r.action.tool("scrub_pool"))
	b := Booking{Store: r.store, Registry: reg, NAS: r.nas, Now: func() time.Time { return r.now }}
	reg.Add(b.Tools()...)
	return r, reg
}

// A card is made only from what the model may send; the NAS's time zone
// comes from the NAS. Only an approved card's arguments book anything.
func TestTheModelCantChooseWhatACardBooks(t *testing.T) {
	_, reg := bookingRig(t)
	ctx := context.Background()
	action := reg.Action("schedule_action")
	for _, raw := range []string{
		`{"kind":"action","tool":"scrub_pool","arguments":{"pool":"tank"},"when":{"repeat":"daily","time":"03:00","zone":"America/Los_Angeles"},"title":"x","detail":"y","risk":"routine"}`,
		`{"action":"scrub_pool","arguments":{"pool":"tank"},"when":{"repeat":"daily","time":"03:00","zone":"America/Los_Angeles"}}`,
		`{"action":"scrub_pool","arguments_json":"{}","repeat":"daily","time":"03:00","zone":"America/Los_Angeles"}`,
	} {
		if _, err := action.Describe(ctx, json.RawMessage(raw)); err == nil {
			t.Errorf("Describe took %s", raw)
		}
	}
	model := json.RawMessage(`{"action":"scrub_pool","arguments_json":"{\"pool\":\"tank\"}","repeat":"daily","time":"03:00"}`)
	if _, err := action.Run(ctx, model); err == nil {
		t.Error("Run booked the model's arguments, not a card")
	}
	card, err := action.Describe(ctx, model)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !strings.Contains(string(card.Args), `"zone":"Australia/Sydney"`) {
		t.Errorf("card args = %s", card.Args)
	}
	for _, other := range []string{"create_update_rule", "create_checkin"} {
		if _, err := reg.Action(other).Run(ctx, card.Args); err == nil {
			t.Errorf("%s booked a schedule_action card", other)
		}
	}
}

// Without the NAS's time zone, nothing is booked: the card couldn't say
// when it runs.
func TestBookingNeedsTheNASsTimeZone(t *testing.T) {
	r, reg := bookingRig(t)
	r.nas.on("system.general.config", func([]any) (any, error) { return nil, errors.New("connection refused") })
	_, err := reg.Action("create_checkin").Describe(context.Background(), json.RawMessage(`{"repeat":"daily","time":"08:00"}`))
	if err == nil || !strings.Contains(err.Error(), "time zone") {
		t.Errorf("Describe = %v", err)
	}
}

// Times read as HH:MM, and "9:30" is 09:30, with no clock-change note.
func TestTimesAreReadStrictly(t *testing.T) {
	_, reg := bookingRig(t)
	ctx := context.Background()
	card, err := reg.Action("create_checkin").Describe(ctx, json.RawMessage(`{"repeat":"daily","time":"9:30"}`))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if strings.Contains(card.Detail, "clocks") || !strings.Contains(card.Title, "every day at 9:30 am") {
		t.Errorf("card = %+v", card)
	}
	for _, bad := range []string{"+9:30", "9:5", "09:30:00", "24:00", "-1:30"} {
		if _, err := reg.Action("create_checkin").Describe(ctx, json.RawMessage(`{"repeat":"daily","time":"`+bad+`"}`)); err == nil {
			t.Errorf("took time %q", bad)
		}
	}
}

// A repeating snapshot's card doesn't promise the name made for it now.
func TestARepeatingSnapshotsCardDoesntNameIt(t *testing.T) {
	r := newRig(t)
	snapshot := tools.Tool{
		Def:        tools.Definition{Name: "create_snapshot", Parameters: json.RawMessage(`{}`)},
		StatusLine: "Taking a snapshot…",
		Action: &tools.Action{
			Describe: func(context.Context, json.RawMessage) (tools.Card, error) {
				return tools.Card{Title: "Take a snapshot of tank/media",
					Detail: "Named zephyr-2026-10-01-1430. Of tank/media alone. It takes no space at first.", Risk: tools.RiskRoutine}, nil
			},
			Run: func(context.Context, json.RawMessage) (tools.Outcome, error) { return tools.Outcome{}, nil },
		},
	}
	reg := tools.NewWith(snapshot)
	reg.Add(Booking{Store: r.store, Registry: reg, NAS: r.nas, Now: func() time.Time { return r.now }}.Tools()...)
	card, err := reg.Action("schedule_action").Describe(context.Background(), json.RawMessage(
		`{"action":"create_snapshot","arguments_json":"{\"dataset\":\"tank/media\"}","repeat":"daily","time":"01:00"}`))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !strings.HasPrefix(card.Detail, "Named for when it's taken. Of tank/media alone.") {
		t.Errorf("detail = %q", card.Detail)
	}
}

// Run Now runs a paused rule: the pause it started with doesn't stop it.
func TestRunNowUpdatesWithAPausedRule(t *testing.T) {
	r := newRig(t)
	r.nas.returns("pool.query", []any{map[string]any{"name": "tank", "status": "ONLINE", "healthy": true}})
	updated, _ := r.apps(standardApps()...)
	settings, _ := json.Marshal(UpdateRule{})
	job := r.book(t, Job{Kind: KindUpdateRule, Arguments: settings, Title: "Update apps automatically every Sunday at 3:00 am",
		Paused: true, When: When{Repeat: "weekly", Weekday: "sunday", Time: "03:00"}, NextRun: at(r.now.Add(48 * time.Hour))})
	if err := r.sched.RunNow(context.Background(), job.ID); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	evs := r.feed(t)
	if fmt.Sprint(updated()) != "[plex sonarr]" || len(evs) != 1 || evs[0].Title != "Updated Plex and Sonarr" {
		t.Errorf("updated %v, feed %+v", updated(), evs)
	}
}

// A rule paused before it reached any app says it didn't update them.
func TestARulePausedBeforeAnyAppSaysSo(t *testing.T) {
	r := newRig(t)
	updated, _ := r.apps(standardApps()...)
	settings, _ := json.Marshal(UpdateRule{})
	job := r.book(t, Job{Kind: KindUpdateRule, Arguments: settings, Title: "Update apps automatically every day at 2:29 pm",
		When: When{Repeat: "daily", Time: "14:29"}, NextRun: at(r.now.Add(-time.Minute))})
	r.nas.on("pool.query", func([]any) (any, error) {
		_ = r.store.Pause(context.Background(), job.ID)
		return []any{map[string]any{"name": "tank", "status": "ONLINE", "healthy": true}}, nil
	})
	r.sched.runDue(context.Background())
	evs := r.feed(t)
	if len(updated()) != 0 || len(evs) != 1 || !strings.HasPrefix(evs[0].Title, "Didn't update apps:") ||
		evs[0].Summary != "It was paused or removed while it ran, so Plex and Sonarr were left alone." {
		t.Errorf("updated %v, feed %+v", updated(), evs)
	}
	got, _ := r.store.Get(context.Background(), job.ID)
	if got.LastStatus != "skipped" {
		t.Errorf("job = %+v", got)
	}
}

// The helper stopping partway through a rule still says what it did.
func TestARuleStoppedPartwaySaysWhatItDid(t *testing.T) {
	r := newRig(t)
	r.nas.returns("pool.query", []any{map[string]any{"name": "tank", "status": "ONLINE", "healthy": true}})
	apps := standardApps()
	apps[2].hangUpdate = true
	_, onUpdate := r.apps(apps...)
	updating := make(chan struct{})
	*onUpdate = func(name string) {
		if name == "sonarr" {
			close(updating)
		}
	}
	settings, _ := json.Marshal(UpdateRule{})
	job := r.book(t, Job{Kind: KindUpdateRule, Arguments: settings, Title: "Update apps automatically every day at 2:29 pm",
		When: When{Repeat: "daily", Time: "14:29"}, NextRun: at(r.now.Add(-time.Minute))})
	ctx, stop := context.WithCancel(context.Background())
	r.sched.runDue(ctx)
	waitFor(t, updating)
	stop()
	evs := r.feed(t)
	if len(evs) != 1 || evs[0].Title != "Updated Plex" || !strings.Contains(evs[0].Summary, "stopped while updating Sonarr") {
		t.Errorf("feed = %+v", evs)
	}
	got, _ := r.store.Get(context.Background(), job.ID)
	if got.LastStatus != "running" {
		t.Errorf("job = %+v, want it left unfinished", got)
	}
}

// A Run Now started before the scheduler's first pass isn't taken for a
// run the helper stopped partway through.
func TestTheFirstPassLeavesARunNowAlone(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	job := r.book(t, Job{Kind: KindCheckIn, Title: "Check in on the NAS every day at 8:00 am",
		When: When{Repeat: "daily", Time: "08:00"}, NextRun: at(r.now.Add(17 * time.Hour))})
	r.sched.reserve(job.ID)
	defer r.sched.release(job.ID)
	if err := r.store.Started(ctx, job.ID, r.now); err != nil {
		t.Fatalf("started: %v", err)
	}
	r.sched.interrupted(ctx)
	evs := r.feed(t)
	got, _ := r.store.Get(ctx, job.ID)
	if len(evs) != 0 || got.LastStatus != "running" {
		t.Errorf("feed = %+v, job = %+v", evs, got)
	}
}
