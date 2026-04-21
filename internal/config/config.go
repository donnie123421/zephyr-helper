package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

type Config struct {
	Addr          string
	PairingToken  string
	OllamaURL     string
	OllamaModel   string
	TrueNASURL    string
	TrueNASAPIKey string
	// EventsDBPath points to the SQLite file that backs the event store.
	// Defaults to /tmp/events.db — the compose YAML doesn't mount a volume
	// yet, so a persistent path would fail to open on existing deploys.
	// The alerts poller writes here on every tick; losing history across
	// container restarts is acceptable until the installer gains a
	// /data volume mount and we bump the default to /data/events.db.
	EventsDBPath string
}

func Load() (*Config, error) {
	cfg := &Config{
		Addr:          envOr("ZEPHYR_ADDR", ":8080"),
		PairingToken:  os.Getenv("PAIRING_TOKEN"),
		OllamaURL:     envOr("OLLAMA_URL", "http://ollama:11434"),
		OllamaModel:   envOr("OLLAMA_MODEL", "qwen2.5:7b-instruct"),
		TrueNASURL:    os.Getenv("TRUENAS_URL"),
		TrueNASAPIKey: os.Getenv("TRUENAS_API_KEY"),
		EventsDBPath:  envOr("EVENTS_DB_PATH", "/tmp/events.db"),
	}

	// The in-app install always supplies PAIRING_TOKEN. For manual installs,
	// mint one and print it so the operator can pair by hand.
	if cfg.PairingToken == "" {
		tok, err := randomToken(32)
		if err != nil {
			return nil, fmt.Errorf("generate pairing token: %w", err)
		}
		cfg.PairingToken = tok
		fmt.Fprintf(os.Stderr, "\n*** ZEPHYR HELPER PAIRING TOKEN ***\n%s\n***\n\n", tok)
	}

	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
