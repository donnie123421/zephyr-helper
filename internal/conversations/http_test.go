package conversations

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, store *Store) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	(&Handler{Store: store}).RegisterMux(mux, func(h http.Handler) http.Handler { return h })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("%s %s: body %q isn't JSON", method, url, data)
		}
	}
	return resp.StatusCode, out
}

func TestConversationEndpoints(t *testing.T) {
	store := openStore(t)
	srv := serve(t, store)
	ctx := context.Background()

	c, _ := store.Start(ctx, "How to Fix: SSH Root Login")
	mustAppend(t, store, c.ID, Message{Role: RoleUser, Content: "long hidden context", Hidden: true})
	mustAppend(t, store, c.ID, Message{Role: RoleAssistant, Content: "1. Remove root's key.", Tools: []string{"list_users"}})
	_ = store.RecordTurn(ctx, c.ID, "anthropic", "claude-opus-5", 900, 120)

	status, list := call(t, "GET", srv.URL+"/conversations", "")
	items, _ := list["conversations"].([]any)
	if status != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", status, list)
	}
	item := items[0].(map[string]any)
	if item["id"] != c.ID || item["message_count"] != float64(1) || item["model"] != "claude-opus-5" {
		t.Errorf("list item = %v", item)
	}
	// Whole seconds, which the app's decoder expects.
	if at, _ := item["updated_at"].(string); strings.Contains(at, ".") || !strings.HasSuffix(at, "Z") {
		t.Errorf("updated_at = %q", at)
	}

	status, got := call(t, "GET", srv.URL+"/conversations/"+c.ID, "")
	messages, _ := got["messages"].([]any)
	if status != 200 || len(messages) != 1 {
		t.Fatalf("get = %d %v, want only the visible message", status, got)
	}
	reply := messages[0].(map[string]any)
	if reply["role"] != "assistant" || reply["content"] != "1. Remove root's key." || len(reply["tools"].([]any)) != 1 {
		t.Errorf("message = %v", reply)
	}
	if got["input_tokens"] != float64(900) || got["output_tokens"] != float64(120) || got["title"] != "How to Fix: SSH Root Login" {
		t.Errorf("conversation = %v", got)
	}

	status, renamed := call(t, "PATCH", srv.URL+"/conversations/"+c.ID, `{"title":"Root SSH"}`)
	if status != 200 || renamed["title"] != "Root SSH" {
		t.Errorf("rename = %d %v", status, renamed)
	}
	if status, _ := call(t, "PATCH", srv.URL+"/conversations/"+c.ID, `{"title":"  "}`); status != 400 {
		t.Errorf("empty rename = %d, want 400", status)
	}

	if status, _ := call(t, "DELETE", srv.URL+"/conversations/"+c.ID, ""); status != 204 {
		t.Errorf("delete = %d", status)
	}
	// A missing conversation is a JSON 404, unlike an unknown route.
	status, missing := call(t, "GET", srv.URL+"/conversations/"+c.ID, "")
	if status != 404 || missing["error"] == nil {
		t.Errorf("get deleted = %d %v", status, missing)
	}

	_, _ = store.Start(ctx, "one")
	_, _ = store.Start(ctx, "two")
	status, deleted := call(t, "DELETE", srv.URL+"/conversations", "")
	if status != 200 || deleted["deleted"] != float64(2) {
		t.Errorf("delete all = %d %v", status, deleted)
	}
}

func TestWithoutAStoreEveryRouteSaysUnavailable(t *testing.T) {
	srv := serve(t, nil)
	status, body := call(t, "GET", srv.URL+"/conversations", "")
	if status != http.StatusServiceUnavailable || body["error"] != Unavailable {
		t.Errorf("list = %d %v", status, body)
	}
	if status, _ := call(t, "DELETE", srv.URL+"/conversations/abc", ""); status != http.StatusServiceUnavailable {
		t.Errorf("delete = %d", status)
	}
}

func TestAnUnknownRouteIsAPlainNotFound(t *testing.T) {
	// What an older helper answers: the app reads it as "update the helper".
	srv := serve(t, openStore(t))
	resp, err := http.Get(srv.URL + "/conversations/abc/nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 404 || json.Valid(data) {
		t.Errorf("unknown route = %d %q", resp.StatusCode, data)
	}
}

func TestListTakesALimit(t *testing.T) {
	store := openStore(t)
	tick(store)
	srv := serve(t, store)
	for _, title := range []string{"one", "two", "three"} {
		_, _ = store.Start(context.Background(), title)
	}
	_, body := call(t, "GET", srv.URL+"/conversations?limit=2", "")
	items, _ := body["conversations"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["title"] != "three" {
		t.Errorf("limited list = %v", body)
	}
	if status, _ := call(t, "GET", srv.URL+"/conversations?limit=x", ""); status != 400 {
		t.Errorf("bad limit = %d, want 400", status)
	}
}
