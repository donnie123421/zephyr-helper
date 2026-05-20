package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/auth"
	"github.com/donnie123421/zephyr-helper/internal/chat"
	"github.com/donnie123421/zephyr-helper/internal/config"
	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/events/correlators"
	eventshttp "github.com/donnie123421/zephyr-helper/internal/events/http"
	"github.com/donnie123421/zephyr-helper/internal/models"
	"github.com/donnie123421/zephyr-helper/internal/ollama"
	"github.com/donnie123421/zephyr-helper/internal/pollers"
	"github.com/donnie123421/zephyr-helper/internal/push"
	"github.com/donnie123421/zephyr-helper/internal/remoteaccess"
	"github.com/donnie123421/zephyr-helper/internal/tailnet"
	"github.com/donnie123421/zephyr-helper/internal/tools"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
	"github.com/donnie123421/zephyr-helper/internal/version"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}

	slog.Info("starting zephyr-helper",
		"version", version.Version,
		"addr", cfg.Addr,
		"ollama_url", cfg.OllamaURL,
		"ollama_model", cfg.OllamaModel,
		"truenas_configured", cfg.TrueNASURL != "" && cfg.TrueNASAPIKey != "",
	)

	tnClient, err := truenas.NewClient(cfg.TrueNASURL, cfg.TrueNASAPIKey)
	if err != nil {
		// Bad URL is a config bug — fail loud rather than silently disable tools.
		slog.Error("truenas client", "err", err)
		os.Exit(1)
	}
	defer tnClient.Close()
	toolRegistry := tools.New(tnClient)
	if toolRegistry.Empty() {
		slog.Warn("tools: registry empty — TRUENAS_URL or TRUENAS_API_KEY missing; chat will run without NAS tools")
	} else {
		slog.Info("tools: registered", "count", len(toolRegistry.Definitions()))
	}

	ollamaClient := ollama.NewClient(cfg.OllamaURL, cfg.OllamaModel)
	chatHandler := chat.NewHandler(ollamaClient, toolRegistry)
	modelsHandler := models.NewHandler(ollamaClient)

	eventsStore, err := events.Open(cfg.EventsDBPath)
	if err != nil {
		slog.Error("events store open", "err", err, "path", cfg.EventsDBPath)
		os.Exit(1)
	}
	defer eventsStore.Close()
	slog.Info("events: store ready", "path", cfg.EventsDBPath)
	eventsHandler := &eventshttp.Handler{Store: eventsStore}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Correlators subscribe to the store and emit narrative parent rows
	// for child events the pollers ingest. Start them before the
	// pollers so the first poll's broadcast doesn't slip past.
	scrubCorrelator := correlators.NewScrubOutcome(eventsStore)
	go scrubCorrelator.Run(ctx)
	slog.Info("scrub correlator started")

	diskCorrelator := correlators.NewDiskReplacement(eventsStore)
	go diskCorrelator.Run(ctx)
	slog.Info("disk correlator started")

	snapshotCorrelator := correlators.NewSnapshotRollup(eventsStore)
	go snapshotCorrelator.Run(ctx)
	slog.Info("snapshot correlator started")

	if tnClient.Configured() {
		alertsPoller := pollers.NewAlerts(tnClient, eventsStore, pollers.DefaultAlertInterval, pollers.DefaultAlertMergeWindow)
		go alertsPoller.Run(ctx)
		slog.Info("alerts poller started", "interval", pollers.DefaultAlertInterval)

		jobsPoller := pollers.NewJobs(tnClient, eventsStore, pollers.DefaultJobInterval, pollers.DefaultJobMergeWindow)
		go jobsPoller.Run(ctx)
		slog.Info("jobs poller started", "interval", pollers.DefaultJobInterval)

		securityPoller := pollers.NewSecurity(tnClient, eventsStore, pollers.DefaultSecurityInterval)
		go securityPoller.Run(ctx)
		slog.Info("security poller started", "interval", pollers.DefaultSecurityInterval)
	}

	// Push notifier: subscribes to eventsStore and POSTs critical
	// events to the zephyr-push-relay Cloudflare Worker, which proxies
	// to OneSignal → APNs → the paired iPhone. No-op when any of the
	// three PUSH_* env vars is missing (e.g. iOS install where the
	// user denied notification permission).
	pushNotifier := push.New(
		eventsStore,
		cfg.PushRelayURL,
		cfg.PushSubscriptionID,
		cfg.PushInstallToken,
		slog.Default(),
	)
	go pushNotifier.Run(ctx)
	if cfg.PushRelayURL != "" {
		slog.Info("push notifier started", "relay", cfg.PushRelayURL)
	}

	// Optional: join the user's tailnet so /remote-access can resolve
	// the actual MagicDNS hostname for the iOS Add Tailscale flow.
	// Bring it up before the HTTP server starts so the first /remote-
	// access call from the app already has a primed status. tsnet
	// registration takes a few hundred ms on first run; we cap at
	// 30s so a misconfigured key doesn't stall startup forever.
	tsServer := tailnet.New(cfg.TSAuthKey, cfg.TSHostname, cfg.TSStateDir, slog.Default())
	if tsServer.Available() {
		startCtx, startCancel := context.WithTimeout(ctx, 30*time.Second)
		if err := tsServer.Start(startCtx); err != nil {
			// Don't fail the whole helper — the tailnet is a nice-to-
			// have. Log and continue; /remote-access falls back to
			// chart-scrape + NAS hostname suggestion.
			slog.Warn("tsnet: start failed; continuing without tailnet", "err", err)
		}
		startCancel()
	}
	defer tsServer.Stop()

	remoteHandler := &remoteaccess.Handler{TN: tnClient, Tailnet: tsServer, Log: slog.Default()}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /version", handleVersion)
	mux.Handle("POST /auth/verify", auth.Require(cfg.PairingToken, http.HandlerFunc(handleAuthVerify)))
	mux.Handle("GET /chat", auth.Require(cfg.PairingToken, chatHandler))
	mux.Handle("GET /model", auth.Require(cfg.PairingToken, http.HandlerFunc(modelsHandler.HandleGet)))
	mux.Handle("POST /model", auth.Require(cfg.PairingToken, http.HandlerFunc(modelsHandler.HandleSet)))
	mux.Handle("GET /remote-access", auth.Require(cfg.PairingToken, remoteHandler))
	eventsHandler.RegisterMux(mux, func(h http.Handler) http.Handler {
		return auth.Require(cfg.PairingToken, h)
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen", "err", err)
			os.Exit(1)
		}
	}()
	slog.Info("listening", "addr", cfg.Addr)

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "err", err)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"version":"` + version.Version + `","commit":"` + version.Commit + `"}`))
}

func handleAuthVerify(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
