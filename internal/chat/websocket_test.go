package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/donnie123421/zephyr-helper/internal/llm"
	"github.com/donnie123421/zephyr-helper/internal/ollama"
	"github.com/donnie123421/zephyr-helper/internal/tools"
)

// claudeScript answers /v1/messages in order: a reply text streams back,
// a status code fails the request.
type claudeScript struct {
	mu    sync.Mutex
	steps []any // string (reply text) or int (HTTP status)
}

func (c *claudeScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	c.mu.Lock()
	step := c.steps[0]
	c.steps = c.steps[1:]
	c.mu.Unlock()

	if status, ok := step.(int); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
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

func TestChatOverWebSocketWithClaude(t *testing.T) {
	claude := httptest.NewServer(&claudeScript{steps: []any{"Hi there.", http.StatusUnauthorized, "Back again."}})
	defer claude.Close()

	store := llm.NewConfigStore(filepath.Join(t.TempDir(), "llm.json"))
	if err := store.Save(llm.Config{
		Provider:  llm.ProviderAnthropic,
		Anthropic: llm.ProviderConfig{APIKey: "sk-ant-test", BaseURL: claude.URL},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	sw := llm.NewSwitch(store, llm.NewOllama(ollama.NewClient("http://127.0.0.1:1", "unused")))
	helper := httptest.NewServer(NewHandler(sw, tools.NewWith()))
	defer helper.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(helper.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// ask sends a message and collects frames until the turn ends.
	ask := func(text string) []map[string]any {
		t.Helper()
		if err := wsjson.Write(ctx, conn, inboundEvent{Type: "user_message", Content: text}); err != nil {
			t.Fatalf("write: %v", err)
		}
		var got []map[string]any
		for {
			var frame map[string]any
			if err := wsjson.Read(ctx, conn, &frame); err != nil {
				t.Fatalf("read after %v: %v", got, err)
			}
			got = append(got, frame)
			if frame["type"] == "done" || frame["type"] == "error" {
				return got
			}
		}
	}

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
