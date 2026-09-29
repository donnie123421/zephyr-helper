package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// at sets the store's clock.
func at(store *Store, when time.Time) { store.now = func() time.Time { return when } }

func TestTotalsAreByModelAndPeriod(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	august := time.Date(2026, 8, 31, 23, 0, 0, 0, time.UTC)
	september := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)

	at(store, august)
	mustRecord(t, store, "anthropic", "claude-opus-5", Tokens{Input: 1000, Output: 100})
	at(store, september)
	mustRecord(t, store, "anthropic", "claude-opus-5", Tokens{Input: 200, Output: 20, CacheRead: 5000, CacheWrite: 300})
	mustRecord(t, store, "anthropic", "claude-opus-5", Tokens{Input: 300, Output: 30})
	mustRecord(t, store, "openai", "gpt-6-astra", Tokens{Input: 50, Output: 5})
	// Nothing to count, as with Ollama.
	mustRecord(t, store, "ollama", "llama3.1:8b", Tokens{})

	totals, err := store.Since(ctx, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	want := "[{anthropic claude-opus-5 {500 50 5000 300} 2} {openai gpt-6-astra {50 5 0 0} 1}]"
	if got := fmt.Sprint(totals); got != want {
		t.Errorf("September = %s, want %s", got, want)
	}

	all, _ := store.Since(ctx, time.Time{})
	if len(all) != 2 || all[0].Input != 1500 || all[0].Answers != 3 {
		t.Errorf("all time = %v", all)
	}
}

func TestCountsOlderThanRetentionAreForgotten(t *testing.T) {
	store := openStore(t)
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	at(store, start)
	mustRecord(t, store, "anthropic", "claude-opus-5", Tokens{Input: 1})
	at(store, start.Add(Retention+time.Hour))
	mustRecord(t, store, "anthropic", "claude-opus-5", Tokens{Input: 2})

	totals, _ := store.Since(context.Background(), time.Time{})
	if len(totals) != 1 || totals[0].Input != 2 || totals[0].Answers != 1 {
		t.Errorf("totals = %v, want only the recent count", totals)
	}
}

func TestTheEndpoint(t *testing.T) {
	store := openStore(t)
	at(store, time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	mustRecord(t, store, "anthropic", "claude-opus-5", Tokens{Input: 200, Output: 20, CacheRead: 5000})

	mux := http.NewServeMux()
	(&Handler{Store: store}).RegisterMux(mux, func(h http.Handler) http.Handler { return h })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	status, body := get(t, srv.URL+"/llm/usage?since=2026-09-01T00:00:00%2B10:00")
	models, _ := body["models"].([]any)
	if status != 200 || len(models) != 1 || body["since"] != "2026-09-01T00:00:00+10:00" {
		t.Fatalf("usage = %d %v", status, body)
	}
	m := models[0].(map[string]any)
	if m["provider"] != "anthropic" || m["model"] != "claude-opus-5" || m["input_tokens"] != float64(200) ||
		m["cache_read_tokens"] != float64(5000) || m["answers"] != float64(1) {
		t.Errorf("model = %v", m)
	}

	if status, body := get(t, srv.URL+"/llm/usage?since=2026-10-01T00:00:00Z"); status != 200 || len(body["models"].([]any)) != 0 {
		t.Errorf("October = %d %v, want an empty list", status, body)
	}
	if status, _ := get(t, srv.URL+"/llm/usage?since=yesterday"); status != 400 {
		t.Errorf("bad since = %d, want 400", status)
	}

	// Without a store it says so, rather than 404 like an older helper.
	bare := http.NewServeMux()
	(&Handler{}).RegisterMux(bare, func(h http.Handler) http.Handler { return h })
	unavailable := httptest.NewServer(bare)
	defer unavailable.Close()
	if status, body := get(t, unavailable.URL+"/llm/usage"); status != 503 || body["error"] == nil {
		t.Errorf("without a store = %d %v", status, body)
	}
}

func mustRecord(t *testing.T, store *Store, provider, model string, tokens Tokens) {
	t.Helper()
	if err := store.Record(context.Background(), provider, model, tokens); err != nil {
		t.Fatalf("record: %v", err)
	}
}

func get(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("GET %s: body %q isn't JSON", url, data)
	}
	return resp.StatusCode, out
}
