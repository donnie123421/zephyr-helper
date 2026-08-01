// Package push forwards critical events from the local events.Store to
// the zephyr-push-relay Cloudflare Worker, which proxies them to
// OneSignal → APNs → the user's iPhone.
//
// The relay holds the OneSignal REST API key on our behalf. Helpers
// only carry a per-iPhone (subscription_id, install_token) pair which
// the relay's allowlist scopes to "you can push to exactly this
// device." A leaked helper from someone else's NAS can't push to ours.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

// pushQueueDepth bounds the backlog the notifier holds while the relay is
// slow or unreachable. 256 is many minutes of headroom at any plausible NAS
// event rate, and caps what a wedged relay can cost us in memory. Past it we
// drop with a warning rather than grow without limit.
const pushQueueDepth = 256

// Notifier subscribes to an events.Store and POSTs push-worthy events
// to the relay. Safe to run as a single long-lived goroutine; relies
// on Store.Subscribe's fan-out, so it doesn't poll.
type Notifier struct {
	store          *events.Store
	relayURL       string
	subscriptionID string
	installToken   string
	// prefs holds the iOS-synced muted categories; nil means "nothing
	// muted" (manual deploys that never wire a store).
	prefs      *PrefsStore
	httpClient *http.Client
	log        *slog.Logger

	// dedupeWindow is how long we suppress repeat pushes for the same
	// event id. The events.Store re-broadcasts on every merge (poller
	// re-sees an unresolved alert every 2 minutes), so without this we'd
	// spam the iPhone. 6 hours is roughly "long enough that a re-fire
	// after that window probably means the situation got worse."
	dedupeWindow time.Duration

	mu         sync.Mutex
	lastPushed map[string]time.Time // event id → last push timestamp
}

// New builds a Notifier. The caller must call Run to actually start
// processing — leaves wiring decisions (start now vs later) to the
// composition root.
func New(
	store *events.Store,
	relayURL, subscriptionID, installToken string,
	prefs *PrefsStore,
	log *slog.Logger,
) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{
		store:          store,
		relayURL:       relayURL,
		subscriptionID: subscriptionID,
		installToken:   installToken,
		prefs:          prefs,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		log:          log.With("component", "push"),
		dedupeWindow: 6 * time.Hour,
		lastPushed:   make(map[string]time.Time),
	}
}

// Run subscribes to the events.Store and forwards qualifying events to
// the relay. Blocks until ctx is cancelled. Safe to spawn as a
// goroutine — internal panics are caught and logged so a single bad
// event can't tear the helper down.
func (n *Notifier) Run(ctx context.Context) {
	if n.relayURL == "" || n.subscriptionID == "" || n.installToken == "" {
		n.log.Info("notifier disabled (missing config)")
		return
	}

	// Rehydrate dedupe state from the store so a restart doesn't
	// re-push every unresolved alert the pollers re-ingest on boot.
	// Best-effort: an empty map just means we risk one duplicate per
	// live alert, which is the pre-persistence behavior.
	if recent, err := n.store.RecentPushes(ctx, time.Now().Add(-n.dedupeWindow)); err != nil {
		n.log.Warn("dedupe rehydrate failed", "err", err)
	} else if len(recent) > 0 {
		n.mu.Lock()
		for id, t := range recent {
			n.lastPushed[id] = t
		}
		n.mu.Unlock()
		n.log.Info("dedupe rehydrated", "entries", len(recent))
	}

	feed, cancel := n.store.Subscribe(64)
	defer cancel()

	// Delivery runs on its own goroutine. A send can take ~35s in the worst
	// case (two 15s attempts either side of a 5s backoff), and while this
	// loop was blocked inside handle() it wasn't draining `feed` — so
	// Store.broadcast, which drops frames for a backed-up subscriber rather
	// than blocking every other writer, silently threw away pushes during
	// exactly the relay outage you'd most want to be told about. Receiving
	// and sending are now decoupled: this loop only classifies and enqueues.
	queue := make(chan events.Event, pushQueueDepth)
	workerDone := make(chan struct{})
	go n.deliverLoop(ctx, queue, workerDone)
	defer func() {
		close(queue)
		<-workerDone
	}()

	// Periodic prune of the dedupe map — keeps it bounded on
	// long-running helpers without an LRU dependency. 1h cadence
	// means at most ~6h × event_rate entries hang around between
	// prunes; fine for our scale.
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()

	n.log.Info("notifier started", "relay", n.relayURL)

	for {
		select {
		case <-ctx.Done():
			n.log.Info("notifier stopping")
			return
		case <-prune.C:
			n.pruneDedupe()
		case ev, ok := <-feed:
			if !ok {
				return
			}
			// Cheap, event-intrinsic filter here so the queue only holds
			// push-worthy events. Time-dependent policy (mutes, quiet hours,
			// dedupe) stays in handle() where it's evaluated at send time,
			// not enqueue time — un-muting while a backlog drains should
			// take effect.
			if !shouldPush(ev) {
				continue
			}
			select {
			case queue <- ev:
			default:
				// Queue full: the relay has been unreachable long enough to
				// back up pushQueueDepth events. Drop loudly — a silent drop
				// here is what this whole change exists to prevent.
				n.log.Warn("push queue full, dropping event",
					"event_id", ev.ID, "kind", ev.Kind, "severity", ev.Severity,
					"queue_depth", pushQueueDepth)
			}
		}
	}
}

// deliverLoop owns every outbound push. Single goroutine on purpose: sends
// stay serialized, so the dedupe map and the relay's per-subscription rate
// budget behave exactly as they did when delivery was inline.
//
// Ranges to completion so a shutdown drains whatever is already queued; those
// sends fail fast because ctx is already cancelled.
func (n *Notifier) deliverLoop(ctx context.Context, queue <-chan events.Event, done chan<- struct{}) {
	defer close(done)
	for ev := range queue {
		n.handle(ctx, ev)
	}
}

func (n *Notifier) handle(ctx context.Context, ev events.Event) {
	defer func() {
		if r := recover(); r != nil {
			n.log.Error("notifier panic recovered",
				"recover", fmt.Sprint(r), "event_id", ev.ID)
		}
	}()

	// shouldPush already ran at enqueue time — this is the time-dependent
	// half of the policy, deliberately evaluated now rather than then so a
	// backlog draining after an outage reflects current mutes and window.
	//
	// Mute + quiet-hours checks run before the dedupe mark on purpose:
	// a suppressed period must not record dedupe entries, so un-muting
	// (or the window ending) lets the very next re-broadcast of a
	// still-live event push normally.
	if n.prefs != nil {
		if category := categoryFor(ev); n.prefs.IsMuted(category) {
			n.log.Debug("push muted by prefs",
				"event_id", ev.ID, "category", category)
			return
		}
	}
	quiet := n.prefs != nil && n.prefs.InQuietHours(time.Now())
	if quiet && ev.Severity != events.SeverityCritical {
		// Warnings can wait for morning; they stay in the in-app feed.
		// Criticals still go out below, delivered passively.
		n.log.Debug("push deferred by quiet hours", "event_id", ev.ID)
		return
	}
	if !n.markIfFresh(ev.ID) {
		// Suppressed — same event re-broadcast from a store merge
		// within the dedupe window.
		return
	}

	payload := buildPayload(ev, quiet)
	if err := n.send(ctx, payload); err != nil {
		n.log.Warn("push send failed",
			"err", err, "event_id", ev.ID, "kind", ev.Kind)
		// Forget the dedupe entry so a retry on the next merge can
		// succeed. We'd rather risk a duplicate than silently drop.
		n.forget(ev.ID)
	} else {
		n.log.Debug("push sent",
			"event_id", ev.ID, "kind", ev.Kind, "severity", ev.Severity)
		// Persist so the dedupe survives a restart. Failure is
		// non-fatal — worst case is one duplicate after a restart.
		if err := n.store.RecordPush(ctx, ev.ID, time.Now().UTC()); err != nil {
			n.log.Warn("push record failed", "err", err, "event_id", ev.ID)
		}
	}
}

// shouldPush is the policy: which events are worth waking the user's
// phone for? Conservative on purpose — anything user-visible but
// non-urgent stays in the in-app feed only.
func shouldPush(ev events.Event) bool {
	// Critical of any kind always pushes.
	if ev.Severity == events.SeverityCritical {
		return true
	}
	// Warning is push-worthy only for hardware / pool integrity kinds.
	// Warning-level alerts and job failures stay feed-only — they're
	// frequent enough that pushing each one trains the user to ignore
	// the badge.
	if ev.Severity == events.SeverityWarning {
		switch ev.Kind {
		case events.KindPool, events.KindDisk, events.KindResilver, events.KindScrub:
			return true
		}
	}
	return false
}

// buildPayload maps an event into the relay's push payload shape. The
// category aligns with the UNNotificationCategory ids iOS registers in
// NotificationManager, so iOS can render with the right action set + sound.
func buildPayload(ev events.Event, quiet bool) pushPayload {
	category := categoryFor(ev)
	// ThreadID groups related notifications in iOS Notification Center.
	// Per-kind threading is cheap and gives reasonable visual grouping
	// (all pool events for one pool stack, etc.). Refinement later: hash
	// per-pool, per-app, etc.
	thread := string(ev.Kind)
	return pushPayload{
		SubscriptionID: "", // filled by Notifier.send
		InstallToken:   "",
		Payload: payloadBody{
			Title:             ev.Title,
			Body:              ev.Summary,
			Category:          category,
			ThreadID:          thread,
			InterruptionLevel: interruptionLevelFor(ev, quiet),
			Data: map[string]any{
				"event_id": ev.ID,
				"kind":     string(ev.Kind),
				"severity": string(ev.Severity),
			},
		},
	}
}

// interruptionLevelFor picks the APNs interruption level. Criticals are
// time-sensitive (break through Focus modes — a dead pool shouldn't wait
// behind a work Focus), demoted to passive during quiet hours (listed in
// Notification Center, no wake or sound). Everything else keeps the
// default level, expressed as empty so the relay omits the field.
func interruptionLevelFor(ev events.Event, quiet bool) string {
	if ev.Severity != events.SeverityCritical {
		return ""
	}
	if quiet {
		return "passive"
	}
	return "time-sensitive"
}

func categoryFor(ev events.Event) string {
	if ev.Severity == events.SeverityCritical {
		switch ev.Kind {
		case events.KindPool:
			return "POOL_DEGRADED"
		case events.KindSecurity:
			return "SECURITY_CRITICAL"
		default:
			return "ALERT_CRITICAL"
		}
	}
	return "ALERT_WARNING"
}

// send POSTs to the relay's /push endpoint with one retry on transient
// failure (5xx or network). 4xx and 429 are treated as terminal — the
// notifier logs and gives up rather than burning quota.
func (n *Notifier) send(ctx context.Context, payload pushPayload) error {
	payload.SubscriptionID = n.subscriptionID
	payload.InstallToken = n.installToken

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	url := n.relayURL + "/push"

	// Two attempts: one immediate, one after 5s. Anything beyond is
	// likely the relay being down or OneSignal upstream issues — drop
	// rather than queue indefinitely.
	const maxAttempts = 2
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := n.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("transport: %w", err)
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests:
			// Rate limited. Don't retry; the per-subscription daily
			// budget is enforced by the relay (200/day). Just stop.
			retry, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			return fmt.Errorf("rate limited (retry after %ds): %s",
				retry, string(respBody))
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			// Config error (bad token, unknown subscription, malformed
			// payload). Retrying won't help — log and drop.
			return fmt.Errorf("relay rejected (%d): %s",
				resp.StatusCode, string(respBody))
		default:
			lastErr = fmt.Errorf("relay %d: %s",
				resp.StatusCode, string(respBody))
		}
	}
	if lastErr == nil {
		return errors.New("push failed after retries")
	}
	return lastErr
}

// markIfFresh returns true if we haven't pushed this event id within
// the dedupe window. Records the timestamp atomically with the check
// so two near-simultaneous broadcasts of the same id can't both push.
func (n *Notifier) markIfFresh(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.lastPushed[id]; ok {
		if time.Since(last) < n.dedupeWindow {
			return false
		}
	}
	n.lastPushed[id] = time.Now()
	return true
}

func (n *Notifier) forget(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.lastPushed, id)
}

func (n *Notifier) pruneDedupe() {
	cutoff := time.Now().Add(-n.dedupeWindow)
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, t := range n.lastPushed {
		if t.Before(cutoff) {
			delete(n.lastPushed, id)
		}
	}
}

// Wire format mirrors the relay's /push expected body.
type pushPayload struct {
	SubscriptionID string      `json:"subscriptionId"`
	InstallToken   string      `json:"installToken"`
	Payload        payloadBody `json:"payload"`
}

type payloadBody struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Category string `json:"category,omitempty"`
	ThreadID string `json:"threadId,omitempty"`
	// InterruptionLevel is the APNs level ("time-sensitive", "passive",
	// "active") the relay maps to OneSignal's ios_interruption_level.
	// Empty = platform default.
	InterruptionLevel string         `json:"interruptionLevel,omitempty"`
	Data              map[string]any `json:"data,omitempty"`
}
