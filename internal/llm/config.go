package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Config is the saved model choice. It holds API keys, so it lives in the
// helper's data folder readable only by the helper, and keys are never
// sent back out.
type Config struct {
	Provider  string         `json:"provider"`
	Anthropic ProviderConfig `json:"anthropic"`
	OpenAI    ProviderConfig `json:"openai"`
	Custom    ProviderConfig `json:"custom"`
}

// ProviderConfig is one hosted provider's settings.
type ProviderConfig struct {
	APIKey string `json:"api_key,omitempty"`
	Model  string `json:"model,omitempty"`
	// BaseURL is the server for Custom. For Anthropic and OpenAI it's an
	// override for a gateway; empty means their own API.
	BaseURL string `json:"base_url,omitempty"`
}

// section is the settings for one hosted provider, nil for any other.
func (c *Config) section(provider string) *ProviderConfig {
	switch provider {
	case ProviderAnthropic:
		return &c.Anthropic
	case ProviderOpenAI:
		return &c.OpenAI
	case ProviderCustom:
		return &c.Custom
	}
	return nil
}

// ConfigStore reads and writes the config file.
type ConfigStore struct {
	path string
}

func NewConfigStore(path string) *ConfigStore {
	return &ConfigStore{path: path}
}

// Load returns the saved config, or Ollama when nothing has been saved.
func (s *ConfigStore) Load() (Config, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{Provider: ProviderOllama}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if cfg.Provider == "" {
		cfg.Provider = ProviderOllama
	}
	return cfg, nil
}

// Save writes cfg with owner-only permissions, replacing the file whole so
// a crash can't leave half a config behind.
func (s *ConfigStore) Save(cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".llm-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Switch holds the provider new chats use, and swaps it when the config
// changes. A chat keeps the provider it started with.
type Switch struct {
	store  *ConfigStore
	ollama *Ollama

	mu      sync.RWMutex
	config  Config
	current Provider
}

// NewSwitch loads the saved config. A config that can't be read or used
// falls back to Ollama rather than leaving the helper without a model.
func NewSwitch(store *ConfigStore, ollama *Ollama) *Switch {
	sw := &Switch{
		store:   store,
		ollama:  ollama,
		config:  Config{Provider: ProviderOllama},
		current: ollama,
	}
	cfg, err := store.Load()
	if err != nil {
		slog.Warn("llm: config unreadable; using Ollama", "err", err)
		return sw
	}
	sw.config = cfg
	provider, err := sw.build(cfg)
	if err != nil {
		slog.Warn("llm: saved provider unusable; using Ollama", "provider", cfg.Provider, "err", err)
		return sw
	}
	sw.current = provider
	return sw
}

// Current is the provider for a new chat.
func (s *Switch) Current() Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Config is the saved config, keys included; only the helper sees it.
func (s *Switch) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

// Apply saves cfg and uses it for new chats. Nothing changes if cfg can't
// be used.
func (s *Switch) Apply(cfg Config) error {
	provider, err := s.build(cfg)
	if err != nil {
		return err
	}
	if err := s.store.Save(cfg); err != nil {
		return fmt.Errorf("save model settings: %w", err)
	}
	s.mu.Lock()
	s.config, s.current = cfg, provider
	s.mu.Unlock()
	slog.Info("llm: provider set", "provider", provider.Name(), "model", provider.Model())
	return nil
}

func (s *Switch) build(cfg Config) (Provider, error) {
	switch cfg.Provider {
	case ProviderOllama:
		return s.ollama, nil
	case ProviderAnthropic:
		if cfg.Anthropic.APIKey == "" {
			return nil, Problem("Add an Anthropic API key first.")
		}
		return NewAnthropic(cfg.Anthropic.APIKey, cfg.Anthropic.Model, cfg.Anthropic.BaseURL), nil
	case ProviderOpenAI:
		if cfg.OpenAI.APIKey == "" {
			return nil, Problem("Add an OpenAI API key first.")
		}
		model := cfg.OpenAI.Model
		if model == "" {
			model = DefaultOpenAIModel
		}
		return NewOpenAI(ProviderOpenAI, cfg.OpenAI.APIKey, model, cfg.OpenAI.BaseURL), nil
	case ProviderCustom:
		if cfg.Custom.BaseURL == "" {
			return nil, Problem("Add the server's address first.")
		}
		if cfg.Custom.Model == "" {
			return nil, Problem("Choose one of the server's models first.")
		}
		return NewOpenAI(ProviderCustom, cfg.Custom.APIKey, cfg.Custom.Model, cfg.Custom.BaseURL), nil
	default:
		return nil, Problem(fmt.Sprintf("Unknown provider %q.", cfg.Provider))
	}
}
