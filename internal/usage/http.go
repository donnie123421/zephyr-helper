package usage

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Handler serves the counts at GET /llm/usage. With no Store (the file
// wouldn't open) it answers 503, so the app can tell that from an older
// helper that has no such route.
type Handler struct {
	Store *Store
}

// RegisterMux mounts the endpoint, wrapped by wrap (bearer auth).
func (h *Handler) RegisterMux(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /llm/usage", wrap(http.HandlerFunc(h.totals)))
}

type totalView struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	Answers          int64  `json:"answers"`
}

// totals takes ?since= as an RFC 3339 time, so the app can ask from the
// start of the month where the user is. Without it, the month so far in
// UTC.
func (h *Handler) totals(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "The helper couldn't open its token counts. Restart it, and check that its data folder is writable.",
		})
		return
	}
	since := startOfMonth(time.Now().UTC())
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be an RFC 3339 time."})
			return
		}
		since = parsed
	}
	totals, err := h.Store.Since(r.Context(), since)
	if err != nil {
		slog.Error("usage: couldn't read the counts", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "The helper couldn't read its token counts."})
		return
	}
	views := make([]totalView, 0, len(totals))
	for _, t := range totals {
		views = append(views, totalView{
			Provider:         t.Provider,
			Model:            t.Model,
			InputTokens:      t.Input,
			OutputTokens:     t.Output,
			CacheReadTokens:  t.CacheRead,
			CacheWriteTokens: t.CacheWrite,
			Answers:          t.Answers,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"since":  since.Truncate(time.Second).Format(time.RFC3339),
		"models": views,
	})
}

func startOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
