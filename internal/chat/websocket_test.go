package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/donnie123421/zephyr-helper/internal/conversations"
	"github.com/donnie123421/zephyr-helper/internal/llm"
	"github.com/donnie123421/zephyr-helper/internal/ollama"
	"github.com/donnie123421/zephyr-helper/internal/tools"
)

// claudeScript answers /v1/messages in order: a reply text streams back,
// a status code fails the request, and slowReply streams in parts.
type claudeScript struct {
	mu     sync.Mutex
	steps  []any    // string (reply text), int (HTTP status) or slowReply
	bodies [][]byte // what each request sent
}

// slowReply streams its parts gap apart, for stopping mid-answer.
type slowReply struct {
	parts []string
	gap   time.Duration
}

func (c *claudeScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, body)
	step := c.steps[0]
	c.steps = c.steps[1:]
	c.mu.Unlock()

	if status, ok := step.(int); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		return
	}
	if slow, ok := step.(slowReply); ok {
		w.Header().Set("Content-Type", "text/event-stream")
		flush := func(event string) {
			var probe struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(event), &probe)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", probe.Type, event)
			w.(http.Flusher).Flush()
		}
		flush(`{"type":"message_start","message":{"id":"msg_s","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":50,"output_tokens":1}}}`)
		flush(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		for _, part := range slow.parts {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(slow.gap):
			}
			flush(fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, part))
		}
		flush(`{"type":"content_block_stop","index":0}`)
		flush(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":9}}`)
		flush(`{"type":"message_stop"}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":50,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, step.(string)),
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":7}}`,
		`{"type":"message_stop"}`,
	}
	for _, e := range events {
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &probe)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", probe.Type, e)
	}
}

// lastMessages is the conversation Claude was most recently sent, as
// "role: text" lines.
func (c *claudeScript) lastMessages(t *testing.T) []string {
	t.Helper()
	c.mu.Lock()
	body := c.bodies[len(c.bodies)-1]
	c.mu.Unlock()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	var out []string
	for _, m := range req.Messages {
		for _, block := range m.Content {
			if block.Type == "text" {
				out = append(out, m.Role+": "+block.Text)
			}
		}
	}
	return out
}

// claudeHelper serves the chat on Claude at claudeURL, saving chats in
// saved when it isn't nil.
func claudeHelper(t *testing.T, claudeURL string, saved *conversations.Store) *httptest.Server {
	t.Helper()
	store := llm.NewConfigStore(filepath.Join(t.TempDir(), "llm.json"))
	if err := store.Save(llm.Config{
		Provider:  llm.ProviderAnthropic,
		Anthropic: llm.ProviderConfig{APIKey: "sk-ant-test", BaseURL: claudeURL},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	sw := llm.NewSwitch(store, llm.NewOllama(ollama.NewClient("http://127.0.0.1:1", "unused")))
	helper := httptest.NewServer(NewHandler(sw, tools.NewWith(), saved))
	t.Cleanup(helper.Close)
	return helper
}

func openConversations(t *testing.T) *conversations.Store {
	t.Helper()
	store, err := conversations.Open(filepath.Join(t.TempDir(), "conversations.db"))
	if err != nil {
		t.Fatalf("open conversations: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func dial(ctx context.Context, t *testing.T, helper *httptest.Server, query string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(helper.URL, "http")+query, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	// Every chat opens by saying what the helper can do.
	if ready := readFrame(ctx, t, conn); ready["type"] != "ready" || fmt.Sprint(ready["features"]) != "[stop retry]" {
		t.Fatalf("first frame = %v, want ready with stop and retry", ready)
	}
	return conn
}

func readFrame(ctx context.Context, t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	var frame map[string]any
	if err := wsjson.Read(ctx, conn, &frame); err != nil {
		t.Fatalf("read: %v", err)
	}
	return frame
}

// ask sends a message and collects frames until the turn ends.
func ask(ctx context.Context, t *testing.T, conn *websocket.Conn, text string) []map[string]any {
	t.Helper()
	if err := wsjson.Write(ctx, conn, inboundEvent{Type: "user_message", Content: text}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got []map[string]any
	for {
		frame := readFrame(ctx, t, conn)
		got = append(got, frame)
		if frame["type"] == "done" || frame["type"] == "error" {
			return got
		}
	}
}

func TestChatOverWebSocketWithClaude(t *testing.T) {
	claude := httptest.NewServer(&claudeScript{steps: []any{"Hi there.", http.StatusUnauthorized, "Back again."}})
	defer claude.Close()
	helper := claudeHelper(t, claude.URL, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := dial(ctx, t, helper, "")
	ask := func(text string) []map[string]any { return ask(ctx, t, conn, text) }

	first := ask("hello")
	if types := frameTypes(first); fmt.Sprint(types) != "[delta usage done]" {
		t.Fatalf("first turn frames = %v", first)
	}
	if first[0]["content"] != "Hi there." || first[1]["input_tokens"] != float64(50) || first[1]["model"] != "claude-opus-5" {
		t.Errorf("first turn = %v", first)
	}

	second := ask("again")
	last := second[len(second)-1]
	if last["type"] != "error" || !strings.Contains(fmt.Sprint(last["message"]), "rejected the API key") {
		t.Fatalf("second turn = %v, want the rejected-key error", second)
	}

	// The chat stays open after an error.
	third := ask("still there?")
	if third[0]["content"] != "Back again." || third[len(third)-1]["type"] != "done" {
		t.Fatalf("third turn = %v", third)
	}
}

func frameTypes(frames []map[string]any) []string {
	var out []string
	for _, f := range frames {
		out = append(out, fmt.Sprint(f["type"]))
	}
	return out
}

func TestChatsAreSavedAndReopened(t *testing.T) {
	script := &claudeScript{steps: []any{"tank is healthy.", "Still healthy."}}
	claude := httptest.NewServer(script)
	defer claude.Close()
	saved := openConversations(t)
	helper := claudeHelper(t, claude.URL, saved)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dial(ctx, t, helper, "")
	first := ask(ctx, t, conn, "How is tank?")
	if types := frameTypes(first); fmt.Sprint(types) != "[conversation delta usage done]" {
		t.Fatalf("first turn frames = %v", first)
	}
	id, _ := first[0]["id"].(string)
	if id == "" || first[0]["title"] != "How is tank?" {
		t.Fatalf("conversation frame = %v", first[0])
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	conv, err := saved.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(conv.Messages) != 2 || conv.Messages[1].Content != "tank is healthy." ||
		conv.InputTokens != 50 || conv.OutputTokens != 7 || conv.Provider != llm.ProviderAnthropic {
		t.Fatalf("saved = %+v", conv)
	}

	// Reopening it carries the chat on.
	again := dial(ctx, t, helper, "?conversation="+id)
	if hello := readFrame(ctx, t, again); hello["type"] != "conversation" || hello["id"] != id {
		t.Fatalf("reopen frame = %v", hello)
	}
	second := ask(ctx, t, again, "And now?")
	if types := frameTypes(second); fmt.Sprint(types) != "[delta usage done]" {
		t.Fatalf("second turn frames = %v", second)
	}
	want := []string{"user: How is tank?", "assistant: tank is healthy.", "user: And now?"}
	if got := script.lastMessages(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Claude was sent %q, want %q", got, want)
	}
	conv, _ = saved.Get(ctx, id)
	if len(conv.Messages) != 4 || conv.InputTokens != 100 {
		t.Errorf("after reopening, saved = %+v", conv)
	}
}

func TestContextFromTheAppIsHiddenAndKeepsItsTitle(t *testing.T) {
	script := &claudeScript{steps: []any{"Remove root's key.", "It won't."}}
	claude := httptest.NewServer(script)
	defer claude.Close()
	saved := openConversations(t)
	helper := claudeHelper(t, claude.URL, saved)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	query := url.Values{"title": {"How to Fix: SSH Root Login"}, "context": {"1"}}
	conn := dial(ctx, t, helper, "?"+query.Encode())
	frames := ask(ctx, t, conn, "Zephyr's security audit flagged this on the user's NAS…")
	// Saved once it has an answer.
	if types := frameTypes(frames); fmt.Sprint(types) != "[delta usage conversation done]" {
		t.Fatalf("frames = %v", frames)
	}
	event := frames[2]
	if event["title"] != "How to Fix: SSH Root Login" {
		t.Fatalf("conversation frame = %v", event)
	}
	id := event["id"].(string)
	conv, _ := saved.Get(ctx, id)
	if len(conv.Messages) != 2 || !conv.Messages[0].Hidden || conv.Messages[1].Hidden || conv.MessageCount != 1 {
		t.Errorf("saved = %+v", conv)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	// Reopened, the model still gets the hidden context.
	again := dial(ctx, t, helper, "?conversation="+id)
	readFrame(ctx, t, again)
	ask(ctx, t, again, "Will that lock me out?")
	want := []string{"user: Zephyr's security audit flagged this on the user's NAS…",
		"assistant: Remove root's key.", "user: Will that lock me out?"}
	if got := script.lastMessages(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Claude was sent %q, want %q", got, want)
	}
}

func TestAnExplanationClosedBeforeItsAnswerIsNotSaved(t *testing.T) {
	claude := httptest.NewServer(&claudeScript{steps: []any{http.StatusUnauthorized}})
	defer claude.Close()
	saved := openConversations(t)
	helper := claudeHelper(t, claude.URL, saved)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	query := url.Values{"title": {"Explain Alert: tank is DEGRADED"}, "context": {"1"}}
	frames := ask(ctx, t, dial(ctx, t, helper, "?"+query.Encode()), "An alert fired on the NAS…")
	if types := frameTypes(frames); fmt.Sprint(types) != "[error]" {
		t.Fatalf("frames = %v", frames)
	}
	if list, _ := saved.List(ctx); len(list) != 0 {
		t.Errorf("saved %+v, want nothing", list)
	}
}

func TestABlankReplyIsNotSaved(t *testing.T) {
	claude := httptest.NewServer(&claudeScript{steps: []any{"\n\n"}})
	defer claude.Close()
	saved := openConversations(t)
	helper := claudeHelper(t, claude.URL, saved)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	frames := ask(ctx, t, dial(ctx, t, helper, ""), "hello")
	conv, _ := saved.Get(ctx, frames[0]["id"].(string))
	if len(conv.Messages) != 1 || conv.Messages[0].Role != conversations.RoleUser {
		t.Errorf("saved = %+v, want just the question", conv.Messages)
	}
}

func TestAChatDeletedWhileOpenStaysDeleted(t *testing.T) {
	claude := httptest.NewServer(&claudeScript{steps: []any{"First.", "Second."}})
	defer claude.Close()
	saved := openConversations(t)
	helper := claudeHelper(t, claude.URL, saved)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dial(ctx, t, helper, "")
	first := ask(ctx, t, conn, "hello")
	id := first[0]["id"].(string)
	if err := saved.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	second := ask(ctx, t, conn, "still there?")
	if second[len(second)-1]["type"] != "done" {
		t.Fatalf("the chat should carry on: %v", second)
	}
	if list, _ := saved.List(ctx); len(list) != 0 {
		t.Errorf("the deleted chat came back: %+v", list)
	}
}

func TestReopeningWithoutAStoreSaysSo(t *testing.T) {
	claude := httptest.NewServer(&claudeScript{})
	defer claude.Close()
	helper := claudeHelper(t, claude.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	frame := readFrame(ctx, t, dial(ctx, t, helper, "?conversation=abc"))
	if frame["type"] != "error" || frame["code"] != codeConversationsUnavailable {
		t.Fatalf("frame = %v", frame)
	}
}

func TestReplayableTidiesTheTranscript(t *testing.T) {
	u := func(s string) conversations.Message {
		return conversations.Message{Role: conversations.RoleUser, Content: s}
	}
	a := func(s string) conversations.Message {
		return conversations.Message{Role: conversations.RoleAssistant, Content: s}
	}
	got := replayable([]conversations.Message{
		a("orphaned reply"), u("first"), u("retry after a failed turn"), a("\n\n"), a("answer"), u("  "), u("next"),
	})
	var lines []string
	for _, m := range got {
		lines = append(lines, m.Role+": "+m.Content)
	}
	want := []string{"user: first\n\nretry after a failed turn", "assistant: answer", "user: next"}
	if fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Errorf("replayable = %q, want %q", lines, want)
	}
}

func TestAFailedTurnKeepsTheQuestionButNoReply(t *testing.T) {
	claude := httptest.NewServer(&claudeScript{steps: []any{http.StatusUnauthorized}})
	defer claude.Close()
	saved := openConversations(t)
	helper := claudeHelper(t, claude.URL, saved)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	frames := ask(ctx, t, dial(ctx, t, helper, ""), "hello")
	if types := frameTypes(frames); fmt.Sprint(types) != "[conversation error]" {
		t.Fatalf("frames = %v", frames)
	}
	conv, _ := saved.Get(ctx, frames[0]["id"].(string))
	if len(conv.Messages) != 1 || conv.Messages[0].Role != conversations.RoleUser {
		t.Errorf("saved = %+v", conv.Messages)
	}
}

func TestReopeningADeletedConversation(t *testing.T) {
	claude := httptest.NewServer(&claudeScript{})
	defer claude.Close()
	helper := claudeHelper(t, claude.URL, openConversations(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dial(ctx, t, helper, "?conversation=gone")
	if frame := readFrame(ctx, t, conn); frame["type"] != "error" || frame["code"] != codeConversationNotFound ||
		frame["message"] != "This conversation has been deleted." {
		t.Fatalf("frame = %v", frame)
	}
	var next map[string]any
	if err := wsjson.Read(ctx, conn, &next); err == nil {
		t.Errorf("the connection should close, got %v", next)
	}
}

func TestStopKeepsWhatArrived(t *testing.T) {
	story := slowReply{parts: []string{"Once ", "upon ", "a ", "time ", "there ", "was ", "a ", "pool."}, gap: 150 * time.Millisecond}
	script := &claudeScript{steps: []any{story, "It was tank."}}
	claude := httptest.NewServer(script)
	defer claude.Close()
	saved := openConversations(t)
	helper := claudeHelper(t, claude.URL, saved)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dial(ctx, t, helper, "")
	if err := wsjson.Write(ctx, conn, inboundEvent{Type: "user_message", Content: "Tell me a story"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var id, shown string
	stopped := false
	for {
		frame := readFrame(ctx, t, conn)
		switch frame["type"] {
		case "conversation":
			id = frame["id"].(string)
		case "delta":
			shown += frame["content"].(string)
			if !stopped {
				stopped = true
				if err := wsjson.Write(ctx, conn, inboundEvent{Type: "stop"}); err != nil {
					t.Fatalf("stop: %v", err)
				}
			}
		}
		if frame["type"] == "done" {
			if frame["stopped"] != true {
				t.Fatalf("done frame = %v, want stopped", frame)
			}
			break
		}
		if frame["type"] == "error" {
			t.Fatalf("error frame: %v", frame)
		}
	}
	if shown == "" || strings.Contains(shown, "pool.") {
		t.Fatalf("shown %q: the stop should have cut the story short", shown)
	}
	conv, _ := saved.Get(ctx, id)
	if len(conv.Messages) != 2 || conv.Messages[1].Content != shown {
		t.Fatalf("saved = %+v, want the question and %q", conv.Messages, shown)
	}

	// The chat carries on, and the model knows what it had said.
	next := ask(ctx, t, conn, "Which pool?")
	if next[len(next)-1]["type"] != "done" {
		t.Fatalf("next turn = %v", next)
	}
	want := []string{"user: Tell me a story", "assistant: " + strings.TrimSpace(shown), "user: Which pool?"}
	if got := script.lastMessages(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Claude was sent %q, want %q", got, want)
	}
}

func TestRetryWritesTheLastAnswerAgain(t *testing.T) {
	script := &claudeScript{steps: []any{"Hi there.", "Hi again."}}
	claude := httptest.NewServer(script)
	defer claude.Close()
	saved := openConversations(t)
	helper := claudeHelper(t, claude.URL, saved)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dial(ctx, t, helper, "")
	first := ask(ctx, t, conn, "hello")
	id := first[0]["id"].(string)

	again := retry(ctx, t, conn)
	if types := frameTypes(again); fmt.Sprint(types) != "[delta usage done]" || again[0]["content"] != "Hi again." {
		t.Fatalf("retry frames = %v", again)
	}
	// The model wasn't shown the answer it's replacing.
	if got := script.lastMessages(t); fmt.Sprint(got) != "[user: hello]" {
		t.Errorf("Claude was sent %q", got)
	}
	conv, _ := saved.Get(ctx, id)
	if len(conv.Messages) != 2 || conv.Messages[1].Content != "Hi again." {
		t.Errorf("saved = %+v", conv.Messages)
	}
}

func TestRetryAfterAFailedAnswer(t *testing.T) {
	claude := httptest.NewServer(&claudeScript{steps: []any{http.StatusUnauthorized, "Recovered."}})
	defer claude.Close()
	saved := openConversations(t)
	helper := claudeHelper(t, claude.URL, saved)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dial(ctx, t, helper, "")
	failed := ask(ctx, t, conn, "hello")
	id := failed[0]["id"].(string)
	if failed[len(failed)-1]["type"] != "error" {
		t.Fatalf("first turn = %v, want an error", failed)
	}
	again := retry(ctx, t, conn)
	if again[len(again)-1]["type"] != "done" {
		t.Fatalf("retry = %v", again)
	}
	conv, _ := saved.Get(ctx, id)
	if len(conv.Messages) != 2 || conv.Messages[0].Content != "hello" || conv.Messages[1].Content != "Recovered." {
		t.Errorf("saved = %+v", conv.Messages)
	}
}

func TestRetryWithNothingToAnswer(t *testing.T) {
	claude := httptest.NewServer(&claudeScript{})
	defer claude.Close()
	helper := claudeHelper(t, claude.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	frames := retry(ctx, t, dial(ctx, t, helper, ""))
	if frames[0]["type"] != "error" || frames[0]["code"] != codeNothingToRetry {
		t.Fatalf("frames = %v", frames)
	}
}

// retry asks for the last answer again and collects frames until the turn ends.
func retry(ctx context.Context, t *testing.T, conn *websocket.Conn) []map[string]any {
	t.Helper()
	if err := wsjson.Write(ctx, conn, inboundEvent{Type: "retry"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got []map[string]any
	for {
		frame := readFrame(ctx, t, conn)
		got = append(got, frame)
		if frame["type"] == "done" || frame["type"] == "error" {
			return got
		}
	}
}
