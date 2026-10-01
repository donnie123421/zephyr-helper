package chat

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/donnie123421/zephyr-helper/internal/llm"
	"github.com/donnie123421/zephyr-helper/internal/ollama"
	"github.com/donnie123421/zephyr-helper/internal/tools"
)

// A check-in looks around with the read-only tools, and saves its briefing
// as a conversation whose prompt the app keeps out of sight.
func TestACheckInSavesItsBriefing(t *testing.T) {
	saved := openConversations(t)
	claude := &claudeScript{steps: []any{
		toolCall{name: "list_pools", input: `{}`},
		"**All is well.** tank is healthy. Nothing needs you this week.",
	}}
	server := httptest.NewServer(claude)
	t.Cleanup(server.Close)
	store := llm.NewConfigStore(filepath.Join(t.TempDir(), "llm.json"))
	if err := store.Save(llm.Config{Provider: llm.ProviderAnthropic, Anthropic: llm.ProviderConfig{APIKey: "sk-ant-test", BaseURL: server.URL}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	fake := &fakeAction{}
	reg := tools.NewWith(append(testRegistryTools(), fake.tool("scrub_pool"))...)
	h := NewHandler(llm.NewSwitch(store, llm.NewOllama(ollama.NewClient("http://127.0.0.1:1", "unused"))), reg, saved, nil, nil)

	result, err := h.CheckIn(context.Background(), "Check-in · Thu 1 Oct")
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if result.Summary != "All is well." {
		t.Errorf("summary = %q", result.Summary)
	}
	conv, err := saved.Get(context.Background(), result.ConversationID)
	if err != nil || conv.Title != "Check-in · Thu 1 Oct" || len(conv.Messages) != 2 {
		t.Fatalf("saved = %+v, %v", conv, err)
	}
	if !conv.Messages[0].Hidden || conv.Messages[1].Content != "**All is well.** tank is healthy. Nothing needs you this week." ||
		len(conv.Messages[1].Tools) != 1 || conv.Messages[1].Tools[0].Name != "list_pools" {
		t.Errorf("messages = %+v", conv.Messages)
	}
	// It only reads: no action was offered to the model.
	claude.mu.Lock()
	first := string(claude.bodies[0])
	claude.mu.Unlock()
	var req struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal([]byte(first), &req)
	for _, tool := range req.Tools {
		if tool.Name == "scrub_pool" {
			t.Error("a check-in was offered an action")
		}
	}
	if fake.runs() != 0 {
		t.Error("a check-in ran an action")
	}
}
