package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// Embed tzdata so quiet-hours windows resolve IANA zones inside the
	// alpine container (which ships no /usr/share/zoneinfo).
	_ "time/tzdata"

	"github.com/donnie123421/zephyr-helper/internal/actions"
	"github.com/donnie123421/zephyr-helper/internal/auth"
	"github.com/donnie123421/zephyr-helper/internal/capacity"
	"github.com/donnie123421/zephyr-helper/internal/chat"
	"github.com/donnie123421/zephyr-helper/internal/config"
	"github.com/donnie123421/zephyr-helper/internal/conversations"
	"github.com/donnie123421/zephyr-helper/internal/digest"
	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/events/correlators"
	eventshttp "github.com/donnie123421/zephyr-helper/internal/events/http"
	"github.com/donnie123421/zephyr-helper/internal/llm"
	"github.com/donnie123421/zephyr-helper/internal/models"
	"github.com/donnie123421/zephyr-helper/internal/ollama"
	"github.com/donnie123421/zephyr-helper/internal/pollers"
	"github.com/donnie123421/zephyr-helper/internal/pro"
	"github.com/donnie123421/zephyr-helper/internal/push"
	"github.com/donnie123421/zephyr-helper/internal/remoteaccess"
	"github.com/donnie123421/zephyr-helper/internal/schedule"
	"github.com/donnie123421/zephyr-helper/internal/tailnet"
	"github.com/donnie123421/zephyr-helper/internal/tlscert"
	"github.com/donnie123421/zephyr-helper/internal/tlsmux"
	"github.com/donnie123421/zephyr-helper/internal/tools"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
	"github.com/donnie123421/zephyr-helper/internal/usage"
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
		slog.Info("tools: registered", "count", toolRegistry.Len(), "actions", toolRegistry.HasActions())
	}

	ollamaClient := ollama.NewClient(cfg.OllamaURL, cfg.OllamaModel)
	// The model new chats use: Ollama unless the app has saved another
	// provider and its key.
	llmSwitch := llm.NewSwitch(llm.NewConfigStore(cfg.LLMConfigPath), llm.NewOllama(ollamaClient))
	slog.Info("llm: provider", "provider", llmSwitch.Current().Name(), "model", llmSwitch.Current().Model())

	// Saved chats. Without the store the assistant still chats; it just
	// can't keep them.
	conversationStore, err := conversations.Open(cfg.ConversationsDBPath)
	if err != nil {
		slog.Warn("conversations store open; chats won't be saved", "err", err, "path", cfg.ConversationsDBPath)
	} else {
		defer conversationStore.Close()
	}
	// Token counts, for the app's month total. Without the store chats
	// carry on; the app just can't show the month.
	usageStore, err := usage.Open(cfg.UsageDBPath)
	if err != nil {
		slog.Warn("usage store open; tokens won't be counted", "err", err, "path", cfg.UsageDBPath)
	} else {
		defer usageStore.Close()
	}
	llmHandler := &llm.Handler{Switch: llmSwitch, Ollama: ollamaClient}
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

	// What the assistant does with the user's approval goes in the
	// activity feed, along with how the jobs it starts end.
	var actionLog *actions.Log
	if tnClient.Configured() {
		actionLog = actions.New(ctx, eventsStore, tnClient)
	}
	chatHandler := chat.NewHandler(llmSwitch, toolRegistry, conversationStore, usageStore, actionLog)

	// Whether Zephyr Pro has lapsed, as the app last said: while it has,
	// nothing booked runs and no digest goes out. Without its file, Pro
	// is taken as not lapsed, and its routes answer 503.
	proState, err := pro.Open(cfg.ProStatePath)
	if err != nil {
		slog.Warn("pro state unavailable; taking Pro as not lapsed", "err", err, "path", cfg.ProStatePath)
	}

	// What's booked for later through the assistant: actions, the app
	// update rule and check-ins. Booking takes the same approval cards as
	// actions, so its tools come with them.
	var scheduler *schedule.Scheduler
	scheduleStore, err := schedule.Open(cfg.ScheduleDBPath)
	if err != nil {
		slog.Warn("schedule store open; nothing can be booked", "err", err, "path", cfg.ScheduleDBPath)
	} else {
		defer scheduleStore.Close()
		if actionLog != nil {
			toolRegistry.Add(schedule.Booking{
				Store: scheduleStore, Registry: toolRegistry, NAS: tnClient,
				Model: func() (string, bool) { return modelLabel(llmSwitch.Current()) },
			}.Tools()...)
			scheduler = schedule.New(scheduleStore, toolRegistry, actionLog, eventsStore, tnClient, chatHandler)
			scheduler.Held = proState.Lapsed
		}
	}

	// Retention: prune events older than the configured window, once at
	// boot and then daily. Without this the archive grows unbounded now
	// that the DB lives on a persistent volume.
	if cfg.EventsRetentionDays > 0 {
		retention := time.Duration(cfg.EventsRetentionDays) * 24 * time.Hour
		go func() {
			prune := func() {
				n, err := eventsStore.Prune(ctx, time.Now().Add(-retention))
				if err != nil {
					slog.Warn("events prune", "err", err)
					return
				}
				if n > 0 {
					slog.Info("events pruned", "deleted", n, "retention_days", cfg.EventsRetentionDays)
				}
			}
			prune()
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					prune()
				}
			}
		}()
		slog.Info("events retention active", "days", cfg.EventsRetentionDays)
	}

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

	var capacityStore *capacity.Store
	if tnClient.Configured() {
		alertsPoller := pollers.NewAlerts(tnClient, eventsStore, pollers.DefaultAlertInterval, pollers.DefaultAlertMergeWindow)
		go alertsPoller.Run(ctx)
		slog.Info("alerts poller started", "interval", pollers.DefaultAlertInterval)

		jobsPoller := pollers.NewJobs(tnClient, eventsStore, pollers.DefaultJobInterval, pollers.DefaultJobMergeWindow)
		// Jobs the assistant started get their own entry from the log.
		jobsPoller.Skip = actionLog.Following
		go jobsPoller.Run(ctx)
		slog.Info("jobs poller started", "interval", pollers.DefaultJobInterval)

		// TrueNAS raises nothing when a resilver finishes; this notices.
		resilvers := pollers.NewResilvers(tnClient, eventsStore, pollers.DefaultResilverInterval)
		go resilvers.Run(ctx)
		slog.Info("resilver watcher started", "interval", pollers.DefaultResilverInterval)

		securityPoller := pollers.NewSecurity(tnClient, eventsStore, pollers.DefaultSecurityInterval)
		go securityPoller.Run(ctx)
		slog.Info("security poller started", "interval", pollers.DefaultSecurityInterval)

		// Capacity history for the pool-full forecast. Losing it only
		// costs the forecast its history, so a failure here is logged
		// rather than taking the rest of the helper down with it.
		var openErr error
		capacityStore, openErr = capacity.Open(cfg.CapacityDBPath)
		if openErr != nil {
			capacityStore = nil
			slog.Warn("capacity store open; not recording pool usage", "err", openErr, "path", cfg.CapacityDBPath)
		} else {
			defer capacityStore.Close()
			go capacity.NewRecorder(tnClient, capacityStore, capacity.DefaultInterval).Run(ctx)
			slog.Info("capacity recorder started", "path", cfg.CapacityDBPath, "interval", capacity.DefaultInterval)
		}
	}

	// Push prefs: the iOS-synced muted categories, persisted next to
	// the events DB. Consulted by the notifier below and exposed to the
	// app via GET/POST /push/prefs so the in-app toggles govern APNs
	// pushes, not just local notifications.
	pushPrefs := push.NewPrefsStore(cfg.PushPrefsPath)
	pushPrefsHandler := &push.PrefsHandler{Store: pushPrefs}

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
		pushPrefs,
		slog.Default(),
	)
	go pushNotifier.Run(ctx)
	if cfg.PushRelayURL != "" {
		slog.Info("push notifier started", "relay", cfg.PushRelayURL)
	}

	// The scheduler starts by noting runs missed or cut short while the
	// helper was down; with the notifier listening first, those notify.
	if scheduler != nil {
		go func() {
			select {
			case <-pushNotifier.Ready():
			case <-time.After(15 * time.Second):
			case <-ctx.Done():
				return
			}
			slog.Info("scheduler started", "path", cfg.ScheduleDBPath)
			scheduler.Run(ctx)
		}()
	}

	// The weekly digest. It's off until the app turns it on: it's part of
	// Zephyr Pro, which the helper can't see. Without its files, its routes
	// still answer, with 503.
	digestStore, storeErr := digest.OpenStore(cfg.DigestPath)
	digestSettings, settingsErr := digest.OpenSettings(cfg.DigestSettingsPath)
	digestSources := digest.Sources{NAS: tnClient, Events: eventsStore, Capacity: capacityStore,
		Summarizer: chatHandler, Store: digestStore}
	digestHandler := &digest.Handler{Sources: digestSources, Settings: digestSettings, Held: proState.Lapsed}
	if storeErr != nil || settingsErr != nil {
		slog.Warn("digest unavailable", "store", storeErr, "settings", settingsErr)
	} else if tnClient.Configured() {
		runner := &digest.Runner{Sources: digestSources, Settings: digestSettings,
			Zone: digest.NASZone(tnClient), Quiet: pushPrefs.InQuietHours, Held: proState.Lapsed}
		go func() {
			select {
			case <-pushNotifier.Ready():
			case <-time.After(15 * time.Second):
			case <-ctx.Done():
				return
			}
			runner.Run(ctx)
		}()
	}

	// Optional: join the user's tailnet so /remote-access can resolve
	// the actual MagicDNS hostname for the iOS Add Tailscale flow.
	// Bring it up before the HTTP server starts so the first /remote-
	// access call from the app already has a primed status. tsnet
	// registration takes a few hundred ms on first run; we cap at
	// 30s so a misconfigured key doesn't stall startup forever.
	tsServer := tailnet.New(cfg.TSAuthKey, cfg.TSHostname, cfg.TSStateDir, slog.Default())
	onTailnet := false
	if tsServer.Available() {
		startCtx, startCancel := context.WithTimeout(ctx, 30*time.Second)
		if err := tsServer.Start(startCtx); err != nil {
			// Don't fail the whole helper — the tailnet is a nice-to-
			// have. Log and continue; /remote-access falls back to
			// chart-scrape + NAS hostname suggestion.
			slog.Warn("tsnet: start failed; continuing without tailnet", "err", err)
		} else {
			onTailnet = true
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
	pushPrefsHandler.RegisterMux(mux, func(h http.Handler) http.Handler {
		return auth.Require(cfg.PairingToken, h)
	})
	llmHandler.RegisterMux(mux, func(h http.Handler) http.Handler {
		return auth.Require(cfg.PairingToken, h)
	})
	// Registered without a store too: it answers 503 rather than the
	// plain 404 the app reads as a helper that predates saved chats.
	(&conversations.Handler{Store: conversationStore}).RegisterMux(mux, func(h http.Handler) http.Handler {
		return auth.Require(cfg.PairingToken, h)
	})
	// The same goes for the month's token counts.
	(&schedule.Handler{Store: scheduleStore, Scheduler: scheduler}).RegisterMux(mux, func(h http.Handler) http.Handler {
		return auth.Require(cfg.PairingToken, h)
	})
	(&capacity.Handler{Store: capacityStore}).RegisterMux(mux, func(h http.Handler) http.Handler {
		return auth.Require(cfg.PairingToken, h)
	})
	digestHandler.RegisterMux(mux, func(h http.Handler) http.Handler {
		return auth.Require(cfg.PairingToken, h)
	})
	proHandler := &pro.Handler{Store: proState}
	if scheduleStore != nil {
		proHandler.Bookings = scheduleStore
	}
	proHandler.RegisterMux(mux, func(h http.Handler) http.Handler {
		return auth.Require(cfg.PairingToken, h)
	})
	(&usage.Handler{Store: usageStore}).RegisterMux(mux, func(h http.Handler) http.Handler {
		return auth.Require(cfg.PairingToken, h)
	})

	// HTTPS on the same port as plain HTTP, sorted per connection by
	// tlsmux, so existing installs keep their compose file and app
	// versions from before HTTPS keep working. Without a certificate the
	// helper stays on plain HTTP; the app finds no /tls and does too.
	var tlsConfig *tls.Config
	if cert, err := tlscert.LoadOrCreate(cfg.DataDir); err != nil {
		slog.Warn("tls: no certificate; serving plain HTTP only", "err", err)
	} else {
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			// HTTP/1.1 only: the chat WebSocket takes over its
			// connection, which HTTP/2 doesn't allow.
			NextProtos: []string{"http/1.1"},
		}
		fingerprint := tlscert.Fingerprint(cert)
		// Deliberately unauthenticated — see ProofHandler.
		mux.Handle("GET /tls", tlscert.ProofHandler{Token: cfg.PairingToken, Fingerprint: fingerprint})
		slog.Info("tls: ready", "fingerprint_sha256", fingerprint)
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		slog.Error("listen", "err", err, "addr", cfg.Addr)
		os.Exit(1)
	}
	go func() {
		if err := serve(srv, ln, tlsConfig); err != nil {
			slog.Error("serve", "err", err)
			os.Exit(1)
		}
	}()
	slog.Info("listening", "addr", cfg.Addr, "https", tlsConfig != nil)

	// Also answer on the helper's own tailnet address, where the app
	// sends helper traffic when it routes over Tailscale. Joining the
	// tailnet doesn't do this by itself.
	if onTailnet {
		if tsLn, err := tsServer.Listen("tcp", tailnetAddr); err != nil {
			slog.Warn("tsnet: listen failed; helper unreachable at its tailnet address", "err", err)
		} else {
			go func() {
				if err := serve(srv, tsLn, tlsConfig); err != nil {
					slog.Warn("tsnet: stopped serving", "err", err)
				}
			}()
			slog.Info("listening on tailnet", "addr", tailnetAddr)
		}
	}

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// A booked run under way gets the same time to finish; one that
	// doesn't shows as interrupted when the helper starts again.
	runsDone := make(chan struct{})
	go func() {
		if scheduler != nil {
			scheduler.Wait()
		}
		close(runsDone)
	}()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "err", err)
	}
	select {
	case <-runsDone:
	case <-shutdownCtx.Done():
		slog.Warn("shutdown: a booked run was still going")
	}
}

// tailnetAddr is where the helper answers on its own tailnet address. Fixed
// rather than following ZEPHYR_ADDR, because it's the port the app dials.
const tailnetAddr = ":8080"

// serve runs srv on ln until shutdown, splitting off HTTPS when there's a
// certificate. It returns nil once the server is shut down.
func serve(srv *http.Server, ln net.Listener, tlsConfig *tls.Config) error {
	if tlsConfig != nil {
		ln = tlsmux.New(ln, tlsConfig, tlsmux.DefaultSniffTimeout)
	}
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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

// modelLabel names the model that writes check-ins, for their card, and
// says whether it's hosted, so runs on the user's API key.
func modelLabel(p llm.Provider) (string, bool) {
	switch p.Name() {
	case llm.ProviderAnthropic:
		return "Claude (" + p.Model() + ")", true
	case llm.ProviderOpenAI:
		return "ChatGPT (" + p.Model() + ")", true
	case llm.ProviderCustom:
		return "your custom server (" + p.Model() + ")", true
	}
	return "the model on your NAS (" + p.Model() + ")", false
}
