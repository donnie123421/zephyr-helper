package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/ollama"
)

// hostedProviders are the providers with a key, a model and (for Custom) an
// address to set.
var hostedProviders = []string{ProviderAnthropic, ProviderOpenAI, ProviderCustom}

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
	Provider  string       `json:"provider"`
	Providers []string     `json:"providers"`
	Anthropic providerView `json:"anthropic"`
	OpenAI    providerView `json:"openai"`
	Custom    providerView `json:"custom"`
	Ollama    ollamaView   `json:"ollama"`
}

type providerView struct {
	// Configured means the provider has what it needs apart from a model:
	// a key, or for Custom an address.
	Configured bool   `json:"configured"`
	KeyLast4   string `json:"key_last4,omitempty"`
	Model      string `json:"model"`
	BaseURL    string `json:"base_url,omitempty"`
}

type ollamaView struct {
	Model string `json:"model"`
}

func (h *Handler) view() configView {
	cfg := h.Switch.Config()
	return configView{
		Provider:  cfg.Provider,
		Providers: append([]string{ProviderOllama}, hostedProviders...),
		Anthropic: viewOf(ProviderAnthropic, cfg.Anthropic),
		OpenAI:    viewOf(ProviderOpenAI, cfg.OpenAI),
		Custom:    viewOf(ProviderCustom, cfg.Custom),
		Ollama:    ollamaView{Model: h.Ollama.Model()},
	}
}

func viewOf(provider string, p ProviderConfig) providerView {
	v := providerView{Model: p.Model, KeyLast4: last4(p.APIKey)}
	switch provider {
	case ProviderAnthropic:
		v.Configured = p.APIKey != ""
		if v.Model == "" {
			v.Model = DefaultAnthropicModel
		}
	case ProviderOpenAI:
		v.Configured = p.APIKey != ""
		if v.Model == "" {
			v.Model = DefaultOpenAIModel
		}
	case ProviderCustom:
		// The address isn't a secret, and the app shows it.
		v.Configured = p.BaseURL != ""
		v.BaseURL = p.BaseURL
	}
	return v
}

// last4 reveals the end of a key so the user can tell which one is saved;
// nothing at all for a key too short for that to be safe.
func last4(key string) string {
	if len(key) < 8 {
		return ""
	}
	return key[len(key)-4:]
}

func (h *Handler) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.view())
}

// sectionRequest changes one provider's settings. Empty fields leave the
// saved value alone.
type sectionRequest struct {
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
	BaseURL string `json:"base_url"`
	// RemoveKey forgets the key; Forget forgets everything for the
	// provider.
	RemoveKey bool `json:"remove_key"`
	Forget    bool `json:"forget"`
}

type configRequest struct {
	Provider  string         `json:"provider"`
	Anthropic sectionRequest `json:"anthropic"`
	OpenAI    sectionRequest `json:"openai"`
	Custom    sectionRequest `json:"custom"`
}

func (req configRequest) section(provider string) sectionRequest {
	switch provider {
	case ProviderAnthropic:
		return req.Anthropic
	case ProviderOpenAI:
		return req.OpenAI
	case ProviderCustom:
		return req.Custom
	}
	return sectionRequest{}
}

func (h *Handler) setConfig(w http.ResponseWriter, r *http.Request) {
	var req configRequest
	if !readJSON(w, r, &req) {
		return
	}
	cfg := h.Switch.Config()
	for _, provider := range hostedProviders {
		in := req.section(provider)
		saved := cfg.section(provider)
		if in.Forget {
			*saved = ProviderConfig{}
		}
		if in.RemoveKey {
			saved.APIKey = ""
		}
		if key := strings.TrimSpace(in.APIKey); key != "" {
			if !keysAllowed(r) {
				writeProblem(w, http.StatusForbidden, errKeysNeedHTTPS)
				return
			}
			saved.APIKey = key
		}
		if model := strings.TrimSpace(in.Model); model != "" {
			saved.Model = model
		}
		// Only Custom's address is the app's to set; the others' base URL is
		// a gateway override the app doesn't manage.
		if base := strings.TrimSpace(in.BaseURL); base != "" && provider == ProviderCustom {
			if err := checkBaseURL(base); err != nil {
				writeProblem(w, http.StatusBadRequest, err)
				return
			}
			saved.BaseURL = base
		}
	}
	if req.Provider != "" {
		cfg.Provider = req.Provider
	}
	if cfg.Provider == ProviderAnthropic && cfg.Anthropic.Model == "" {
		cfg.Anthropic.Model = DefaultAnthropicModel
	}
	if cfg.Provider == ProviderOpenAI && cfg.OpenAI.Model == "" {
		cfg.OpenAI.Model = DefaultOpenAIModel
	}
	if err := h.Switch.Apply(cfg); err != nil {
		writeProblem(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, h.view())
}

type testResponse struct {
	OK     bool        `json:"ok"`
	Error  string      `json:"error,omitempty"`
	Models []ModelInfo `json:"models,omitempty"`
}

// test checks a key, or a Custom server, by listing its models before the
// app saves it. A rejected key is a normal outcome, so it's a 200 with ok
// false. Anything not in the request comes from the saved settings.
func (h *Handler) test(w http.ResponseWriter, r *http.Request) {
	var req configRequest
	if !readJSON(w, r, &req) {
		return
	}
	cfg := h.Switch.Config()
	saved := cfg.section(req.Provider)
	if saved == nil {
		writeProblem(w, http.StatusBadRequest, Problem("Only Claude, ChatGPT and custom servers can be tested."))
		return
	}
	in := req.section(req.Provider)

	key := strings.TrimSpace(in.APIKey)
	if key != "" && !keysAllowed(r) {
		writeProblem(w, http.StatusForbidden, errKeysNeedHTTPS)
		return
	}
	if key == "" {
		key = saved.APIKey
	}
	base := saved.BaseURL
	if newBase := strings.TrimSpace(in.BaseURL); newBase != "" && req.Provider == ProviderCustom {
		if err := checkBaseURL(newBase); err != nil {
			writeJSON(w, http.StatusOK, testResponse{Error: messageFor(err)})
			return
		}
		base = newBase
	}
	switch {
	case req.Provider == ProviderCustom && base == "":
		writeJSON(w, http.StatusOK, testResponse{Error: "Add the server's address first."})
		return
	case req.Provider != ProviderCustom && key == "":
		writeJSON(w, http.StatusOK, testResponse{Error: "There's no key to test."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	models, err := listModels(ctx, req.Provider, key, base)
	if err != nil {
		writeJSON(w, http.StatusOK, testResponse{Error: messageFor(err)})
		return
	}
	writeJSON(w, http.StatusOK, testResponse{OK: true, Models: models})
}

// models lists what a provider's saved settings can use, for the picker.
// Without ?provider= it lists Claude's, as the first app versions expect.
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		provider = ProviderAnthropic
	}
	cfg := h.Switch.Config()
	saved := cfg.section(provider)
	if saved == nil {
		writeProblem(w, http.StatusBadRequest, Problem("Only Claude, ChatGPT and custom servers have models to list here."))
		return
	}
	if provider == ProviderCustom && saved.BaseURL == "" {
		writeProblem(w, http.StatusBadRequest, Problem("Add the server's address first."))
		return
	}
	if provider != ProviderCustom && saved.APIKey == "" {
		writeProblem(w, http.StatusBadRequest, Problem("Add an API key first."))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	models, err := listModels(ctx, provider, saved.APIKey, saved.BaseURL)
	if err != nil {
		writeProblem(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func listModels(ctx context.Context, provider, key, baseURL string) ([]ModelInfo, error) {
	if provider == ProviderAnthropic {
		return AnthropicModels(ctx, key, baseURL)
	}
	return OpenAIModels(ctx, provider, key, baseURL)
}

// checkBaseURL accepts an http or https address with a host.
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Problem("Enter the server's address in full, like http://192.168.1.20:1234/v1.")
	}
	return nil
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
