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
}

// RegisterMux adds the routes, each wrapped (in auth).
func (h *Handler) RegisterMux(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /digest/settings", wrap(http.HandlerFunc(h.getSettings)))
	mux.Handle("PUT /digest/settings", wrap(http.HandlerFunc(h.putSettings)))
	mux.Handle("GET /digest/latest", wrap(http.HandlerFunc(h.latest)))
	mux.Handle("GET /digests", wrap(http.HandlerFunc(h.list)))
	mux.Handle("GET /digests/{id}", wrap(http.HandlerFunc(h.get)))
	mux.Handle("POST /digest/preview", wrap(http.HandlerFunc(h.preview)))
}

func (h *Handler) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.Settings.Get())
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
	writeJSON(w, http.StatusOK, saved)
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
	d.AppVersions = nil
	writeJSON(w, http.StatusOK, d)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
