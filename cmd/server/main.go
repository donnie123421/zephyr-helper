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

	if tnClient.Configured() {
		alertsPoller := pollers.NewAlerts(tnClient, eventsStore, pollers.DefaultAlertInterval, pollers.DefaultAlertMergeWindow)
		go alertsPoller.Run(ctx)
		slog.Info("alerts poller started", "interval", pollers.DefaultAlertInterval)

		jobsPoller := pollers.NewJobs(tnClient, eventsStore, pollers.DefaultJobInterval, pollers.DefaultJobMergeWindow)
		go jobsPoller.Run(ctx)
		slog.Info("jobs poller started", "interval", pollers.DefaultJobInterval)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /version", handleVersion)
	mux.Handle("POST /auth/verify", auth.Require(cfg.PairingToken, http.HandlerFunc(handleAuthVerify)))
	mux.Handle("GET /chat", auth.Require(cfg.PairingToken, chatHandler))
	mux.Handle("GET /model", auth.Require(cfg.PairingToken, http.HandlerFunc(modelsHandler.HandleGet)))
	mux.Handle("POST /model", auth.Require(cfg.PairingToken, http.HandlerFunc(modelsHandler.HandleSet)))
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
