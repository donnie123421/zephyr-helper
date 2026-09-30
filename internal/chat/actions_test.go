package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/actions"
	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/llm"
	"github.com/donnie123421/zephyr-helper/internal/tools"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// fakeAction is an action tool that records what it was asked to run.
type fakeAction struct {
	mu          sync.Mutex
	ran         []string
	describeErr error
	runErr      error
	job         *tools.Job
	// settled, when set, are the arguments the card settles on.
	settled json.RawMessage
}

func (f *fakeAction) tool(name string) tools.Tool {
	return tools.Tool{
		Def:        tools.Definition{Name: name, Parameters: json.RawMessage(`{}`)},
		StatusLine: "Starting the scrub…",
		Action: &tools.Action{
			Describe: func(_ context.Context, args json.RawMessage) (tools.Card, error) {
				if f.describeErr != nil {
					return tools.Card{}, f.describeErr
				}
				return tools.Card{Title: "Start a scrub on tank", Detail: "tank will be slower for a few hours.", Risk: tools.RiskRoutine, Args: f.settled}, nil
			},
			Run: func(_ context.Context, args json.RawMessage) (tools.Outcome, error) {
				f.mu.Lock()
				f.ran = append(f.ran, string(args))
				f.mu.Unlock()
				if f.runErr != nil {
					return tools.Outcome{}, f.runErr
				}
				return tools.Outcome{Message: "Started a scrub on tank.", Job: f.job}, nil
			},
		},
	}
}

func (f *fakeAction) runs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ran)
}

// fakeJobs ends every job the way it's told.
type fakeJobs struct{ ended truenas.Job }

func (j fakeJobs) WaitJob(_ context.Context, id int64, _ time.Duration) (truenas.Job, error) {
	job := j.ended
	job.ID = id
	return job, nil
}

func openEvents(t *testing.T) *events.Store {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// actionTurn is a turn in a chat with actions on, recording in a fresh
// events store.
type actionTurn struct {
	h      *Handler
	acts   *turnActions
	log    *actions.Log
	events *events.Store
	out    frames
}

func newActionTurn(t *testing.T, fake *fakeAction, jobs actions.JobWaiter) *actionTurn {
	t.Helper()
	reg := tools.NewWith(append(testRegistryTools(), fake.tool("scrub_pool"))...)
	store := openEvents(t)
	if jobs == nil {
		jobs = fakeJobs{ended: truenas.Job{State: "SUCCESS"}}
	}
	log := actions.New(context.Background(), store, jobs)
	return &actionTurn{
		h:      &Handler{tools: reg},
		acts:   &turnActions{tools: reg, approvals: newApprovals(), log: log, timeout: time.Minute},
		log:    log,
		events: store,
	}
}

// answering sends frames to the turn's record and answers every card.
func (a *actionTurn) answering(approve bool) func(any) error {
	return func(v any) error {
		_ = a.out.send(v)
		if req, ok := v.(approvalRequestEvent); ok {
			go a.acts.approvals.deliver(req.ID, approve)
		}
		return nil
	}
}

func (a *actionTurn) framesOf(kind string) []map[string]any {
	var out []map[string]any
	for _, f := range a.out {
		if f["type"] == kind {
			out = append(out, f)
		}
	}
	return out
}

func (a *actionTurn) recorded(t *testing.T) []events.Event {
	t.Helper()
	a.log.Wait()
	evs, err := a.events.List(context.Background(), events.ListFilter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return evs
}

func testRegistryTools() []tools.Tool {
	return []tools.Tool{{
		Def:        tools.Definition{Name: "list_pools", Parameters: json.RawMessage(`{}`)},
		StatusLine: "Checking pools…",
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return `{"pools":["tank"]}`, nil
		},
	}}
}

func scrubCall(id string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: "scrub_pool", Arguments: json.RawMessage(`{"pool":"tank"}`)}
}

func TestAnApprovedActionRunsAndIsRecorded(t *testing.T) {
	fake := &fakeAction{}
	turn := newActionTurn(t, fake, nil)
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: []llm.ToolCall{scrubCall("call_1")}}},
		{deltas: []string{"The scrub is running."}},
	}}
	var statuses []string
	onStatus := func(s string) { statuses = append(statuses, s) }

	if _, err := turn.h.runTurn(context.Background(), turn.answering(true), onStatus, &fakeProvider{}, session, turn.acts); err != nil {
		t.Fatalf("runTurn: %v", err)
	}

	if got := fmt.Sprint(turn.out.types()); got != "[approval_request action_result tool_used delta]" {
		t.Fatalf("frames = %v", got)
	}
	req, result, used := turn.out[0], turn.out[1], turn.out[2]
	if req["tool"] != "scrub_pool" || req["title"] != "Start a scrub on tank" || req["risk"] != "routine" ||
		req["detail"] != "tank will be slower for a few hours." || req["expires_at"] == nil || req["id"] == "" {
		t.Errorf("approval_request = %v", req)
	}
	if result["id"] != req["id"] || result["status"] != "done" || result["message"] != "Started a scrub on tank." {
		t.Errorf("action_result = %v", result)
	}
	action, _ := used["action"].(map[string]any)
	if used["name"] != "scrub_pool" || action["id"] != req["id"] || action["status"] != "done" || action["title"] != "Start a scrub on tank" {
		t.Errorf("tool_used = %v", used)
	}
	if fmt.Sprint(statuses) != "[Starting the scrub…]" {
		t.Errorf("statuses = %v", statuses)
	}
	if fake.runs() != 1 {
		t.Errorf("ran %d times, want 1", fake.runs())
	}
	if r := session.results[0][0]; r.CallID != "call_1" || r.IsError || !strings.Contains(r.Content, `"status":"done"`) {
		t.Errorf("result for the model = %+v", r)
	}

	evs := turn.recorded(t)
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want 1", evs)
	}
	ev := evs[0]
	if ev.Kind != events.KindJob || ev.Severity != events.SeverityInfo || ev.Title != "Started a scrub on tank" ||
		ev.DedupeKey != events.AssistantKey(req["id"].(string), events.AssistantRan) ||
		!strings.Contains(string(ev.Body), `"status":"done"`) {
		t.Errorf("event = %+v, body %s", ev, ev.Body)
	}
}

func TestADeclinedActionDoesNotRun(t *testing.T) {
	fake := &fakeAction{}
	turn := newActionTurn(t, fake, nil)
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: []llm.ToolCall{scrubCall("call_1")}}},
		{deltas: []string{"OK, I won't."}},
	}}
	if _, err := turn.h.runTurn(context.Background(), turn.answering(false), func(string) {}, &fakeProvider{}, session, turn.acts); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if fake.runs() != 0 {
		t.Fatalf("a declined action ran")
	}
	if results := turn.framesOf("action_result"); len(results) != 1 || results[0]["status"] != "declined" {
		t.Errorf("action_result = %v", results)
	}
	if r := session.results[0][0]; r.IsError || !strings.Contains(r.Content, `"status":"declined"`) {
		t.Errorf("result for the model = %+v", r)
	}
	if evs := turn.recorded(t); len(evs) != 0 {
		t.Errorf("a declined action was recorded: %+v", evs)
	}
}

func TestAnUnansweredCardExpires(t *testing.T) {
	fake := &fakeAction{}
	turn := newActionTurn(t, fake, nil)
	turn.acts.timeout = 20 * time.Millisecond
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: []llm.ToolCall{scrubCall("call_1")}}},
		{deltas: []string{"Let me know."}},
	}}
	if _, err := turn.h.runTurn(context.Background(), turn.out.send, func(string) {}, &fakeProvider{}, session, turn.acts); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if fake.runs() != 0 {
		t.Fatalf("an unanswered action ran")
	}
	if results := turn.framesOf("action_result"); len(results) != 1 || results[0]["status"] != "expired" {
		t.Errorf("action_result = %v", results)
	}
	// A late answer finds nothing waiting.
	req := turn.framesOf("approval_request")[0]
	turn.acts.approvals.deliver(req["id"].(string), true)
	if fake.runs() != 0 {
		t.Errorf("a late approval ran the action")
	}
}

func TestStoppingWhileACardWaitsCancelsIt(t *testing.T) {
	fake := &fakeAction{}
	turn := newActionTurn(t, fake, nil)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	send := func(v any) error {
		_ = turn.out.send(v)
		if _, ok := v.(approvalRequestEvent); ok {
			go stop()
		}
		return nil
	}
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: []llm.ToolCall{scrubCall("call_1")}}},
		{err: context.Canceled},
	}}
	if _, err := turn.h.runTurn(ctx, send, func(string) {}, &fakeProvider{}, session, turn.acts); !errors.Is(err, context.Canceled) {
		t.Fatalf("runTurn err = %v, want cancelled", err)
	}
	if fake.runs() != 0 {
		t.Fatalf("an action ran after the reply was stopped")
	}
	if results := turn.framesOf("action_result"); len(results) != 1 || results[0]["status"] != "cancelled" {
		t.Errorf("action_result = %v", results)
	}
	// The session still gets a result for the call, or the next request
	// would carry a tool call with no answer.
	if len(session.results) != 1 || !strings.Contains(session.results[0][0].Content, `"status":"cancelled"`) {
		t.Errorf("results = %+v", session.results)
	}
}

func TestAtMostFiveActionsPerReply(t *testing.T) {
	fake := &fakeAction{}
	turn := newActionTurn(t, fake, nil)
	var calls []llm.ToolCall
	for i := 0; i < 6; i++ {
		calls = append(calls, scrubCall(fmt.Sprintf("call_%d", i)))
	}
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: calls}},
		{deltas: []string{"Done."}},
	}}
	if _, err := turn.h.runTurn(context.Background(), turn.answering(true), func(string) {}, &fakeProvider{}, session, turn.acts); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if cards := turn.framesOf("approval_request"); len(cards) != 5 {
		t.Errorf("%d cards, want 5", len(cards))
	}
	if fake.runs() != 5 {
		t.Errorf("ran %d, want 5", fake.runs())
	}
	if r := session.results[0][5]; r.CallID != "call_5" || !r.IsError || !strings.Contains(r.Content, "At most 5 actions") {
		t.Errorf("sixth result = %+v", r)
	}
}

func TestReadsAndActionsInOneStepKeepTheirOrder(t *testing.T) {
	fake := &fakeAction{}
	turn := newActionTurn(t, fake, nil)
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: []llm.ToolCall{
			{ID: "read_1", Name: "list_pools"}, scrubCall("act_1"), {ID: "read_2", Name: "list_pools"},
		}}},
		{deltas: []string{"Done."}},
	}}
	if _, err := turn.h.runTurn(context.Background(), turn.answering(true), func(string) {}, &fakeProvider{}, session, turn.acts); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	var ids []string
	for _, r := range session.results[0] {
		ids = append(ids, r.CallID)
	}
	if fmt.Sprint(ids) != "[read_1 act_1 read_2]" {
		t.Errorf("result order = %v", ids)
	}
}

func TestADescribeErrorGoesBackToTheModelWithoutACard(t *testing.T) {
	fake := &fakeAction{describeErr: errors.New(`there's no pool named "tanks"`)}
	turn := newActionTurn(t, fake, nil)
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: []llm.ToolCall{scrubCall("call_1")}}},
		{deltas: []string{"Which pool?"}},
	}}
	if _, err := turn.h.runTurn(context.Background(), turn.answering(true), func(string) {}, &fakeProvider{}, session, turn.acts); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if cards := turn.framesOf("approval_request"); len(cards) != 0 {
		t.Errorf("cards = %v, want none", cards)
	}
	if r := session.results[0][0]; !r.IsError || !strings.Contains(r.Content, "no pool named") {
		t.Errorf("result = %+v", r)
	}
}

func TestActionsDontRunInAChatWithoutThem(t *testing.T) {
	fake := &fakeAction{}
	turn := newActionTurn(t, fake, nil)
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: []llm.ToolCall{scrubCall("call_1")}}},
		{deltas: []string{"I can't."}},
	}}
	if _, err := turn.h.runTurn(context.Background(), turn.answering(true), func(string) {}, &fakeProvider{}, session, nil); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if fake.runs() != 0 || len(turn.framesOf("approval_request")) != 0 {
		t.Fatalf("an action ran, or asked, in a chat without actions")
	}
	if r := session.results[0][0]; !r.IsError || !strings.Contains(r.Content, "unknown tool") {
		t.Errorf("result = %+v", r)
	}
	// Nor is it offered.
	for _, tool := range (&Handler{tools: turn.h.tools}).toolList(false) {
		if tool.Name == "scrub_pool" {
			t.Errorf("an action was offered in a chat without actions")
		}
	}
}

func TestAFailedActionIsReportedAndRecorded(t *testing.T) {
	fake := &fakeAction{runErr: errors.New("the pool is exported")}
	turn := newActionTurn(t, fake, nil)
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: []llm.ToolCall{scrubCall("call_1")}}},
		{deltas: []string{"It didn't work."}},
	}}
	if _, err := turn.h.runTurn(context.Background(), turn.answering(true), func(string) {}, &fakeProvider{}, session, turn.acts); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	results := turn.framesOf("action_result")
	if len(results) != 1 || results[0]["status"] != "failed" ||
		results[0]["message"] != "The pool is exported" {
		t.Errorf("action_result = %v", results)
	}
	if r := session.results[0][0]; !r.IsError || !strings.Contains(r.Content, "the pool is exported") {
		t.Errorf("result = %+v", r)
	}
	evs := turn.recorded(t)
	if len(evs) != 1 || evs[0].Severity != events.SeverityWarning || evs[0].Title != "Couldn't start a scrub on tank" ||
		evs[0].Summary != "the pool is exported" {
		t.Errorf("events = %+v", evs)
	}
}

func TestAJobAnActionStartedIsFollowedToItsEnd(t *testing.T) {
	fake := &fakeAction{job: &tools.Job{ID: 42, Done: "Updated Plex", Failed: "Plex didn't update"}}
	turn := newActionTurn(t, fake, fakeJobs{ended: truenas.Job{State: "FAILED", Error: "[EFAULT] Failed to pull image\nTraceback …"}})
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: []llm.ToolCall{scrubCall("call_1")}}},
		{deltas: []string{"It's updating."}},
	}}
	if _, err := turn.h.runTurn(context.Background(), turn.answering(true), func(string) {}, &fakeProvider{}, session, turn.acts); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if results := turn.framesOf("action_result"); len(results) != 1 || results[0]["status"] != "started" {
		t.Errorf("action_result = %v", results)
	}
	if r := session.results[0][0]; !strings.Contains(r.Content, `"status":"started"`) || !strings.Contains(r.Content, "activity feed") {
		t.Errorf("result = %+v", r)
	}

	evs := turn.recorded(t)
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want the start and the failure", evs)
	}
	var failure *events.Event
	for i := range evs {
		if events.IsAssistantJobFailure(evs[i]) {
			failure = &evs[i]
		}
	}
	if failure == nil || failure.Title != "Plex didn't update" || failure.Summary != "Failed to pull image" ||
		failure.Severity != events.SeverityWarning {
		t.Fatalf("failure = %+v", failure)
	}
	// The jobs poller leaves it to this entry.
	if !turn.log.Following(42) || turn.log.Following(43) {
		t.Errorf("Following = %v/%v, want the action's job only", turn.log.Following(42), turn.log.Following(43))
	}
}

func TestAnActionRunsWithTheArgumentsItsCardSettledOn(t *testing.T) {
	fake := &fakeAction{settled: json.RawMessage(`{"pool":"tank","name":"zephyr-2026-09-30-1415"}`)}
	turn := newActionTurn(t, fake, nil)
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: []llm.ToolCall{scrubCall("call_1")}}},
		{deltas: []string{"Done."}},
	}}
	if _, err := turn.h.runTurn(context.Background(), turn.answering(true), func(string) {}, &fakeProvider{}, session, turn.acts); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.ran) != 1 || fake.ran[0] != string(fake.settled) {
		t.Errorf("ran with %v, want the card's %s", fake.ran, fake.settled)
	}
}

func TestAJobCanReportHowItEnded(t *testing.T) {
	fake := &fakeAction{job: &tools.Job{ID: 7, Report: func(context.Context, truenas.Job) tools.Ending {
		return tools.Ending{Title: "The scrub on tank found 3 errors", Summary: "Check the pool.", Bad: true}
	}}}
	turn := newActionTurn(t, fake, fakeJobs{ended: truenas.Job{State: "SUCCESS"}})
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{ToolCalls: []llm.ToolCall{scrubCall("call_1")}}},
		{deltas: []string{"Scrubbing."}},
	}}
	if _, err := turn.h.runTurn(context.Background(), turn.answering(true), func(string) {}, &fakeProvider{}, session, turn.acts); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	var found *events.Event
	evs := turn.recorded(t)
	for i := range evs {
		if events.IsAssistantJobFailure(evs[i]) {
			found = &evs[i]
		}
	}
	// A job that succeeded but found errors still warns, and pushes.
	if found == nil || found.Title != "The scrub on tank found 3 errors" || found.Summary != "Check the pool." {
		t.Fatalf("events = %+v", evs)
	}
}
