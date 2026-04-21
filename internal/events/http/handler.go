// Package http glues the events store to the HTTP + WebSocket surface
// the iOS client consumes. Handlers are thin — validate query/path,
// delegate to the store, serialize the result.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/donnie123421/zephyr-helper/internal/events"
)

type Handler struct {
	Store *events.Store
}

// RegisterMux wires every /events* route onto `mux`. The caller is
// responsible for wrapping routes with auth middleware; we don't want to
// hard-code that dependency here because the auth package already handles
// it for the rest of the server.
func (h *Handler) RegisterMux(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /events", wrap(http.HandlerFunc(h.List)))
	mux.Handle("GET /events/{id}", wrap(http.HandlerFunc(h.Get)))
	mux.Handle("POST /events/{id}/ack", wrap(http.HandlerFunc(h.Ack)))
	mux.Handle("GET /events/stream", wrap(http.HandlerFunc(h.Stream)))
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var filter events.ListFilter

	if since := q.Get("since"); since != "" {
		// ISO-8601 is the canonical form; also accept unix-millis as a
		// convenience for quick debugging from curl.
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = t
		} else if ms, err := strconv.ParseInt(since, 10, 64); err == nil {
			filter.Since = time.UnixMilli(ms).UTC()
		} else {
			http.Error(w, "invalid since: expected RFC3339 or unix-millis", http.StatusBadRequest)
			return
		}
	}
	if k := q.Get("kind"); k != "" {
		filter.Kind = events.Kind(k)
	}
	if s := q.Get("severity"); s != "" {
		filter.Severity = events.Severity(s)
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		filter.Limit = n
	}

	list, err := h.Store.List(r.Context(), filter)
	if err != nil {
		slog.Error("events list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"events": list,
		"count":  len(list),
	})
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	e, err := h.Store.Get(r.Context(), id)
	if errors.Is(err, events.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("events get", "err", err, "id", id)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (h *Handler) Ack(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	err := h.Store.Ack(r.Context(), id, time.Now().UTC())
	if errors.Is(err, events.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("events ack", "err", err, "id", id)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Stream is a WebSocket that pushes newly-ingested events to the caller.
// Connects and immediately subscribes — the client is expected to have
// already fetched historical events via GET /events. We don't replay
// history over the stream to keep the protocol narrow.
func (h *Handler) Stream(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // bearer auth is enforced upstream
	})
	if err != nil {
		slog.Error("events stream accept", "err", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	ch, unsub := h.Store.Subscribe(32)
	defer unsub()

	// Watch the read side so client disconnects cancel the subscription.
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if err := wsjson.Write(ctx, conn, e); err != nil {
				slog.Info("events stream write failed", "err", err)
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
