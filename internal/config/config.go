package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Addr          string
	PairingToken  string
	OllamaURL     string
	OllamaModel   string
	TrueNASURL    string
	TrueNASAPIKey string
	// DataDir is the directory for everything that should survive
	// container restarts: the events DB and push prefs. The iOS
	// installer mounts a named volume at /data; the Dockerfile
	// pre-creates the directory owned by the app user so the volume
	// inherits sane ownership on first mount. On deploys that predate
	// the volume mount, /data still exists inside the container FS —
	// writable but ephemeral, i.e. exactly the durability we had when
	// everything lived in /tmp.
	DataDir string
	// EventsDBPath points to the SQLite file that backs the event store.
	// Defaults to {DataDir}/events.db; EVENTS_DB_PATH overrides for
	// tests and manual deploys with their own layout.
	EventsDBPath string
	// PushPrefsPath is the JSON file holding the iOS-synced notification
	// preferences (muted push categories). Lives next to the events DB.
	PushPrefsPath string
	// CapacityDBPath is the SQLite file holding one reading per pool per
	// day, the history behind the capacity forecast. Its own file so the
	// hourly sample never waits on the events DB's write lock.
	CapacityDBPath string
	// LLMConfigPath is the chosen model provider and its API key, written
	// by POST /llm/config with owner-only permissions.
	LLMConfigPath string
	// EventsRetentionDays bounds the events archive; a daily pruner
	// deletes rows older than this. 0 disables pruning entirely.
	EventsRetentionDays int

	// TSAuthKey is an optional Tailscale auth key. When set, the helper
	// joins the user's tailnet on boot via tsnet, which lets it resolve
	// the user's actual MagicDNS hostname for the iOS Add Tailscale flow
	// (and gives us a tailnet-native identity for future push relay
	// work). Empty string disables tsnet entirely — the helper runs
	// exactly as it does today.
	TSAuthKey string
	// TSHostname is the machine name the helper advertises on the
	// tailnet. Defaults to "zephyr-helper" so the tailnet admin
	// console doesn't show an unhelpful container hash.
	TSHostname string
	// TSStateDir is where tsnet persists its node key and state.
	// Without a volume mount the helper re-registers as a fresh
	// device on every restart, which still works but pollutes the
	// admin console's device list. Defaults to /tmp/tsnet so it
	// survives `tailscale up` restarts within a container lifetime.
	TSStateDir string

	// PushRelayURL is the base URL of the zephyr-push-relay
	// Cloudflare Worker (e.g. "https://push.zephyrtech.com.au").
	// When non-empty alongside PushSubscriptionID + PushInstallToken,
	// the helper starts a goroutine that forwards push-worthy events
	// (critical alerts, pool degradations, security events) to the
	// relay, which proxies them to OneSignal → APNs → the paired
	// iPhone. Empty disables push entirely; the helper still surfaces
	// events in-app via the existing /events stream.
	PushRelayURL string
	// PushSubscriptionID is the OneSignal subscription id the iOS
	// app registered with the relay at pair time. The relay uses it
	// to target the right device.
	PushSubscriptionID string
	// PushInstallToken is a 32-byte secret shared with the relay's
	// allowlist for this subscription. The relay rejects /push calls
	// whose token doesn't match the one iOS registered — so a leaked
	// helper from another user can't push to this device.
	PushInstallToken string
}

func Load() (*Config, error) {
	cfg := &Config{
		Addr:                envOr("ZEPHYR_ADDR", ":8080"),
		PairingToken:        os.Getenv("PAIRING_TOKEN"),
		OllamaURL:           envOr("OLLAMA_URL", "http://ollama:11434"),
		OllamaModel:         envOr("OLLAMA_MODEL", "qwen2.5:7b-instruct"),
		TrueNASURL:          os.Getenv("TRUENAS_URL"),
		TrueNASAPIKey:       os.Getenv("TRUENAS_API_KEY"),
		DataDir:             envOr("DATA_DIR", "/data"),
		EventsRetentionDays: envIntOr("EVENTS_RETENTION_DAYS", 90),
		TSAuthKey:           os.Getenv("TS_AUTHKEY"),
		TSHostname:          envOr("TS_HOSTNAME", "zephyr-helper"),
		TSStateDir:          envOr("TS_STATE_DIR", "/tmp/tsnet"),
		PushRelayURL:        os.Getenv("PUSH_RELAY_URL"),
		PushSubscriptionID:  os.Getenv("PUSH_SUBSCRIPTION_ID"),
		PushInstallToken:    os.Getenv("PUSH_INSTALL_TOKEN"),
	}

	// Ensure the data dir exists and is writable before deriving paths
	// from it. The Dockerfile pre-creates /data, but a manual deploy
	// with a custom DATA_DIR (or a hostile mount) can hand us a dir we
	// can't write — fall back to /tmp rather than crash-looping, since
	// an ephemeral event store is strictly better than no helper.
	if err := ensureWritableDir(cfg.DataDir); err != nil {
		fmt.Fprintf(os.Stderr, "data dir %q not writable (%v); falling back to /tmp — event history will not survive restarts\n", cfg.DataDir, err)
		cfg.DataDir = "/tmp"
	}
	cfg.EventsDBPath = envOr("EVENTS_DB_PATH", filepath.Join(cfg.DataDir, "events.db"))
	cfg.PushPrefsPath = filepath.Join(cfg.DataDir, "push_prefs.json")
	cfg.CapacityDBPath = filepath.Join(cfg.DataDir, "capacity.db")
	cfg.LLMConfigPath = filepath.Join(cfg.DataDir, "llm.json")

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

func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	probe.Close()
	return os.Remove(probe.Name())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
