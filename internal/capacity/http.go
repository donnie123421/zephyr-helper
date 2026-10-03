package capacity

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Handler serves each pool's forecast at GET /capacity.
type Handler struct {
	Store *Store
	Now   func() time.Time
}

// RegisterMux adds the route, wrapped (in auth).
func (h *Handler) RegisterMux(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /capacity", wrap(http.HandlerFunc(h.forecasts)))
}

// Forecasts is every pool recorded in the last week, with its forecast.
func (h *Handler) Forecasts(r *http.Request) ([]Forecast, error) {
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	pools, err := h.Store.Pools(r.Context(), now.AddDate(0, 0, -7))
	if err != nil {
		return nil, err
	}
	out := make([]Forecast, 0, len(pools))
	for _, pool := range pools {
		history, err := h.Store.History(r.Context(), pool, now.AddDate(0, 0, -forecastWindow))
		if err != nil {
			return nil, err
		}
		out = append(out, ForecastFrom(pool, history, now))
	}
	return out, nil
}

func (h *Handler) forecasts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.Store == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "The helper couldn't open its capacity history."})
		return
	}
	forecasts, err := h.Forecasts(r)
	if err != nil {
		slog.Error("capacity: forecasts", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "The helper couldn't read its capacity history."})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"pools": forecasts, "min_days": MinDays})
}
