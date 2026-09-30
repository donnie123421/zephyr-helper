package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
	"github.com/donnie123421/zephyr-helper/internal/actions"
	"github.com/donnie123421/zephyr-helper/internal/conversations"
	"github.com/donnie123421/zephyr-helper/internal/tools"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// An approved action ran, then the model's next step failed (a rate limit, a
// dropped connection to Anthropic, a bad key). The saved chat should still
// keep the card and what became of it.
func TestAnActionThatRanIsSavedWhenItsTurnFails(t *testing.T) {
	fake := &fakeAction{}
	reg := tools.NewWith(append(testRegistryTools(), fake.tool("scrub_pool"))...)
	script := &claudeScript{steps: []any{
		"Hello.",
		toolCall{name: "scrub_pool", input: `{"pool":"tank"}`},
		http.StatusUnauthorized, // the step after the action fails
	}}
	claude := httptest.NewServer(script)
	defer claude.Close()
	saved := openConversations(t)
	acts := actions.New(context.Background(), openEvents(t), fakeJobs{ended: truenas.Job{State: "SUCCESS"}})
	helper := claudeHelperWithActions(t, claude.URL, saved, reg, nil, acts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _ := dialReady(ctx, t, helper, "?actions=1")
	var id string
	first := ask(ctx, t, conn, "hi")
	if first[len(first)-1]["type"] != "done" {
		t.Fatalf("first turn = %v", first)
	}
	for _, f := range first {
		if f["type"] == "conversation" {
			id, _ = f["id"].(string)
		}
	}
	if err := wsjson.Write(ctx, conn, inboundEvent{Type: "user_message", Content: "Scrub tank"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	for {
		frame := readFrame(ctx, t, conn)
		if frame["type"] == "conversation" {
			id, _ = frame["id"].(string)
		}
		if frame["type"] == "approval_request" {
			_ = wsjson.Write(ctx, conn, inboundEvent{Type: "approval_response", ID: frame["id"].(string), Approved: true})
		}
		if frame["type"] == "done" || frame["type"] == "error" {
			break
		}
	}
	if fake.runs() != 1 {
		t.Fatalf("the action ran %d times, want 1", fake.runs())
	}
	conv, err := saved.Get(ctx, id)
	if err != nil {
		t.Fatalf("saved: %v", err)
	}
	kept := false
	for _, m := range conv.Messages {
		for _, use := range m.Tools {
			if use.Action != nil {
				kept = true
			}
		}
	}
	if !kept {
		t.Errorf("the scrub ran, but the saved chat has no record of its card: %d messages, last %q",
			len(conv.Messages), conv.Messages[len(conv.Messages)-1].Content)
	}
}

// A reopened chat tells the model what its actions did, even for a reply
// with no text of its own.
func TestReplayTellsTheModelWhatActionsDid(t *testing.T) {
	done := conversations.ToolUse{Name: "scrub_pool", Action: &conversations.ActionRecord{
		ID: "a", Title: "Start a scrub on tank", Status: actionDone, Message: "Started a scrub on tank.",
	}}
	declined := conversations.ToolUse{Name: "service_action", Action: &conversations.ActionRecord{
		ID: "b", Title: "Restart the SMB service", Status: actionDeclined,
	}}
	got := replayable([]conversations.Message{
		{Role: conversations.RoleUser, Content: "Scrub tank and restart SMB"},
		{Role: conversations.RoleAssistant, Content: "", Tools: []conversations.ToolUse{done, declined}},
		{Role: conversations.RoleUser, Content: "Thanks"},
	})
	if len(got) != 3 || got[1].Role != conversations.RoleAssistant {
		t.Fatalf("replayed %+v", got)
	}
	want := "(Actions in this reply, which the user was asked to approve: “Start a scrub on tank”: Started a scrub on tank.; “Restart the SMB service”: the user declined it)"
	if got[1].Content != want {
		t.Errorf("replayed reply = %q, want %q", got[1].Content, want)
	}
}

// Retry won't write again an answer whose actions ran.
func TestRetryRefusesAnAnswerWhoseActionsRan(t *testing.T) {
	fake := &fakeAction{}
	reg := tools.NewWith(append(testRegistryTools(), fake.tool("scrub_pool"))...)
	script := &claudeScript{steps: []any{toolCall{name: "scrub_pool", input: `{"pool":"tank"}`}, "Scrubbing."}}
	claude := httptest.NewServer(script)
	defer claude.Close()
	acts := actions.New(context.Background(), openEvents(t), fakeJobs{ended: truenas.Job{State: "SUCCESS"}})
	helper := claudeHelperWithActions(t, claude.URL, openConversations(t), reg, nil, acts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _ := dialReady(ctx, t, helper, "?actions=1")
	if err := wsjson.Write(ctx, conn, inboundEvent{Type: "user_message", Content: "Scrub tank"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	for {
		frame := readFrame(ctx, t, conn)
		if frame["type"] == "approval_request" {
			_ = wsjson.Write(ctx, conn, inboundEvent{Type: "approval_response", ID: frame["id"].(string), Approved: true})
		}
		if frame["type"] == "done" {
			break
		}
	}
	if err := wsjson.Write(ctx, conn, inboundEvent{Type: "retry"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	frame := readFrame(ctx, t, conn)
	if frame["type"] != "error" || frame["code"] != codeRetryAfterActions {
		t.Fatalf("retry = %v, want the retry_after_actions error", frame)
	}
	if fake.runs() != 1 {
		t.Errorf("ran %d times", fake.runs())
	}
}
