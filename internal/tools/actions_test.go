package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// truenasJob is a finished job in state.
func truenasJob(state, failure string) truenas.Job {
	return truenas.Job{ID: 1, State: state, Error: failure}
}

// quick shortens the actions' waits for a test.
func quick(t *testing.T) {
	t.Helper()
	saved := [...]time.Duration{shortJobWait, quickCheck, jobPoll, scanCheck}
	shortJobWait, quickCheck, jobPoll, scanCheck = 500*time.Millisecond, 200*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { shortJobWait, quickCheck, jobPoll, scanCheck = saved[0], saved[1], saved[2], saved[3] })
}

// jobs answers core.get_jobs from a table of job states, and hands out
// job ids for the methods that start them.
type jobs struct {
	mu     sync.Mutex
	next   int64
	states map[int64]map[string]any
}

func withJobs(nas *fakeNAS) *jobs {
	j := &jobs{next: 100, states: map[int64]map[string]any{}}
	nas.on("core.get_jobs", func(params []json.RawMessage) (any, *rpcFault) {
		var filters [][]any
		_ = json.Unmarshal(params[0], &filters)
		id := int64(filters[0][2].(float64))
		j.mu.Lock()
		defer j.mu.Unlock()
		if state, ok := j.states[id]; ok {
			return []any{state}, nil
		}
		return []any{}, nil
	})
	return j
}

// starts makes method start a job that ends in state (RUNNING stays
// running), with result.
func (j *jobs) starts(nas *fakeNAS, method, state string, result any, failure string) {
	nas.on(method, func([]json.RawMessage) (any, *rpcFault) {
		j.mu.Lock()
		defer j.mu.Unlock()
		j.next++
		j.states[j.next] = map[string]any{"id": j.next, "method": method, "state": state, "result": result, "error": failure}
		return j.next, nil
	})
}

func actionNamed(t *testing.T, tn Tool) *Action {
	t.Helper()
	if tn.Action == nil {
		t.Fatalf("%s isn't an action", tn.Def.Name)
	}
	return tn.Action
}

// approve describes then runs an action as the chat does, with the
// arguments its card settled on.
func approve(t *testing.T, a *Action, args string) (Card, Outcome, error) {
	t.Helper()
	ctx := context.Background()
	card, err := a.Describe(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Describe(%s): %v", args, err)
	}
	run := json.RawMessage(args)
	if len(card.Args) > 0 {
		run = card.Args
	}
	outcome, err := a.Run(ctx, run)
	return card, outcome, err
}

func TestAcknowledgeAlertsListsThemAndDismissesEach(t *testing.T) {
	nas := newFakeNAS()
	nas.returns("alert.list", []any{
		map[string]any{"uuid": "a1", "formatted": "Pool backup is 85% full.\nMore detail", "dismissed": false},
		map[string]any{"uuid": "a2", "formatted": "Certificate expires in 12 days.", "dismissed": false},
		map[string]any{"uuid": "a3", "formatted": "Old news.", "dismissed": true},
	})
	nas.returns("alert.dismiss", nil)
	action := actionNamed(t, acknowledgeAlerts(nas.client(t)))

	card, outcome, err := approve(t, action, `{"uuids":["a1","a2","a1"]}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if card.Title != "Acknowledge 2 alerts" || card.Detail != "• Pool backup is 85% full.\n• Certificate expires in 12 days." || card.Risk != RiskRoutine {
		t.Errorf("card = %+v", card)
	}
	if outcome.Message != "Acknowledged 2 alerts." {
		t.Errorf("outcome = %+v", outcome)
	}
	var dismissed []string
	for _, c := range nas.called("alert.dismiss") {
		var id string
		decodeParam(t, c, 0, &id)
		dismissed = append(dismissed, id)
	}
	if fmt.Sprint(dismissed) != "[a1 a2]" {
		t.Errorf("dismissed %v", dismissed)
	}

	// A dismissed alert, or one that doesn't exist, isn't offered.
	for _, args := range []string{`{"uuids":["a3"]}`, `{"uuids":["nope"]}`, `{"uuids":[]}`} {
		if _, err := action.Describe(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("Describe(%s) made a card", args)
		}
	}
}

func TestSnapshotNamesItInTheNASTimeZoneAndRunsWhatTheCardSaid(t *testing.T) {
	nas := newFakeNAS()
	nas.returns("pool.dataset.query", []any{map[string]any{"id": "tank/media", "children": []any{map[string]any{"id": "tank/media/tv"}}}})
	nas.returns("pool.snapshot.query", []any{})
	nas.returns("system.general.config", map[string]any{"timezone": "Australia/Brisbane"})
	nas.returns("pool.snapshot.create", map[string]any{"id": "tank/media@x"})
	action := actionNamed(t, createSnapshot(nas.client(t)))

	card, outcome, err := approve(t, action, `{"dataset":"tank/media"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	brisbane, _ := time.LoadLocation("Australia/Brisbane")
	want := "zephyr-" + time.Now().In(brisbane).Format("2006-01-02-15")
	var created map[string]any
	decodeParam(t, nas.called("pool.snapshot.create")[0], 0, &created)
	name, _ := created["name"].(string)
	if !strings.HasPrefix(name, want) || created["dataset"] != "tank/media" || created["recursive"] != false {
		t.Errorf("created %v, want a name starting %s", created, want)
	}
	if card.Title != "Take a snapshot of tank/media" || !strings.Contains(card.Detail, "Named "+name+".") ||
		!strings.Contains(card.Detail, "not the datasets inside it") {
		t.Errorf("card = %+v", card)
	}
	if outcome.Message != "Took snapshot tank/media@"+name+"." {
		t.Errorf("outcome = %+v", outcome)
	}

	// A name that's taken, or that ZFS won't have, gets no card.
	nas.returns("pool.snapshot.query", []any{map[string]any{"id": "tank/media@daily"}})
	for _, args := range []string{`{"dataset":"tank/media","name":"daily"}`, `{"dataset":"tank/media","name":"bad name"}`} {
		if _, err := action.Describe(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("Describe(%s) made a card", args)
		}
	}
}

func TestScrubBefore26UsesPoolScrubScrubAndSaysWhenTheLastOneFinished(t *testing.T) {
	quick(t)
	nas := newFakeNAS()
	j := withJobs(nas)
	finished := time.Now().Add(-34 * 24 * time.Hour).UnixMilli()
	nas.returns("pool.query", []any{map[string]any{
		"id": 7, "name": "tank", "healthy": true, "status": "ONLINE", "allocated": 12 << 40,
		"scan": map[string]any{"function": "SCRUB", "state": "FINISHED", "errors": 0, "end_time": map[string]any{"$date": finished}},
	}})
	j.starts(nas, "pool.scrub.scrub", "RUNNING", nil, "")
	action := actionNamed(t, scrubPool(nas.client(t)))

	card, outcome, err := approve(t, action, `{"pool":"tank"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if card.Title != "Start a scrub on tank" ||
		card.Detail != "tank will be slower for a few hours while ZFS reads and checks every block (12.0 TiB). Its last scrub finished 34 days ago and found no errors." {
		t.Errorf("card = %+v", card)
	}
	var name, verb string
	scrub := nas.called("pool.scrub.scrub")[0]
	decodeParam(t, scrub, 0, &name)
	decodeParam(t, scrub, 1, &verb)
	if name != "tank" || verb != "START" {
		t.Errorf("pool.scrub.scrub(%s, %s), want (tank, START)", name, verb)
	}
	// The job lasts as long as the scrub; the pool says how it went.
	if outcome.Message != "Started a scrub on tank." || outcome.Job == nil || outcome.Job.Report == nil {
		t.Fatalf("outcome = %+v", outcome)
	}

	// A refusal fails the job straight away.
	j.starts(nas, "pool.scrub.scrub", "FAILED", nil, "[EFAULT] Too many scrubs running")
	if _, err := action.Run(context.Background(), json.RawMessage(`{"pool":"tank"}`)); err == nil || err.Error() != "Too many scrubs running" {
		t.Errorf("Run = %v", err)
	}
}

func TestScrubOn26ChecksThePoolStarted(t *testing.T) {
	quick(t)
	nas := newFakeNAS()
	j := withJobs(nas)
	var mu sync.Mutex
	scanning := false
	nas.on("pool.query", func([]json.RawMessage) (any, *rpcFault) {
		mu.Lock()
		defer mu.Unlock()
		state := "FINISHED"
		if scanning {
			state = "SCANNING"
		}
		return []any{map[string]any{"id": 7, "name": "tank", "healthy": true, "scan": map[string]any{"function": "SCRUB", "state": state}}}, nil
	})
	j.starts(nas, "zpool.scrub.run", "RUNNING", nil, "")
	nas.on("zpool.scrub.run", func(params []json.RawMessage) (any, *rpcFault) {
		mu.Lock()
		scanning = true
		mu.Unlock()
		j.mu.Lock()
		defer j.mu.Unlock()
		j.next++
		j.states[j.next] = map[string]any{"id": j.next, "state": "RUNNING"}
		return j.next, nil
	})
	action := actionNamed(t, scrubPool(nas.client(t)))

	_, outcome, err := approve(t, action, `{"pool":"tank"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var run map[string]any
	decodeParam(t, nas.called("zpool.scrub.run")[0], 0, &run)
	if run["pool_name"] != "tank" || run["action"] != "START" || run["threshold"] != float64(1) || run["scan_type"] != "SCRUB" {
		t.Errorf("zpool.scrub.run(%v)", run)
	}
	if outcome.Job == nil || outcome.Job.ID == 0 {
		t.Errorf("outcome = %+v, want the scrub's job followed", outcome)
	}
	if len(nas.called("pool.scrub.scrub")) != 0 {
		t.Error("used the older method on 26")
	}

	// Declined without a word: the pool never starts scanning.
	mu.Lock()
	scanning = false
	mu.Unlock()
	j.starts(nas, "zpool.scrub.run", "SUCCESS", nil, "")
	if _, err := action.Run(context.Background(), json.RawMessage(`{"pool":"tank"}`)); err == nil || !strings.Contains(err.Error(), "didn't start") {
		t.Errorf("Run = %v, want it to say TrueNAS didn't start it", err)
	}
}

func TestScrubReportSaysWhatItFound(t *testing.T) {
	nas := newFakeNAS()
	tn := nas.client(t)
	report := scrubReport(tn, "tank")
	cases := []struct {
		scan  map[string]any
		title string
		bad   bool
	}{
		{map[string]any{"function": "SCRUB", "state": "FINISHED", "errors": 0}, "The scrub on tank finished and found no errors", false},
		{map[string]any{"function": "SCRUB", "state": "FINISHED", "errors": 3}, "The scrub on tank found 3 errors", true},
		{map[string]any{"function": "SCRUB", "state": "CANCELED"}, "The scrub on tank was cancelled", false},
	}
	for _, c := range cases {
		nas.returns("pool.query", []any{map[string]any{"name": "tank", "scan": c.scan}})
		got := report(context.Background(), truenasJob("SUCCESS", ""))
		if got.Title != c.title || got.Bad != c.bad {
			t.Errorf("scan %v: report = %+v, want %q bad=%v", c.scan, got, c.title, c.bad)
		}
	}
	if got := report(context.Background(), truenasJob("FAILED", "[EFAULT] Pool is not healthy")); !got.Bad || got.Summary != "Pool is not healthy" {
		t.Errorf("failed job: report = %+v", got)
	}
}

func TestScrubRefusesWhatCantBeDone(t *testing.T) {
	nas := newFakeNAS()
	nas.returns("pool.query", []any{map[string]any{"id": 7, "name": "tank", "scan": map[string]any{"function": "RESILVER", "state": "SCANNING", "percentage": 43.2}}})
	action := actionNamed(t, scrubPool(nas.client(t)))
	if _, err := action.Describe(context.Background(), json.RawMessage(`{"pool":"tank"}`)); err == nil || err.Error() != "tank is already running a resilver, 43% done" {
		t.Errorf("start during a resilver = %v", err)
	}
	if _, err := action.Describe(context.Background(), json.RawMessage(`{"pool":"tank","action":"cancel"}`)); err == nil {
		t.Error("offered to cancel a resilver as a scrub")
	}
}

func TestSnapshotOn2504UsesZFSSnapshot(t *testing.T) {
	nas := newFakeNAS()
	nas.returns("pool.dataset.query", []any{map[string]any{"id": "tank/media"}})
	nas.returns("zfs.snapshot.query", []any{})
	nas.returns("zfs.snapshot.create", map[string]any{"id": "tank/media@x"})
	action := actionNamed(t, createSnapshot(nas.client(t)))
	if _, _, err := approve(t, action, `{"dataset":"tank/media","name":"before-upgrade"}`); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var created map[string]any
	decodeParam(t, nas.called("zfs.snapshot.create")[0], 0, &created)
	if created["name"] != "before-upgrade" || len(nas.called("zfs.snapshot.query")) == 0 {
		t.Errorf("created %v", created)
	}
}

func TestBackupsThatWouldDoNothingArentOffered(t *testing.T) {
	nas := newFakeNAS()
	action := actionNamed(t, runBackupTask(nas.client(t)))
	cases := []struct {
		kind string
		task map[string]any
		want string
	}{
		{"rsync", map[string]any{"id": 1, "desc": "Photos", "locked": true}, "dataset is locked"},
		{"replication", map[string]any{"id": 1, "name": "tank-to-backup", "enabled": false}, "turned off"},
		{"replication", map[string]any{"id": 1, "name": "tank-to-backup", "enabled": true, "state": map[string]any{"state": "RUNNING"}}, "already running"},
	}
	for _, c := range cases {
		nas.returns(backupKinds[c.kind].query, []any{c.task})
		_, err := action.Describe(context.Background(), json.RawMessage(fmt.Sprintf(`{"kind":%q,"id":1}`, c.kind)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: Describe = %v, want %q", c.kind, c.task, err, c.want)
		}
	}
}

func TestBackupRunsNowAndIsFollowedWhenItTakesAWhile(t *testing.T) {
	quick(t)
	nas := newFakeNAS()
	j := withJobs(nas)
	ranAt := time.Now().Add(-49 * time.Hour).UnixMilli()
	nas.returns("cloudsync.query", []any{map[string]any{
		"id": 3, "description": "Offsite", "direction": "PUSH", "path": "/mnt/tank/documents", "enabled": true,
		"credentials": map[string]any{"name": "Backblaze B2"},
		"job":         map[string]any{"state": "SUCCESS", "time_finished": map[string]any{"$date": ranAt}},
	}})
	j.starts(nas, "cloudsync.sync", "RUNNING", nil, "")
	action := actionNamed(t, runBackupTask(nas.client(t)))

	card, outcome, err := approve(t, action, `{"kind":"cloud_sync","id":3}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if card.Title != "Run the cloud sync “Offsite” now" ||
		card.Detail != "Copies /mnt/tank/documents to Backblaze B2. Its last run, 2 days ago, succeeded." {
		t.Errorf("card = %+v", card)
	}
	if outcome.Message != "Started the cloud sync “Offsite”." || outcome.Job == nil ||
		outcome.Job.Done != "The cloud sync “Offsite” finished" {
		t.Errorf("outcome = %+v", outcome)
	}

	// One that fails straight away says why.
	j.starts(nas, "cloudsync.sync", "FAILED", nil, "[EFAULT] Credentials are invalid\nTraceback")
	if _, err := action.Run(context.Background(), json.RawMessage(`{"kind":"cloud_sync","id":3}`)); err == nil || err.Error() != "Credentials are invalid" {
		t.Errorf("Run = %v", err)
	}

	// One that's already running isn't offered.
	nas.returns("cloudsync.query", []any{map[string]any{"id": 3, "description": "Offsite", "job": map[string]any{"state": "RUNNING"}}})
	if _, err := action.Describe(context.Background(), json.RawMessage(`{"kind":"cloud_sync","id":3}`)); err == nil {
		t.Error("offered to run a task that's already running")
	}
}

func TestRestartingAnAppStopsThenStarts(t *testing.T) {
	quick(t)
	nas := newFakeNAS()
	j := withJobs(nas)
	nas.returns("app.query", []any{map[string]any{"name": "plex", "state": "RUNNING", "metadata": map[string]any{"title": "Plex"}}})
	j.starts(nas, "app.stop", "SUCCESS", nil, "")
	j.starts(nas, "app.start", "SUCCESS", nil, "")
	action := actionNamed(t, appAction(nas.client(t)))

	card, outcome, err := approve(t, action, `{"app":"plex","action":"restart"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if card.Title != "Restart Plex" || card.Risk != RiskDisruptive {
		t.Errorf("card = %+v", card)
	}
	if outcome.Message != "Restarted Plex." || outcome.Job != nil {
		t.Errorf("outcome = %+v", outcome)
	}
	if len(nas.called("app.stop")) != 1 || len(nas.called("app.start")) != 1 {
		t.Errorf("stop %d, start %d times", len(nas.called("app.stop")), len(nas.called("app.start")))
	}
	if _, err := action.Describe(context.Background(), json.RawMessage(`{"app":"plex","action":"start"}`)); err == nil {
		t.Error("offered to start an app that's running")
	}
}

func TestUpdatingAnAppGoesToTheVersionOnTheCard(t *testing.T) {
	quick(t)
	nas := newFakeNAS()
	j := withJobs(nas)
	nas.returns("app.query", []any{map[string]any{
		"name": "plex", "state": "RUNNING", "upgrade_available": true, "version": "1.2.0", "human_version": "1.41.2_1.2.0",
		"metadata": map[string]any{"title": "Plex"},
	}})
	nas.returns("app.upgrade_summary", map[string]any{"upgrade_version": "1.3.0", "upgrade_human_version": "1.41.3_1.3.0"})
	j.starts(nas, "app.upgrade", "RUNNING", nil, "")
	action := actionNamed(t, updateApp(nas.client(t)))

	card, outcome, err := approve(t, action, `{"app":"plex"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if card.Title != "Update Plex" || !strings.HasPrefix(card.Detail, "From 1.41.2_1.2.0 to 1.41.3_1.3.0.") || card.Risk != RiskDisruptive {
		t.Errorf("card = %+v", card)
	}
	var name string
	var options map[string]any
	upgrade := nas.called("app.upgrade")[0]
	decodeParam(t, upgrade, 0, &name)
	decodeParam(t, upgrade, 1, &options)
	if name != "plex" || options["app_version"] != "1.3.0" {
		t.Errorf("app.upgrade(%s, %v)", name, options)
	}
	if outcome.Job == nil || outcome.Job.Done != "Updated Plex to 1.41.3_1.3.0" {
		t.Errorf("outcome = %+v", outcome)
	}

	// A custom app updates by pulling newer images, with no version.
	nas.returns("app.query", []any{map[string]any{"name": "mine", "state": "RUNNING", "custom_app": true, "upgrade_available": true}})
	card, outcome, err = approve(t, action, `{"app":"mine"}`)
	if err != nil {
		t.Fatalf("custom Run: %v", err)
	}
	if card.Title != "Update mine" || !strings.Contains(card.Detail, "newer images") || len(card.Args) != 0 {
		t.Errorf("custom card = %+v", card)
	}
	if calls := nas.called("app.upgrade"); len(calls[len(calls)-1].params) != 1 {
		t.Errorf("custom app.upgrade params = %s, want just the name", calls[len(calls)-1].params)
	}

	// TrueNAS only updates a running app.
	nas.returns("app.query", []any{map[string]any{"name": "plex", "state": "STOPPED", "upgrade_available": true}})
	if _, err := action.Describe(context.Background(), json.RawMessage(`{"app":"plex"}`)); err == nil || !strings.Contains(err.Error(), "start it first") {
		t.Errorf("stopped app = %v", err)
	}
}

func TestRollbackPicksTheNewestEarlierVersion(t *testing.T) {
	quick(t)
	nas := newFakeNAS()
	j := withJobs(nas)
	nas.returns("app.query", []any{map[string]any{"name": "plex", "state": "RUNNING", "version": "1.11.0", "metadata": map[string]any{"title": "Plex"}}})
	nas.returns("app.rollback_versions", []any{"1.2.0", "1.10.0", "1.9.0"})
	j.starts(nas, "app.rollback", "SUCCESS", nil, "")
	action := actionNamed(t, rollbackApp(nas.client(t)))

	card, outcome, err := approve(t, action, `{"app":"plex"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if card.Title != "Roll Plex back to 1.10.0" {
		t.Errorf("card = %+v", card)
	}
	var options map[string]any
	decodeParam(t, nas.called("app.rollback")[0], 1, &options)
	if options["app_version"] != "1.10.0" || options["rollback_snapshot"] != true {
		t.Errorf("app.rollback options %v", options)
	}
	if outcome.Message != "Rolled Plex back to 1.10.0." {
		t.Errorf("outcome = %+v", outcome)
	}
}

func TestVMsStopCleanly(t *testing.T) {
	quick(t)
	nas := newFakeNAS()
	j := withJobs(nas)
	nas.returns("vm.query", []any{map[string]any{"id": 4, "name": "HomeAssistant", "shutdown_timeout": 90, "status": map[string]any{"state": "RUNNING"}}})
	j.starts(nas, "vm.stop", "SUCCESS", nil, "")
	action := actionNamed(t, vmAction(nas.client(t)))

	card, outcome, err := approve(t, action, `{"name":"HomeAssistant","action":"stop"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if card.Title != "Stop the VM “HomeAssistant”" || card.Risk != RiskDisruptive ||
		!strings.HasSuffix(card.Detail, "If it hasn't shut down after 90 seconds, it's switched off.") {
		t.Errorf("card = %+v", card)
	}
	var id int64
	var options map[string]any
	stop := nas.called("vm.stop")[0]
	decodeParam(t, stop, 0, &id)
	decodeParam(t, stop, 1, &options)
	// Without force_after_timeout, TrueNAS reports a stop the guest
	// ignored as done.
	if id != 4 || options["force"] != false || options["force_after_timeout"] != true {
		t.Errorf("vm.stop(%d, %v)", id, options)
	}
	if outcome.Message != "Stopped the VM “HomeAssistant”." {
		t.Errorf("outcome = %+v", outcome)
	}
}

func TestAContainerStartsWithItsIDAlone(t *testing.T) {
	quick(t)
	nas := newFakeNAS()
	nas.returns("container.query", []any{map[string]any{"id": 9, "name": "web", "status": map[string]any{"state": "STOPPED"}}})
	nas.returns("container.start", nil)
	action := actionNamed(t, containerAction(nas.client(t)))
	card, outcome, err := approve(t, action, `{"name":"web","action":"start"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	start := nas.called("container.start")[0]
	var id int64
	decodeParam(t, start, 0, &id)
	// container.start takes the id alone, and isn't a job.
	if id != 9 || len(start.params) != 1 || len(nas.called("core.get_jobs")) != 0 {
		t.Errorf("container.start%s, %d job polls", start.params, len(nas.called("core.get_jobs")))
	}
	if card.Title != "Start the container “web”" || outcome.Message != "Started the container “web”." {
		t.Errorf("card %+v, outcome %+v", card, outcome)
	}
}

func TestContainersNeed26(t *testing.T) {
	nas := newFakeNAS()
	action := actionNamed(t, containerAction(nas.client(t)))
	_, err := action.Describe(context.Background(), json.RawMessage(`{"name":"web","action":"start"}`))
	if err == nil || err.Error() != "this TrueNAS version has no containers" {
		t.Errorf("Describe = %v", err)
	}
}

func TestServicesUseServiceControlOrTheOlderMethods(t *testing.T) {
	quick(t)
	nas := newFakeNAS()
	j := withJobs(nas)
	nas.returns("service.query", []any{map[string]any{"service": "cifs", "state": "RUNNING", "enable": true}})
	j.starts(nas, "service.control", "SUCCESS", true, "")
	action := actionNamed(t, serviceAction(nas.client(t)))

	card, outcome, err := approve(t, action, `{"service":"smb","action":"restart"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if card.Title != "Restart the SMB service" || card.Risk != RiskDisruptive || !strings.Contains(card.Detail, "file shares drop") {
		t.Errorf("card = %+v", card)
	}
	var verb, service string
	control := nas.called("service.control")[0]
	decodeParam(t, control, 0, &verb)
	decodeParam(t, control, 1, &service)
	if verb != "RESTART" || service != "cifs" || outcome.Message != "Restarted the SMB service." {
		t.Errorf("service.control(%s, %s), outcome %+v", verb, service, outcome)
	}

	// 25.04 has no service.control.
	nas.on("service.control", func([]json.RawMessage) (any, *rpcFault) { return nil, notFound })
	nas.returns("service.restart", true)
	if _, err := action.Run(context.Background(), json.RawMessage(`{"service":"cifs","action":"restart"}`)); err != nil {
		t.Fatalf("Run on 25.04: %v", err)
	}
	if len(nas.called("service.restart")) != 1 {
		t.Error("didn't fall back to service.restart")
	}

	// A start that leaves it stopped says so.
	nas.returns("service.query", []any{map[string]any{"service": "nfs", "state": "STOPPED", "enable": false}})
	nas.returns("service.start", false)
	if _, err := action.Run(context.Background(), json.RawMessage(`{"service":"nfs","action":"start"}`)); err == nil || !strings.Contains(err.Error(), "didn't come up") {
		t.Errorf("failed start = %v", err)
	}
}

func TestEveryActionHasAStrictSchema(t *testing.T) {
	for _, tool := range actionTools(nil) {
		var schema map[string]any
		if err := json.Unmarshal(tool.Def.Parameters, &schema); err != nil {
			t.Errorf("%s: %v", tool.Def.Name, err)
			continue
		}
		if schema["additionalProperties"] != false {
			t.Errorf("%s allows extra properties", tool.Def.Name)
		}
		// Claude's strict mode refuses numeric and length limits.
		for _, banned := range []string{"minItems", "maxItems", "minimum", "maximum", "minLength", "maxLength"} {
			if strings.Contains(string(tool.Def.Parameters), banned) {
				t.Errorf("%s uses %s", tool.Def.Name, banned)
			}
		}
		if tool.StatusLine == "" || tool.Action.Describe == nil || tool.Action.Run == nil {
			t.Errorf("%s is missing a part", tool.Def.Name)
		}
	}
}

func TestCardNamesStayTellable(t *testing.T) {
	long := "tank/" + strings.Repeat("a", 150) + "/photos-2019"
	if got := plain(long); !strings.HasSuffix(got, "/photos-2019") || !strings.HasPrefix(got, "tank/") || !strings.Contains(got, "…") {
		t.Errorf("plain(long) = %q", got)
	}
	if got := plain("media\n\tSTOP\u2028evil"); got != "media STOP evil" {
		t.Errorf("plain kept control characters: %q", got)
	}
	app := func(name, title string) map[string]any {
		return map[string]any{"name": name, "metadata": map[string]any{"title": title}}
	}
	for _, c := range []struct {
		app  map[string]any
		want string
	}{
		{app("plex", "Plex"), "Plex"},
		{app("plex2", "Plex"), "Plex (plex2)"},
		{map[string]any{"name": "mine"}, "mine"},
	} {
		if got := appTitle(c.app); got != c.want {
			t.Errorf("appTitle(%v) = %q, want %q", c.app, got, c.want)
		}
	}
}
