package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/donnie123421/zephyr-helper/internal/actions"
	"github.com/donnie123421/zephyr-helper/internal/conversations"
	"github.com/donnie123421/zephyr-helper/internal/llm"
	"github.com/donnie123421/zephyr-helper/internal/tools"
)

// maxActionsPerReply caps the cards one reply can put in front of the user.
const maxActionsPerReply = 5

// approvalTimeout is how long a card waits for an answer. No answer counts
// as declined.
const approvalTimeout = 5 * time.Minute

// actionTimeout bounds an approved action's run. Jobs return their id
// straight away; the longest waits are the jobs an action sees through,
// such as a VM shutting down, which TrueNAS gives up to five minutes.
const actionTimeout = 6 * time.Minute

// systemPromptWithActions is added in chats with actions on.
const systemPromptWithActions = ` You can also make changes with the action tools. Each one shows the user a card, ` +
	`written by the tool, and runs only if they approve it. Propose an action only when the user asks for it or agrees ` +
	`to your suggestion; the card is how they approve, so don't ask for permission in text first. Never say an action ` +
	`happened unless its result says so. If the user declines, don't propose it again unless they ask. At most ` +
	`5 actions per reply. You can't update TrueNAS itself, reboot or shut down, delete anything, roll back ` +
	`snapshots, change configuration (shares, users, permissions, datasets, networking) or replace disks. For ` +
	`those, tell the user where to do it in the Zephyr app: TrueNAS updates, and rebooting or shutting down, ` +
	`are in More → System Admin; snapshots in More → Snapshots; shares, datasets and disk replacement in the ` +
	`Storage tab; accounts in More → Users & Groups; networking in More → Network. ` +
	`You can also book these for later with schedule_action, set up automatic app updates with ` +
	`create_update_rule, and set up regular check-ins with create_checkin; each is approved on a card too. ` +
	`Times are the NAS's local time; if the user doesn't say when, ask rather than guess.`

// What became of a card, in action_result frames and saved chats.
const (
	actionDone      = "done"
	actionStarted   = "started" // TrueNAS carries on; Events has the rest
	actionFailed    = "failed"
	actionDeclined  = "declined"
	actionExpired   = "expired"   // no answer in time
	actionCancelled = "cancelled" // the reply was stopped, or the app left, first
)

// approvalRequestEvent puts a card in front of the user.
type approvalRequestEvent struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Tool      string    `json:"tool"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail"`
	Risk      string    `json:"risk"`
	ExpiresAt time.Time `json:"expires_at"`
}

// actionResultEvent says what became of a card.
type actionResultEvent struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// approvals routes the app's answers to the cards waiting for them. The
// reader goroutine delivers answers and stops, in the order the app sent
// them; the turn waits.
type approvals struct {
	mu      sync.Mutex
	waiting map[string]chan<- answer
	// seq numbers answers and stops in the order the reader saw them.
	seq uint64
	// stopSeq is the last stop's number; 0 for none.
	stopSeq uint64
}

type answer struct {
	id       string
	approved bool
	// seq and at place the answer against a stop and the cards'
	// deadline: an approved action can hold up the loop for minutes, and
	// answers queue meanwhile, so they're decided by when they came.
	seq uint64
	at  time.Time
}

func newApprovals() *approvals {
	return &approvals{waiting: map[string]chan<- answer{}}
}

// expect routes answers to ids into ch, which must have room for them all.
func (a *approvals) expect(ids []string, ch chan<- answer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range ids {
		a.waiting[id] = ch
	}
}

// drop stops routing answers to ids.
func (a *approvals) drop(ids []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range ids {
		delete(a.waiting, id)
	}
}

// deliver hands the app's answer to the card waiting for it. An answer to
// a card that isn't waiting (it expired, or never existed) is dropped.
func (a *approvals) deliver(id string, approved bool) {
	a.mu.Lock()
	a.seq++
	seq := a.seq
	ch, ok := a.waiting[id]
	delete(a.waiting, id)
	a.mu.Unlock()
	if !ok {
		slog.Info("chat: an answer arrived for a card that isn't waiting", "id", id)
		return
	}
	ch <- answer{id: id, approved: approved, seq: seq, at: time.Now()}
}

// stop notes the app stopping the reply, in order with its answers: an
// answer after it counts for nothing. The reader calls it before it
// cancels the turn.
func (a *approvals) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	a.stopSeq = a.seq
}

// mark is where the order has got to, and the last stop.
func (a *approvals) mark() (seq, stop uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seq, a.stopSeq
}

// turnActions runs one reply's actions past the user.
type turnActions struct {
	tools     *tools.Registry
	approvals *approvals
	log       *actions.Log
	// proposed counts the cards this reply has shown.
	proposed int
	// timeout is how long cards wait; approvalTimeout outside tests.
	timeout time.Duration
}

// proposal is an action the model asked for.
type proposal struct {
	index  int // in the step's tool calls
	call   llm.ToolCall
	action *tools.Action
}

// pendingCard is a card waiting for the user.
type pendingCard struct {
	proposal
	id      string
	card    tools.Card
	settled bool
}

// run puts the step's actions to the user, one card each, and runs the
// ones they approve as the answers come in. Every proposal gets its result
// in results: what the action did, or why it didn't run.
func (t *turnActions) run(ctx context.Context, send func(any) error, onStatus func(string), props []proposal, results []llm.ToolResult) {
	var cards []*pendingCard
	for _, p := range props {
		if t.proposed >= maxActionsPerReply {
			results[p.index] = errorResult(p.call, "At most 5 actions can be proposed in one reply. Ask the user which of the rest they want.")
			continue
		}
		card, err := p.action.Describe(ctx, p.call.Arguments)
		if err != nil {
			results[p.index] = errorResult(p.call, err.Error())
			continue
		}
		t.proposed++
		cards = append(cards, &pendingCard{proposal: p, id: newCardID(), card: card})
	}
	if len(cards) == 0 {
		return
	}

	ids := make([]string, len(cards))
	for i, c := range cards {
		ids[i] = c.id
	}
	answers := make(chan answer, len(cards))
	t.approvals.expect(ids, answers)
	defer t.approvals.drop(ids)

	deadline := time.Now().Add(t.timeout)
	for _, c := range cards {
		_ = send(approvalRequestEvent{
			Type: "approval_request", ID: c.id, Tool: c.call.Name,
			Title: c.card.Title, Detail: c.card.Detail, Risk: string(c.card.Risk),
			ExpiresAt: deadline.UTC().Truncate(time.Second),
		})
	}

	// An answer counts only if it came before any stop of this reply and
	// before the deadline, whenever the loop gets to it. (A stop from a
	// disconnect needs no ordering: no answers come after it.)
	base, _ := t.approvals.mark()
	open := len(cards)
	handle := func(a answer) {
		c := findCard(cards, a.id)
		if c == nil || c.settled {
			return
		}
		c.settled = true
		open--
		_, stop := t.approvals.mark()
		switch {
		case stop > base && a.seq > stop:
			t.settle(send, c, actionCancelled, "The reply was stopped before the user answered, so nothing was done.", results)
		case a.at.After(deadline):
			t.settle(send, c, actionExpired, "The user didn't answer within 5 minutes, so nothing was done.", results)
		case a.approved:
			t.execute(ctx, send, onStatus, c, results)
		default:
			t.settle(send, c, actionDeclined, "The user declined, so nothing was done. Don't propose it again unless they ask.", results)
		}
	}
	// drain handles the answers that arrived before a stop or the deadline
	// was seen, so they're decided by when they came, not by which the
	// loop happened to pick first.
	drain := func() {
		for {
			select {
			case a := <-answers:
				handle(a)
			default:
				return
			}
		}
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for open > 0 {
		select {
		case a := <-answers:
			handle(a)
		case <-timer.C:
			drain()
			t.settleRest(send, cards, actionExpired, "The user didn't answer within 5 minutes, so nothing was done.", results)
			open = 0
		case <-ctx.Done():
			drain()
			t.settleRest(send, cards, actionCancelled, "The reply was stopped before the user answered, so nothing was done.", results)
			open = 0
		}
	}
}

// execute runs an approved action. It isn't cut short by a stop or the app
// leaving: once approved, the call goes through and its outcome is recorded.
func (t *turnActions) execute(ctx context.Context, send func(any) error, onStatus func(string), c *pendingCard, results []llm.ToolResult) {
	onStatus(t.tools.StatusLine(c.call.Name))
	// What runs is what the card said, defaults included.
	args := c.call.Arguments
	if len(c.card.Args) > 0 {
		args = c.card.Args
	}
	slog.Info("chat: running an approved action", "name", c.call.Name, "args", string(args))
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), actionTimeout)
	outcome, err := c.action.Run(runCtx, args)
	cancel()

	if t.log != nil {
		t.log.Record(ctx, actions.Entry{
			ID: c.id, Tool: c.call.Name, Arguments: args,
			Card: c.card, Outcome: outcome, Err: err,
		})
	}

	switch {
	case err != nil:
		slog.Warn("chat: an approved action failed", "name", c.call.Name, "err", err)
		// The card already says what it was; its footer says why not.
		t.finish(send, c, actionFailed, upperFirst(err.Error()), tools.ToolError(err.Error()), true, results)
	case outcome.Job != nil:
		t.finish(send, c, actionStarted, outcome.Message, statusResult(actionStarted,
			outcome.Message+" TrueNAS carries on in the background, and how it ends will appear in Events, on the app's Assistant tab."), false, results)
	default:
		t.finish(send, c, actionDone, outcome.Message, statusResult(actionDone, outcome.Message), false, results)
	}
}

// settle ends a card that didn't run.
func (t *turnActions) settle(send func(any) error, c *pendingCard, status, forModel string, results []llm.ToolResult) {
	t.finish(send, c, status, "", statusResult(status, forModel), false, results)
}

func (t *turnActions) settleRest(send func(any) error, cards []*pendingCard, status, forModel string, results []llm.ToolResult) {
	for _, c := range cards {
		if !c.settled {
			c.settled = true
			t.settle(send, c, status, forModel, results)
		}
	}
}

// finish tells the app what became of a card, keeps it with the chat, and
// gives the model its result.
func (t *turnActions) finish(send func(any) error, c *pendingCard, status, message, content string, failed bool, results []llm.ToolResult) {
	_ = send(actionResultEvent{Type: "action_result", ID: c.id, Status: status, Message: message})
	use := conversations.NewToolUse(c.call.Name, c.call.Arguments, content, failed, time.Now())
	use.Action = &conversations.ActionRecord{
		ID: c.id, Title: c.card.Title, Detail: c.card.Detail, Risk: string(c.card.Risk),
		Status: status, Message: message,
	}
	_ = send(toolUsedEvent{Type: "tool_used", ToolUse: use})
	results[c.index] = llm.ToolResult{CallID: c.call.ID, Content: content, IsError: failed}
}

func upperFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

func findCard(cards []*pendingCard, id string) *pendingCard {
	for _, c := range cards {
		if c.id == id {
			return c
		}
	}
	return nil
}

func errorResult(call llm.ToolCall, message string) llm.ToolResult {
	return llm.ToolResult{CallID: call.ID, Content: tools.ToolError(message), IsError: true}
}

func statusResult(status, message string) string {
	data, _ := json.Marshal(map[string]string{"status": status, "message": message})
	return string(data)
}

// newCardID names a card: random, so an answer can't be guessed onto
// another chat's card.
func newCardID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
