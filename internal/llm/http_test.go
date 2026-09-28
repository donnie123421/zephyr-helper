package llm

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/donnie123421/zephyr-helper/internal/ollama"
)

const testKey = "sk-ant-api03-secret-key-WXYZ"

func newTestHandler(t *testing.T, saved *Config) (*Handler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llm.json")
	store := NewConfigStore(path)
	if saved != nil {
		if err := store.Save(*saved); err != nil {
			t.Fatalf("seed config: %v", err)
		}
	}
	client := ollama.NewClient("http://127.0.0.1:1", "qwen2.5:7b-instruct")
	sw := NewSwitch(store, NewOllama(client))
	return &Handler{Switch: sw, Ollama: client}, path
}

func serve(h *Handler, method, path, body string, overTLS bool) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	h.RegisterMux(mux, func(next http.Handler) http.Handler { return next })
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if overTLS {
		req.TLS = &tls.ConnectionState{}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeView(t *testing.T, rec *httptest.ResponseRecorder) configView {
	t.Helper()
	var v configView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return v
}

func TestDefaultsToOllama(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := serve(h, http.MethodGet, "/llm/config", "", true)
	v := decodeView(t, rec)
	if v.Provider != ProviderOllama || v.Anthropic.Configured || v.Ollama.Model != "qwen2.5:7b-instruct" {
		t.Errorf("view = %+v", v)
	}
	if v.Anthropic.Model != DefaultAnthropicModel {
		t.Errorf("anthropic model = %q, want the default", v.Anthropic.Model)
	}
	if h.Switch.Current().Name() != ProviderOllama {
		t.Errorf("current = %s", h.Switch.Current().Name())
	}
}

func TestSavingAKeySwitchesToClaudeWithoutExposingIt(t *testing.T) {
	h, path := newTestHandler(t, nil)
	rec := serve(h, http.MethodPost, "/llm/config",
		`{"provider":"anthropic","anthropic":{"api_key":"`+testKey+`","model":"claude-sonnet-5"}}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("response leaked the key: %s", rec.Body.String())
	}
	v := decodeView(t, rec)
	if v.Provider != ProviderAnthropic || !v.Anthropic.Configured || v.Anthropic.KeyLast4 != "WXYZ" || v.Anthropic.Model != "claude-sonnet-5" {
		t.Errorf("view = %+v", v)
	}
	if cur := h.Switch.Current(); cur.Name() != ProviderAnthropic || cur.Model() != "claude-sonnet-5" {
		t.Errorf("current = %s/%s", cur.Name(), cur.Model())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", info.Mode().Perm())
	}
	if get := serve(h, http.MethodGet, "/llm/config", "", false); strings.Contains(get.Body.String(), "secret") {
		t.Fatalf("GET leaked the key: %s", get.Body.String())
	}

	// A restart picks the saved provider back up.
	reloaded := NewSwitch(NewConfigStore(path), h.Switch.ollama)
	if reloaded.Current().Name() != ProviderAnthropic {
		t.Errorf("after reload current = %s", reloaded.Current().Name())
	}
}

func TestKeysAreRefusedOverPlainHTTP(t *testing.T) {
	h, path := newTestHandler(t, nil)
	rec := serve(h, http.MethodPost, "/llm/config",
		`{"provider":"anthropic","anthropic":{"api_key":"`+testKey+`"}}`, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a key sent over plain HTTP was saved")
	}
	if rec := serve(h, http.MethodPost, "/llm/test",
		`{"provider":"anthropic","anthropic":{"api_key":"`+testKey+`"}}`, false); rec.Code != http.StatusForbidden {
		t.Errorf("test over plain HTTP: status %d, want 403", rec.Code)
	}
}

func TestChoosingClaudeNeedsAKey(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := serve(h, http.MethodPost, "/llm/config", `{"provider":"anthropic"}`, true)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "API key") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if h.Switch.Current().Name() != ProviderOllama {
		t.Error("a failed save changed the provider")
	}
}

func TestRemovingTheKeyGoesBackToOllama(t *testing.T) {
	h, _ := newTestHandler(t, &Config{Provider: ProviderAnthropic, Anthropic: ProviderConfig{APIKey: testKey, Model: "claude-opus-5"}})
	if h.Switch.Current().Name() != ProviderAnthropic {
		t.Fatalf("seeded provider = %s", h.Switch.Current().Name())
	}
	rec := serve(h, http.MethodPost, "/llm/config", `{"provider":"ollama","anthropic":{"remove_key":true}}`, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	v := decodeView(t, rec)
	if v.Provider != ProviderOllama || v.Anthropic.Configured {
		t.Errorf("view = %+v", v)
	}
}

func TestTestingAKeyListsModels(t *testing.T) {
	fake := &fakeClaude{models: []string{"claude-opus-5", "claude-haiku-4-5"}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	// BaseURL points the helper's Anthropic calls at the fake.
	h, _ := newTestHandler(t, &Config{Provider: ProviderOllama, Anthropic: ProviderConfig{BaseURL: srv.URL}})

	rec := serve(h, http.MethodPost, "/llm/test", `{"provider":"anthropic","anthropic":{"api_key":"`+testKey+`"}}`, true)
	var got testResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || len(got.Models) != 2 || got.Models[1].ID != "claude-haiku-4-5" {
		t.Errorf("test = %+v", got)
	}

	fake.status = http.StatusUnauthorized
	rec = serve(h, http.MethodPost, "/llm/test", `{"provider":"anthropic","anthropic":{"api_key":"bad"}}`, true)
	got = testResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.OK || !strings.Contains(got.Error, "rejected the API key") {
		t.Errorf("rejected key: status %d, %+v", rec.Code, got)
	}
}

func TestSavingAnOpenAIKeyDefaultsToTheFlagship(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := serve(h, http.MethodPost, "/llm/config", `{"provider":"openai","openai":{"api_key":"sk-proj-secret-OPEN"}}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("response leaked the key: %s", rec.Body.String())
	}
	v := decodeView(t, rec)
	if v.Provider != ProviderOpenAI || !v.OpenAI.Configured || v.OpenAI.KeyLast4 != "OPEN" || v.OpenAI.Model != DefaultOpenAIModel {
		t.Errorf("view = %+v", v.OpenAI)
	}
	if cur := h.Switch.Current(); cur.Name() != ProviderOpenAI || cur.Model() != DefaultOpenAIModel {
		t.Errorf("current = %s/%s", cur.Name(), cur.Model())
	}
	if rec := serve(h, http.MethodPost, "/llm/config", `{"openai":{"api_key":"sk-proj-other-key"}}`, false); rec.Code != http.StatusForbidden {
		t.Errorf("an OpenAI key over plain HTTP: status %d, want 403", rec.Code)
	}
}

func TestCustomServerNeedsAnAddressAndAModel(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	if rec := serve(h, http.MethodPost, "/llm/config", `{"provider":"custom","custom":{"base_url":"ftp://pc"}}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("ftp address: status %d, want 400", rec.Code)
	}
	rec := serve(h, http.MethodPost, "/llm/config", `{"provider":"custom","custom":{"base_url":"http://pc:1234/v1"}}`, true)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "models") {
		t.Errorf("no model: status %d %s", rec.Code, rec.Body.String())
	}

	// No key needed for a local server, so plain HTTP is fine here.
	rec = serve(h, http.MethodPost, "/llm/config", `{"provider":"custom","custom":{"base_url":"http://pc:1234/v1","model":"qwen3-32b"}}`, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	v := decodeView(t, rec)
	if v.Provider != ProviderCustom || !v.Custom.Configured || v.Custom.BaseURL != "http://pc:1234/v1" || v.Custom.Model != "qwen3-32b" {
		t.Errorf("view = %+v", v.Custom)
	}

	rec = serve(h, http.MethodPost, "/llm/config", `{"provider":"ollama","custom":{"forget":true}}`, false)
	if v := decodeView(t, rec); v.Custom.Configured || v.Custom.BaseURL != "" || v.Custom.Model != "" {
		t.Errorf("after forget: %+v", v.Custom)
	}
}

func TestTestingACustomServerListsItsModels(t *testing.T) {
	fake := &fakeOpenAI{models: []string{"qwen3-32b", "llama-4-scout"}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	h, _ := newTestHandler(t, nil)

	rec := serve(h, http.MethodPost, "/llm/test", `{"provider":"custom","custom":{"base_url":"`+srv.URL+`/v1"}}`, false)
	var got testResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || len(got.Models) != 2 {
		t.Errorf("test = %+v", got)
	}

	rec = serve(h, http.MethodPost, "/llm/test", `{"provider":"custom","custom":{}}`, false)
	got = testResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.OK || !strings.Contains(got.Error, "address") {
		t.Errorf("no address: %+v", got)
	}
}

func TestListingOpenAIModelsUsesTheSavedKey(t *testing.T) {
	fake := &fakeOpenAI{models: []string{"gpt-6-luna", "gpt-6-astra", "whisper-3"}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	h, _ := newTestHandler(t, &Config{Provider: ProviderOllama, OpenAI: ProviderConfig{APIKey: "sk-proj-saved-key", BaseURL: srv.URL + "/v1"}})

	rec := serve(h, http.MethodGet, "/llm/models?provider=openai", "", false)
	var body struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Models) != 2 || body.Models[0].ID != "gpt-6-astra" {
		t.Errorf("models = %s (%v)", rec.Body.String(), err)
	}
}
