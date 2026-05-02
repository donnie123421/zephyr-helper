// Package remoteaccess answers `GET /remote-access` so the iOS app
// can prefill the "Add Tailscale endpoint" sheet without making the
// user copy/paste the tailnet hostname out of the Tailscale admin
// console.
//
// Detection is best-effort: we ask TrueNAS for the tailscale app's
// install state and scrape its compose env for TS_HOSTNAME. If the
// user customised the hostname we surface it; if not, we still tell
// the iOS app the app is installed so it can show a friendlier
// "found it, now paste the hostname" UX.
package remoteaccess

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/tailnet"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// Handler serves the read-only `GET /remote-access` endpoint.
type Handler struct {
	TN      *truenas.Client
	// Tailnet is optional. When the helper joined the tailnet via
	// tsnet at startup, the handler upgrades the response with the
	// authoritative MagicDNS hostname instead of guessing.
	Tailnet *tailnet.Server
	Log     *slog.Logger
}

// Response is the outer envelope returned to the iOS app. New
// transports (Cloudflare Tunnel, DDNS) will land as additional
// pointer fields here as the helper learns to introspect them.
type Response struct {
	Tailscale *Tailscale `json:"tailscale,omitempty"`
	// NASHostname is the TrueNAS system hostname. Tailscale defaults
	// its machine name to the OS hostname when no TS_HOSTNAME env
	// override is set in the chart, so iOS can suggest
	// `<nasHostname>.<tailnet>.ts.net` as a prefill candidate even
	// when the chart values don't expose anything Tailscale-specific.
	NASHostname string `json:"nasHostname,omitempty"`
}

// Tailscale carries everything the iOS prefill flow needs about the
// installed-on-NAS Tailscale instance.
type Tailscale struct {
	Installed bool   `json:"installed"`
	State     string `json:"state,omitempty"`    // RUNNING / STOPPED / DEPLOYING / etc.
	Hostname  string `json:"hostname,omitempty"` // best-effort tailnet hostname
	IPv4      string `json:"ipv4,omitempty"`     // 100.x.y.z when known via tsnet
	// Source labels how confident the hostname is. iOS uses this
	// to choose between "Detected via tailnet" (highest confidence,
	// from the helper's own tsnet status) and "Found in chart"
	// (compose-env scrape, less reliable).
	Source string `json:"source,omitempty"` // "tailnet" / "chart" / ""
	// Human-readable hint when we know it's installed but couldn't
	// pin down the hostname — surfaced in the iOS sheet so the user
	// knows where to look.
	Hint string `json:"hint,omitempty"`
}

// ServeHTTP implements net/http.Handler. The endpoint is gated by
// the same pairing token as every other authenticated route.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Cap the introspection at 5s — the iOS sheet doesn't want to
	// block any longer, and a slow TrueNAS shouldn't stall the user.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	ts := h.detectTailscale(ctx)
	nasHostname := h.fetchNASHostname(ctx)

	// If the helper joined the tailnet, ask its local API for the
	// NAS *peer* (not the helper's own Self). The iPhone needs to
	// reach TrueNAS at port 443 — which runs on the NAS host, not
	// inside the helper container — so the only useful tailnet
	// identity here is the one registered by the Tailscale TrueNAS
	// app on the NAS itself.
	//
	// We look up that peer by the NAS's system hostname. When the
	// match succeeds the response is upgraded with the
	// authoritative MagicDNS name + 100.x.y.z IP and labelled
	// `source: "tailnet"` so iOS treats it as high-confidence.
	if h.Tailnet != nil && h.Tailnet.Available() && nasHostname != "" {
		if peer, ok := h.Tailnet.FindPeerByHostname(ctx, nasHostname); ok {
			if ts == nil {
				ts = &Tailscale{Installed: false}
			}
			if peer.DNSName != "" {
				ts.Hostname = peer.DNSName
			} else if peer.HostName != "" {
				ts.Hostname = peer.HostName
			}
			ts.Source = "tailnet"
			if len(peer.TailscaleIPs) > 0 {
				ts.IPv4 = peer.TailscaleIPs[0]
			}
		}
	} else if ts != nil && ts.Hostname != "" {
		// Chart scrape did find something — label it so iOS knows
		// to be slightly less confident than a tsnet result.
		ts.Source = "chart"
	}

	resp := Response{
		Tailscale:   ts,
		NASHostname: nasHostname,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// fetchNASHostname reads /system/general for the NAS's hostname.
// Returns empty string on any failure — this is purely a UX hint, a
// missing hostname just means iOS falls back to the manual paste
// flow it already supports.
func (h *Handler) fetchNASHostname(ctx context.Context) string {
	if h.TN == nil || !h.TN.Configured() {
		return ""
	}
	raw, err := h.TN.GetRaw(ctx, "/system/general")
	if err != nil {
		h.logf("remote-access: system/general failed", "err", err)
		return ""
	}
	var general struct {
		Hostname string `json:"hostname"`
	}
	if err := json.Unmarshal(raw, &general); err != nil {
		// Older builds nest hostname under config — try again.
		var nested struct {
			Config struct {
				Hostname string `json:"hostname"`
			} `json:"config"`
		}
		if err := json.Unmarshal(raw, &nested); err == nil {
			return strings.TrimSpace(nested.Config.Hostname)
		}
		h.logf("remote-access: decode system/general failed", "err", err)
		return ""
	}
	return strings.TrimSpace(general.Hostname)
}

func (h *Handler) detectTailscale(ctx context.Context) *Tailscale {
	if h.TN == nil || !h.TN.Configured() {
		return &Tailscale{Installed: false}
	}

	// TrueNAS exposes installed apps under /app. We scan instead of
	// hitting /app/id/tailscale directly because the chart name varies
	// across catalogs ("tailscale", "tailscale-coordinator", etc.).
	apps, err := h.TN.GetRaw(ctx, "/app")
	if err != nil {
		h.logf("remote-access: list apps failed", "err", err)
		return &Tailscale{Installed: false}
	}

	var entries []appEntry
	if err := json.Unmarshal(apps, &entries); err != nil {
		h.logf("remote-access: decode apps failed", "err", err)
		return &Tailscale{Installed: false}
	}

	app := findTailscaleApp(entries)
	if app == nil {
		return &Tailscale{Installed: false}
	}

	ts := &Tailscale{
		Installed: true,
		State:     app.State,
	}

	// Scrape the compose env for a user-configured hostname. The
	// official Tailscale image honours TS_HOSTNAME; community charts
	// also expose HOSTNAME, MACHINE_NAME, or simply NAME. We try the
	// common keys in priority order.
	if hostname := scrapeHostnameFromConfig(app); hostname != "" {
		ts.Hostname = hostname
	} else {
		ts.Hint = "Tailscale is installed but no hostname is configured in the chart. Find it in the Tailscale admin console under Devices."
	}

	return ts
}

// appEntry mirrors the slice of /app fields we actually care about.
// Everything else in the TrueNAS payload is ignored — keeping the
// shape narrow makes the JSON tolerant to schema drift across
// 24.10 / 25.04 / 25.10 / 26.0.
type appEntry struct {
	Name     string                 `json:"name"`
	State    string                 `json:"state"`
	Metadata map[string]any         `json:"metadata"`
	// Both shapes seen in the wild — `config` on classic charts,
	// `values` on the newer compose-style apps. We probe both.
	Config map[string]any `json:"config"`
	Values map[string]any `json:"values"`
	// Some catalogs surface the running container env under
	// active_workloads.containers[*].environment. Captured loosely
	// for env-scrape fallback.
	ActiveWorkloads map[string]any `json:"active_workloads"`
}

func findTailscaleApp(entries []appEntry) *appEntry {
	for i, e := range entries {
		name := strings.ToLower(e.Name)
		title := lowerString(e.Metadata, "title")
		if name == "tailscale" || strings.Contains(name, "tailscale") {
			return &entries[i]
		}
		if strings.Contains(title, "tailscale") {
			return &entries[i]
		}
	}
	return nil
}

// scrapeHostnameFromConfig walks the chart's user-supplied values
// looking for a hostname. TrueCharts/iX charts use slightly different
// keys; this checks the common ones in order.
func scrapeHostnameFromConfig(app *appEntry) string {
	candidates := []string{
		"hostname",
		"machineName",
		"machine_name",
		"name",
		"TS_HOSTNAME",
	}

	scopes := []map[string]any{app.Values, app.Config}
	for _, scope := range scopes {
		if scope == nil {
			continue
		}
		for _, key := range candidates {
			if v := stringAt(scope, key); v != "" {
				return v
			}
		}
		// Some charts nest under network.* or tailscale.*
		if hostname := stringAt(scope, "network", "hostname"); hostname != "" {
			return hostname
		}
		if hostname := stringAt(scope, "tailscale", "hostname"); hostname != "" {
			return hostname
		}
	}

	// Last resort: scan active container env for TS_HOSTNAME-style
	// vars. The shape is wildly inconsistent across catalogs so we
	// just look for any string value that ends in `.ts.net`.
	if app.ActiveWorkloads != nil {
		if v := scanForTSNetHost(app.ActiveWorkloads); v != "" {
			return v
		}
	}

	return ""
}

func stringAt(m map[string]any, keys ...string) string {
	if len(keys) == 0 {
		return ""
	}
	cur := any(m)
	for _, k := range keys {
		mp, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = mp[k]
		if !ok {
			return ""
		}
	}
	if s, ok := cur.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func lowerString(m map[string]any, keys ...string) string {
	return strings.ToLower(stringAt(m, keys...))
}

// scanForTSNetHost recursively searches a JSON-like map for any
// string that looks like a tailnet hostname. Cheap and effective for
// catalogs whose env shape we don't already know.
func scanForTSNetHost(v any) string {
	switch x := v.(type) {
	case string:
		s := strings.TrimSpace(x)
		if strings.HasSuffix(s, ".ts.net") {
			return s
		}
	case map[string]any:
		for _, val := range x {
			if hit := scanForTSNetHost(val); hit != "" {
				return hit
			}
		}
	case []any:
		for _, val := range x {
			if hit := scanForTSNetHost(val); hit != "" {
				return hit
			}
		}
	}
	return ""
}

func (h *Handler) logf(msg string, args ...any) {
	if h.Log != nil {
		h.Log.Warn(msg, args...)
	}
}

// Sentinel errors kept exported so tests can assert specific failure
// modes once we add them.
var (
	ErrNotConfigured = errors.New("remoteaccess: truenas client not configured")
)
