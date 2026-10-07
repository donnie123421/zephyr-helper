package digest

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Handler serves the digests and their settings to the app.
type Handler struct {
	Sources  Sources
	Settings *SettingsStore
	// Held says Zephyr Pro has lapsed: the digest is on, but none goes out.
	Held func() bool
}

// settingsView is the settings as the app sees them: with whether the
// digest is on but held, as it is while Zephyr Pro has lapsed.
type settingsView struct {
	Settings
	Held bool `json:"held"`
}

func (h *Handler) view(s Settings) settingsView {
	return settingsView{Settings: s, Held: s.Enabled && h.Held != nil && h.Held()}
}

// RegisterMux adds the routes, each wrapped (in auth). Without its files
// (Settings or Sources.Store nil), every route answers 503 rather than
// going missing, which the app would read as a helper too old for them.
func (h *Handler) RegisterMux(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	route := func(pattern string, f http.HandlerFunc) {
		mux.Handle(pattern, wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h.Settings == nil || h.Sources.Store == nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The helper couldn't open its digest files. Its log says why."})
				return
			}
			f(w, r)
		})))
	}
	route("GET /digest/settings", h.getSettings)
	route("PUT /digest/settings", h.putSettings)
	route("GET /digest/latest", h.latest)
	route("GET /digests", h.list)
	route("GET /digests/{id}", h.get)
	route("POST /digest/preview", h.preview)
}

func (h *Handler) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.view(h.Settings.Get()))
}

func (h *Handler) putSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
		Summary *bool `json:"summary"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.Enabled == nil || body.Summary == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Send enabled and summary."})
		return
	}
	saved, err := h.Settings.Set(Settings{Enabled: *body.Enabled, Summary: *body.Summary}, h.Sources.now())
	if err != nil {
		slog.Error("digest: settings", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "The helper couldn't save that."})
		return
	}
	writeJSON(w, http.StatusOK, h.view(saved))
}

func (h *Handler) latest(w http.ResponseWriter, _ *http.Request) {
	d, ok := h.Sources.Store.Latest()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "There's no digest yet: the first comes on Sunday morning."})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"digests": h.Sources.Store.List()})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	d, ok := h.Sources.Store.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "That digest is no longer kept."})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// preview is the week so far, without the model's summary: a look, not a
// digest to keep.
func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	d, err := h.Sources.Assemble(r.Context(), false)
	if err != nil {
		slog.Warn("digest: preview", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "TrueNAS didn't answer, so there's no preview."})
		return
	}
	d.Preview = true
	d.AppVersions, d.TrueNASVersion = nil, ""
	writeJSON(w, http.StatusOK, d)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
