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
		Addr:               envOr("ZEPHYR_ADDR", ":8080"),
		PairingToken:       os.Getenv("PAIRING_TOKEN"),
		OllamaURL:          envOr("OLLAMA_URL", "http://ollama:11434"),
		OllamaModel:        envOr("OLLAMA_MODEL", "qwen2.5:7b-instruct"),
		TrueNASURL:         os.Getenv("TRUENAS_URL"),
		TrueNASAPIKey:      os.Getenv("TRUENAS_API_KEY"),
		EventsDBPath:       envOr("EVENTS_DB_PATH", "/tmp/events.db"),
		TSAuthKey:          os.Getenv("TS_AUTHKEY"),
		TSHostname:         envOr("TS_HOSTNAME", "zephyr-helper"),
		TSStateDir:         envOr("TS_STATE_DIR", "/tmp/tsnet"),
		PushRelayURL:       os.Getenv("PUSH_RELAY_URL"),
		PushSubscriptionID: os.Getenv("PUSH_SUBSCRIPTION_ID"),
		PushInstallToken:   os.Getenv("PUSH_INSTALL_TOKEN"),
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
