package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/ollama"
)

// Handler serves the app's model settings under /llm.
type Handler struct {
	Switch *Switch
	Ollama *ollama.Client
}

// RegisterMux mounts the endpoints, each wrapped by wrap (bearer auth).
func (h *Handler) RegisterMux(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /llm/config", wrap(http.HandlerFunc(h.getConfig)))
	mux.Handle("POST /llm/config", wrap(http.HandlerFunc(h.setConfig)))
	mux.Handle("POST /llm/test", wrap(http.HandlerFunc(h.test)))
	mux.Handle("GET /llm/models", wrap(http.HandlerFunc(h.models)))
}

// configView is the settings as the app sees them: never a key, only
// whether one is saved and its last four characters.
type configView struct {
	Provider  string        `json:"provider"`
	Providers []string      `json:"providers"`
	Anthropic anthropicView `json:"anthropic"`
	Ollama    ollamaView    `json:"ollama"`
}

type anthropicView struct {
	Configured bool   `json:"configured"`
	KeyLast4   string `json:"key_last4,omitempty"`
	Model      string `json:"model"`
}

type ollamaView struct {
	Model string `json:"model"`
}

func (h *Handler) view() configView {
	cfg := h.Switch.Config()
	v := configView{
		Provider:  cfg.Provider,
		Providers: []string{ProviderOllama, ProviderAnthropic},
		Anthropic: anthropicView{
			Configured: cfg.Anthropic.APIKey != "",
			Model:      cfg.Anthropic.Model,
		},
		Ollama: ollamaView{Model: h.Ollama.Model()},
	}
	if v.Anthropic.Model == "" {
		v.Anthropic.Model = DefaultAnthropicModel
	}
	if key := cfg.Anthropic.APIKey; len(key) >= 8 {
		v.Anthropic.KeyLast4 = key[len(key)-4:]
	}
	return v
}

func (h *Handler) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.view())
}

type configRequest struct {
	Provider  string `json:"provider"`
	Anthropic struct {
		// APIKey replaces the saved key; empty keeps it.
		APIKey    string `json:"api_key"`
		Model     string `json:"model"`
		RemoveKey bool   `json:"remove_key"`
	} `json:"anthropic"`
}

func (h *Handler) setConfig(w http.ResponseWriter, r *http.Request) {
	var req configRequest
	if !readJSON(w, r, &req) {
		return
	}
	cfg := h.Switch.Config()
	if req.Anthropic.RemoveKey {
		cfg.Anthropic.APIKey = ""
	}
	if key := strings.TrimSpace(req.Anthropic.APIKey); key != "" {
		if !keysAllowed(r) {
			writeProblem(w, http.StatusForbidden, errKeysNeedHTTPS)
			return
		}
		cfg.Anthropic.APIKey = key
	}
	if model := strings.TrimSpace(req.Anthropic.Model); model != "" {
		cfg.Anthropic.Model = model
	}
	if req.Provider != "" {
		cfg.Provider = req.Provider
	}
	if cfg.Provider == ProviderAnthropic && cfg.Anthropic.Model == "" {
		cfg.Anthropic.Model = DefaultAnthropicModel
	}
	if err := h.Switch.Apply(cfg); err != nil {
		writeProblem(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, h.view())
}

type testRequest struct {
	Provider  string `json:"provider"`
	Anthropic struct {
		APIKey string `json:"api_key"`
	} `json:"anthropic"`
}

type testResponse struct {
	OK     bool        `json:"ok"`
	Error  string      `json:"error,omitempty"`
	Models []ModelInfo `json:"models,omitempty"`
}

// test checks a key by listing the models it can use, before the app saves
// it. A rejected key is a normal outcome, so it's a 200 with ok false.
func (h *Handler) test(w http.ResponseWriter, r *http.Request) {
	var req testRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Provider != ProviderAnthropic {
		writeProblem(w, http.StatusBadRequest, Problem("Only Anthropic keys can be tested."))
		return
	}
	cfg := h.Switch.Config()
	key := strings.TrimSpace(req.Anthropic.APIKey)
	if key != "" && !keysAllowed(r) {
		writeProblem(w, http.StatusForbidden, errKeysNeedHTTPS)
		return
	}
	if key == "" {
		key = cfg.Anthropic.APIKey
	}
	if key == "" {
		writeJSON(w, http.StatusOK, testResponse{Error: "There's no key to test."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	models, err := AnthropicModels(ctx, key, cfg.Anthropic.BaseURL)
	if err != nil {
		writeJSON(w, http.StatusOK, testResponse{Error: messageFor(err)})
		return
	}
	writeJSON(w, http.StatusOK, testResponse{OK: true, Models: models})
}

// models lists what the saved key can use, for the picker.
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	if p := r.URL.Query().Get("provider"); p != "" && p != ProviderAnthropic {
		writeProblem(w, http.StatusBadRequest, Problem("Only Anthropic models can be listed here."))
		return
	}
	cfg := h.Switch.Config()
	if cfg.Anthropic.APIKey == "" {
		writeProblem(w, http.StatusBadRequest, Problem("Add an Anthropic API key first."))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	models, err := AnthropicModels(ctx, cfg.Anthropic.APIKey, cfg.Anthropic.BaseURL)
	if err != nil {
		writeProblem(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

var errKeysNeedHTTPS = Problem("API keys are only accepted over HTTPS. Update Zephyr, then try again.")

// keysAllowed reports whether r arrived over TLS. The app only sends a key
// over the helper's pinned HTTPS, so a key on a plain connection means an
// app or path that shouldn't be handling one.
func keysAllowed(r *http.Request) bool {
	return r.TLS != nil
}

func messageFor(err error) string {
	var problem Problem
	if errors.As(err, &problem) {
		return string(problem)
	}
	return err.Error()
}

func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(into); err != nil {
		writeProblem(w, http.StatusBadRequest, Problem("The request body isn't valid JSON."))
		return false
	}
	return true
}

func writeProblem(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": messageFor(err)})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
