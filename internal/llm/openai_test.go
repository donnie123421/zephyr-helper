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

// fakeOpenAI answers /v1/chat/completions with queued SSE streams and
// /v1/models with a fixed list, recording each chat request.
type fakeOpenAI struct {
	mu       sync.Mutex
	replies  []string
	status   int
	code     string
	requests []map[string]any
	models   []string
}

func (f *fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		fmt.Fprintf(w, `{"error":{"message":"nope","type":"invalid_request_error","param":null,"code":%q}}`, f.code)
		return
	}
	switch r.URL.Path {
	case "/v1/models":
		var data []string
		for i, id := range f.models {
			data = append(data, fmt.Sprintf(`{"id":%q,"object":"model","created":%d,"owned_by":"openai"}`, id, 1000+i))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[%s]}`, strings.Join(data, ","))
	case "/v1/chat/completions":
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		f.requests = append(f.requests, req)
		reply := f.replies[0]
		f.replies = f.replies[1:]
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reply)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeOpenAI) request(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func chunks(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		fmt.Fprintf(&b, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-6-astra\",%s}\n\n", p)
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

const usageChunk = `"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":20,"total_tokens":320,"prompt_tokens_details":{"cached_tokens":256}}`

func openAITextReply(finish string, texts ...string) string {
	parts := []string{`"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]`}
	for _, t := range texts {
		parts = append(parts, fmt.Sprintf(`"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]`, t))
	}
	parts = append(parts, fmt.Sprintf(`"choices":[{"index":0,"delta":{},"finish_reason":%q}]`, finish), usageChunk)
	return chunks(parts...)
}

func openAIToolReply() string {
	return chunks(
		`"choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"pool_status","arguments":""}}]},"finish_reason":null}]`,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pool_name\":"}}]},"finish_reason":null}]`,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"tank\"}"}}]},"finish_reason":null}]`,
		`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`,
		usageChunk,
	)
}

func newOpenAISession(t *testing.T, fake *fakeOpenAI, provider string) Session {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return NewOpenAI(provider, "sk-test", "gpt-6-astra", srv.URL).NewSession("You are Zephyr.", testTools)
}

func TestOpenAIStreamsTextAndReportsUsage(t *testing.T) {
	fake := &fakeOpenAI{replies: []string{openAITextReply("stop", "All ", "healthy.")}}
	session := newOpenAISession(t, fake, ProviderOpenAI)
	session.AddUserMessage("How are my pools?")

	var streamed []string
	result, err := session.Step(context.Background(), func(d string) error {
		streamed = append(streamed, d)
		return nil
	})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := strings.Join(streamed, ""); got != "All healthy." {
		t.Errorf("streamed %q", got)
	}
	want := Usage{InputTokens: 44, CacheReadTokens: 256, OutputTokens: 20}
	if result.Usage != want {
		t.Errorf("usage = %+v, want %+v", result.Usage, want)
	}

	req := fake.request(0)
	if req["model"] != "gpt-6-astra" || req["stream"] != true {
		t.Errorf("request = %v", req)
	}
	if opts, _ := req["stream_options"].(map[string]any); opts["include_usage"] != true {
		t.Error("usage wasn't requested")
	}
	messages := req["messages"].([]any)
	if first := messages[0].(map[string]any); first["role"] != "system" || first["content"] != "You are Zephyr." {
		t.Errorf("first message = %v", first)
	}
	tools := req["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "pool_status" {
		t.Errorf("tools = %v", tools)
	}
}

func TestOpenAIToolRoundTrip(t *testing.T) {
	fake := &fakeOpenAI{replies: []string{openAIToolReply(), openAITextReply("stop", "tank is ONLINE.")}}
	session := newOpenAISession(t, fake, ProviderOpenAI)
	session.AddUserMessage("Is tank ok?")

	first, err := session.Step(context.Background(), func(string) error { return nil })
	if err != nil {
		t.Fatalf("first step: %v", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].ID != "call_1" || first.ToolCalls[0].Name != "pool_status" {
		t.Fatalf("tool calls = %+v", first.ToolCalls)
	}
	var args map[string]string
	if err := json.Unmarshal(first.ToolCalls[0].Arguments, &args); err != nil || args["pool_name"] != "tank" {
		t.Errorf("arguments = %s (%v)", first.ToolCalls[0].Arguments, err)
	}

	session.AddToolResults([]ToolResult{{CallID: "call_1", Content: `{"status":"ONLINE"}`}})
	if _, err := session.Step(context.Background(), func(string) error { return nil }); err != nil {
		t.Fatalf("second step: %v", err)
	}

	messages := fake.request(1)["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("second request has %d messages, want system, user, assistant, tool", len(messages))
	}
	assistant := messages[2].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if assistant["role"] != "assistant" || len(calls) != 1 || calls[0].(map[string]any)["id"] != "call_1" {
		t.Errorf("assistant turn = %v", assistant)
	}
	tool := messages[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Errorf("tool message = %v", tool)
	}
}

func TestOpenAICutOffReplyIsReportedAndNotReplayed(t *testing.T) {
	fake := &fakeOpenAI{replies: []string{openAITextReply("length", "partial"), openAITextReply("stop", "ok")}}
	session := newOpenAISession(t, fake, ProviderOpenAI)
	session.AddUserMessage("first")
	_, err := session.Step(context.Background(), func(string) error { return nil })
	var problem Problem
	if !errors.As(err, &problem) || !strings.Contains(string(problem), "length limit") {
		t.Fatalf("err = %v", err)
	}
	session.AddUserMessage("second")
	if _, err := session.Step(context.Background(), func(string) error { return nil }); err != nil {
		t.Fatalf("step: %v", err)
	}
	for _, m := range fake.request(1)["messages"].([]any) {
		if m.(map[string]any)["role"] == "assistant" {
			t.Fatal("the cut-off reply was replayed")
		}
	}
}

func TestOpenAIErrorsAreExplained(t *testing.T) {
	cases := []struct {
		provider string
		status   int
		code     string
		want     string
	}{
		{ProviderOpenAI, http.StatusUnauthorized, "invalid_api_key", "OpenAI rejected the API key"},
		{ProviderOpenAI, http.StatusTooManyRequests, "insufficient_quota", "out of credit"},
		{ProviderOpenAI, http.StatusTooManyRequests, "rate_limit_exceeded", "rate limit"},
		{ProviderCustom, http.StatusUnauthorized, "", "The server rejected the API key"},
	}
	for _, tc := range cases {
		fake := &fakeOpenAI{status: tc.status, code: tc.code}
		session := newOpenAISession(t, fake, tc.provider)
		session.AddUserMessage("hi")
		_, err := session.Step(context.Background(), func(string) error { return nil })
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %d %s: err = %v, want %q", tc.provider, tc.status, tc.code, err, tc.want)
		}
	}
}

func TestOpenAIModelsKeepsChatModelsNewestFirst(t *testing.T) {
	fake := &fakeOpenAI{models: []string{"gpt-6-luna", "text-embedding-4", "gpt-6-astra", "gpt-6-realtime", "dall-e-4"}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	models, err := OpenAIModels(context.Background(), ProviderOpenAI, "sk-test", srv.URL)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if fmt.Sprint(ids) != "[gpt-6-astra gpt-6-luna]" {
		t.Errorf("models = %v, want the chat models newest first", ids)
	}

	custom, err := OpenAIModels(context.Background(), ProviderCustom, "", srv.URL)
	if err != nil || len(custom) != 5 {
		t.Errorf("a custom server's list should pass through whole: %v (%v)", custom, err)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://pc:1234":               "http://pc:1234/v1",
		"http://pc:1234/":              "http://pc:1234/v1",
		"http://pc:1234/v1":            "http://pc:1234/v1",
		"https://openrouter.ai/api/v1": "https://openrouter.ai/api/v1",
	} {
		if got := NormalizeBaseURL(in); got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}
