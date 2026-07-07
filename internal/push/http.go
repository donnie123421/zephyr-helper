package push

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// PrefsHandler exposes the notification preferences to the iOS app.
// Thin like the events handlers: validate, delegate to the store,
// serialize. Auth is wrapped by the caller (same bearer middleware as
// every other stateful route).
type PrefsHandler struct {
	Store *PrefsStore
}

// RegisterMux wires GET/POST /push/prefs onto `mux`.
func (h *PrefsHandler) RegisterMux(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /push/prefs", wrap(http.HandlerFunc(h.Get)))
	mux.Handle("POST /push/prefs", wrap(http.HandlerFunc(h.Set)))
}

func (h *PrefsHandler) Get(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"muted_categories": h.Store.Muted(),
	})
}

func (h *PrefsHandler) Set(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MutedCategories []string `json:"muted_categories"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if len(body.MutedCategories) > MaxMutedCategories {
		http.Error(w, "too many categories", http.StatusBadRequest)
		return
	}
	for _, c := range body.MutedCategories {
		// Unknown category names are accepted on purpose (a newer iOS
		// build may know categories this helper doesn't yet) — only
		// obviously bogus values are rejected.
		if c == "" || len(c) > 64 {
			http.Error(w, "invalid category", http.StatusBadRequest)
			return
		}
	}
	if err := h.Store.SetMuted(body.MutedCategories); err != nil {
		slog.Error("push prefs save", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"muted_categories": h.Store.Muted(),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
