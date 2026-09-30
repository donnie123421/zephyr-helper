package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/llm"
	"github.com/donnie123421/zephyr-helper/internal/tools"
)

// twoCards sets up a reply with two action cards: "slow" blocks in Run until
// release is closed, and "second" records that it ran.
type twoCards struct {
	acts     *turnActions
	props    []proposal
	started  chan struct{}
	release  chan struct{}
	ranB     atomic.Bool
	requests chan string
}

func newTwoCards(timeout time.Duration) *twoCards {
	tc := &twoCards{started: make(chan struct{}), release: make(chan struct{}), requests: make(chan string, 2)}
	slow := tools.Tool{
		Def: tools.Definition{Name: "slow", Parameters: json.RawMessage(`{}`)},
		Action: &tools.Action{
			Describe: func(context.Context, json.RawMessage) (tools.Card, error) {
				return tools.Card{Title: "Restart Plex", Risk: tools.RiskDisruptive}, nil
			},
			Run: func(context.Context, json.RawMessage) (tools.Outcome, error) {
				close(tc.started)
				<-tc.release
				return tools.Outcome{Message: "Restarted Plex."}, nil
			},
		},
	}
	second := tools.Tool{
		Def: tools.Definition{Name: "second", Parameters: json.RawMessage(`{}`)},
		Action: &tools.Action{
			Describe: func(context.Context, json.RawMessage) (tools.Card, error) {
				return tools.Card{Title: "Stop Jellyfin", Risk: tools.RiskDisruptive}, nil
			},
			Run: func(context.Context, json.RawMessage) (tools.Outcome, error) {
				tc.ranB.Store(true)
				return tools.Outcome{Message: "Stopped Jellyfin."}, nil
			},
		},
	}
	reg := tools.NewWith(slow, second)
	tc.acts = &turnActions{tools: reg, approvals: newApprovals(), timeout: timeout}
	tc.props = []proposal{
		{index: 0, call: llm.ToolCall{ID: "call_a", Name: "slow", Arguments: json.RawMessage(`{}`)}, action: reg.Action("slow")},
		{index: 1, call: llm.ToolCall{ID: "call_b", Name: "second", Arguments: json.RawMessage(`{}`)}, action: reg.Action("second")},
	}
	return tc
}

func (tc *twoCards) send(v any) error {
	if req, ok := v.(approvalRequestEvent); ok {
		tc.requests <- req.ID
	}
	return nil
}

// A stop counts as declining every card still waiting. Here the user stops
// the reply while card A's action runs, and only then approves card B.
func TestAnApprovalAfterAStopDoesntRun(t *testing.T) {
	const rounds = 40
	ran := 0
	for i := 0; i < rounds; i++ {
		tc := newTwoCards(time.Minute)
		ctx, stop := context.WithCancel(context.Background())
		results := make([]llm.ToolResult, 2)
		done := make(chan struct{})
		go func() {
			tc.acts.run(ctx, tc.send, func(string) {}, tc.props, results)
			close(done)
		}()
		idA, idB := <-tc.requests, <-tc.requests

		tc.acts.approvals.deliver(idA, true)
		<-tc.started
		// The user stops the reply, then approves card B; the reader notes
		// the stop before it cancels the turn, as ServeHTTP does.
		tc.acts.approvals.stop()
		stop()
		tc.acts.approvals.deliver(idB, true)
		close(tc.release)
		<-done

		if tc.ranB.Load() {
			ran++
			if !strings.Contains(results[1].Content, `"status":"done"`) {
				t.Fatalf("B ran but its result is %s", results[1].Content)
			}
		}
		stop()
	}
	if ran > 0 {
		t.Errorf("card B, approved after the stop, ran in %d of %d rounds", ran, rounds)
	}
}

// A card left unanswered past its expiry counts as declined. Here card B's
// approval arrives after it expired, while card A's action was running.
func TestAnApprovalAfterTheCardsExpiredDoesntRun(t *testing.T) {
	const rounds = 20
	ran := 0
	for i := 0; i < rounds; i++ {
		tc := newTwoCards(30 * time.Millisecond)
		results := make([]llm.ToolResult, 2)
		done := make(chan struct{})
		go func() {
			tc.acts.run(context.Background(), tc.send, func(string) {}, tc.props, results)
			close(done)
		}()
		idA, idB := <-tc.requests, <-tc.requests

		tc.acts.approvals.deliver(idA, true)
		<-tc.started
		time.Sleep(60 * time.Millisecond)    // B's card has expired...
		tc.acts.approvals.deliver(idB, true) // ...and is approved late
		close(tc.release)
		<-done

		if tc.ranB.Load() {
			ran++
		}
	}
	if ran > 0 {
		t.Errorf("card B, approved after it expired, ran in %d of %d rounds", ran, rounds)
	}
}

// Approvals are decided in the order they came: one sent before the stop
// runs, even though the action ahead of it was still running at the stop.
func TestAnApprovalBeforeAStopStillRuns(t *testing.T) {
	for i := 0; i < 20; i++ {
		tc := newTwoCards(time.Minute)
		ctx, stop := context.WithCancel(context.Background())
		results := make([]llm.ToolResult, 2)
		done := make(chan struct{})
		go func() {
			tc.acts.run(ctx, tc.send, func(string) {}, tc.props, results)
			close(done)
		}()
		idA, idB := <-tc.requests, <-tc.requests

		tc.acts.approvals.deliver(idA, true)
		<-tc.started
		tc.acts.approvals.deliver(idB, true) // approved while A runs...
		tc.acts.approvals.stop()             // ...then the reply is stopped
		stop()
		close(tc.release)
		<-done

		if !tc.ranB.Load() || !strings.Contains(results[1].Content, `"status":"done"`) {
			t.Fatalf("round %d: card B, approved before the stop, didn't run: %s", i, results[1].Content)
		}
	}
}

// A stop in an earlier reply doesn't void a later reply's answers.
func TestAnEarlierStopDoesntVoidALaterReply(t *testing.T) {
	tc := newTwoCards(time.Minute)
	tc.acts.approvals.stop()
	results := make([]llm.ToolResult, 2)
	done := make(chan struct{})
	close(tc.release)
	go func() {
		tc.acts.run(context.Background(), tc.send, func(string) {}, tc.props, results)
		close(done)
	}()
	idA, idB := <-tc.requests, <-tc.requests
	tc.acts.approvals.deliver(idA, true)
	tc.acts.approvals.deliver(idB, true)
	<-done
	if !tc.ranB.Load() {
		t.Fatalf("an earlier reply's stop voided this reply's approval: %s", results[1].Content)
	}
}
