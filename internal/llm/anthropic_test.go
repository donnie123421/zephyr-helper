package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeClaude answers /v1/messages with queued SSE streams and records each
// request body, so tests can check what the adapter sent.
type fakeClaude struct {
	mu       sync.Mutex
	replies  []string // SSE bodies, one per request
	status   int      // non-zero: fail every request with this status
	requests []map[string]any
	models   []string
}

func (f *fakeClaude) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		return
	}
	switch r.URL.Path {
	case "/v1/models":
		var data []string
		for _, id := range f.models {
			data = append(data, fmt.Sprintf(`{"type":"model","id":%q,"display_name":%q,"created_at":"2026-01-01T00:00:00Z","max_input_tokens":1000000,"max_tokens":128000,"capabilities":{}}`, id, "Name "+id))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[%s],"has_more":false,"first_id":null,"last_id":null}`, strings.Join(data, ","))
	case "/v1/messages":
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		f.requests = append(f.requests, req)
		if len(f.replies) == 0 {
			http.Error(w, "no reply queued", http.StatusInternalServerError)
			return
		}
		reply := f.replies[0]
		f.replies = f.replies[1:]
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reply)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeClaude) request(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &probe)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", probe.Type, e)
	}
	return b.String()
}

const messageStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":120,"output_tokens":1,"cache_creation_input_tokens":900,"cache_read_input_tokens":0}}}`

func textReply(stopReason string, texts ...string) string {
	events := []string{messageStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`}
	for _, t := range texts {
		events = append(events, fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, t))
	}
	events = append(events,
		`{"type":"content_block_stop","index":0}`,
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":42}}`, stopReason),
		`{"type":"message_stop"}`,
	)
	return sse(events...)
}

func toolReply() string {
	return sse(
		messageStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking. "}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"pool_status","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"pool_name\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"tank\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":30}}`,
		`{"type":"message_stop"}`,
	)
}

var testTools = []Tool{
	{Name: "pool_status", Description: "Pool detail", Parameters: json.RawMessage(`{"type":"object","properties":{"pool_name":{"type":"string"}},"additionalProperties":false}`)},
	{Name: "list_pools", Description: "List pools", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
}

func newFakeSession(t *testing.T, fake *fakeClaude) Session {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return NewAnthropic("sk-ant-test", "claude-opus-5", srv.URL).NewSession("You are Zephyr.", testTools)
}

func TestClaudeStreamsTextAndReportsUsage(t *testing.T) {
	fake := &fakeClaude{replies: []string{textReply("end_turn", "All ", "pools ", "healthy.")}}
	session := newFakeSession(t, fake)
	session.AddUserMessage("How are my pools?")

	var streamed []string
	result, err := session.Step(context.Background(), func(d string) error {
		streamed = append(streamed, d)
		return nil
	})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := strings.Join(streamed, ""); got != "All pools healthy." {
		t.Errorf("streamed %q", got)
	}
	if len(result.ToolCalls) != 0 {
		t.Errorf("tool calls = %+v, want none", result.ToolCalls)
	}
	want := Usage{InputTokens: 120, OutputTokens: 42, CacheWriteTokens: 900}
	if result.Usage != want {
		t.Errorf("usage = %+v, want %+v", result.Usage, want)
	}

	req := fake.request(0)
	if req["model"] != "claude-opus-5" {
		t.Errorf("model = %v", req["model"])
	}
	if _, ok := req["cache_control"]; !ok {
		t.Error("no top-level cache_control: the conversation wouldn't be cached")
	}
	system := req["system"].([]any)[0].(map[string]any)
	if system["text"] != "You are Zephyr." || system["cache_control"] == nil {
		t.Errorf("system block = %v, want the prompt with a cache breakpoint", system)
	}
}

func TestClaudeSendsStrictToolsInTheGivenOrder(t *testing.T) {
	fake := &fakeClaude{replies: []string{textReply("end_turn", "ok")}}
	session := newFakeSession(t, fake)
	session.AddUserMessage("hi")
	if _, err := session.Step(context.Background(), func(string) error { return nil }); err != nil {
		t.Fatalf("step: %v", err)
	}
	tools := fake.request(0)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v", tools)
	}
	for i, name := range []string{"pool_status", "list_pools"} {
		tool := tools[i].(map[string]any)
		if tool["name"] != name || tool["strict"] != true {
			t.Errorf("tool %d = %v, want %s with strict", i, tool, name)
		}
		schema := tool["input_schema"].(map[string]any)
		if schema["additionalProperties"] != false || schema["type"] != "object" {
			t.Errorf("tool %s schema = %v", name, schema)
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Errorf("tool %s has no properties object", name)
		}
	}
}

func TestClaudeToolRoundTrip(t *testing.T) {
	fake := &fakeClaude{replies: []string{toolReply(), textReply("end_turn", "tank is ONLINE.")}}
	session := newFakeSession(t, fake)
	session.AddUserMessage("Is tank ok?")

	first, err := session.Step(context.Background(), func(string) error { return nil })
	if err != nil {
		t.Fatalf("first step: %v", err)
	}
	if len(first.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", first.ToolCalls)
	}
	call := first.ToolCalls[0]
	if call.ID != "toolu_01" || call.Name != "pool_status" {
		t.Errorf("call = %+v", call)
	}
	var args map[string]string
	if err := json.Unmarshal(call.Arguments, &args); err != nil || args["pool_name"] != "tank" {
		t.Errorf("arguments = %s (%v)", call.Arguments, err)
	}

	session.AddToolResults([]ToolResult{{CallID: "toolu_01", Content: `{"error":"pool offline"}`, IsError: true}})
	if _, err := session.Step(context.Background(), func(string) error { return nil }); err != nil {
		t.Fatalf("second step: %v", err)
	}

	messages := fake.request(1)["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("second request has %d messages, want user, assistant, tool results", len(messages))
	}
	assistant := messages[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("message 1 role = %v", assistant["role"])
	}
	var sawToolUse bool
	for _, block := range assistant["content"].([]any) {
		if b := block.(map[string]any); b["type"] == "tool_use" && b["id"] == "toolu_01" {
			sawToolUse = true
		}
	}
	if !sawToolUse {
		t.Errorf("assistant turn wasn't replayed with its tool_use: %v", assistant["content"])
	}
	result := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_01" || result["is_error"] != true {
		t.Errorf("tool result = %v", result)
	}
}

func TestClaudeRefusalIsReportedAndNotReplayed(t *testing.T) {
	fake := &fakeClaude{replies: []string{textReply("refusal", "I can't"), textReply("end_turn", "Sure.")}}
	session := newFakeSession(t, fake)
	session.AddUserMessage("first")

	_, err := session.Step(context.Background(), func(string) error { return nil })
	var problem Problem
	if !errors.As(err, &problem) || !strings.Contains(string(problem), "declined") {
		t.Fatalf("err = %v, want a refusal problem", err)
	}

	session.AddUserMessage("second")
	if _, err := session.Step(context.Background(), func(string) error { return nil }); err != nil {
		t.Fatalf("step after refusal: %v", err)
	}
	for _, m := range fake.request(1)["messages"].([]any) {
		if m.(map[string]any)["role"] == "assistant" {
			t.Fatalf("the refused reply was replayed: %v", fake.request(1)["messages"])
		}
	}
}

func TestClaudeRejectedKeyIsExplained(t *testing.T) {
	fake := &fakeClaude{status: http.StatusUnauthorized}
	session := newFakeSession(t, fake)
	session.AddUserMessage("hi")
	_, err := session.Step(context.Background(), func(string) error { return nil })
	var problem Problem
	if !errors.As(err, &problem) || !strings.Contains(string(problem), "rejected the API key") {
		t.Fatalf("err = %v, want the rejected-key problem", err)
	}
}

func TestAnthropicModels(t *testing.T) {
	fake := &fakeClaude{models: []string{"claude-opus-5", "claude-sonnet-5"}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	models, err := AnthropicModels(context.Background(), "sk-ant-test", srv.URL)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "claude-opus-5" || models[1].DisplayName != "Name claude-sonnet-5" {
		t.Errorf("models = %+v", models)
	}

	fake.status = http.StatusUnauthorized
	if _, err := AnthropicModels(context.Background(), "bad", srv.URL); err == nil {
		t.Error("a rejected key should fail the listing")
	}
}
