package schedule

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Handler serves what's booked to the app's Scheduled screen: the list,
// and pause, resume, run now and remove. Nothing is booked here; that
// takes a card approved in a chat.
type Handler struct {
	Store     *Store
	Scheduler *Scheduler
}

// RegisterMux adds the routes, each wrapped (in auth).
func (h *Handler) RegisterMux(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /schedules", wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /schedules/{id}/pause", wrap(http.HandlerFunc(h.pause)))
	mux.Handle("POST /schedules/{id}/resume", wrap(http.HandlerFunc(h.resume)))
	mux.Handle("POST /schedules/{id}/run", wrap(http.HandlerFunc(h.runNow)))
	mux.Handle("DELETE /schedules/{id}", wrap(http.HandlerFunc(h.remove)))
}

// jobView is a job as the app shows it.
type jobView struct {
	ID       string `json:"id"`
	Kind     Kind   `json:"kind"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Risk     string `json:"risk"`
	Repeats  bool   `json:"repeats"`
	Schedule string `json:"schedule"`
	// Zone is the time zone Schedule is in: the NAS's when it was booked.
	Zone        string     `json:"zone"`
	NextRun     *time.Time `json:"next_run,omitempty"`
	Paused      bool       `json:"paused"`
	Running     bool       `json:"running"`
	LastRun     *time.Time `json:"last_run,omitempty"`
	LastStatus  string     `json:"last_status,omitempty"`
	LastMessage string     `json:"last_message,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (h *Handler) view(job Job) jobView {
	running := false
	if h.Scheduler != nil {
		h.Scheduler.mu.Lock()
		running = h.Scheduler.running[job.ID]
		h.Scheduler.mu.Unlock()
	}
	trunc := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		v := t.UTC().Truncate(time.Second)
		return &v
	}
	return jobView{
		ID: job.ID, Kind: job.Kind, Title: job.Title, Detail: job.Detail, Risk: job.Risk,
		Repeats: job.When.Repeat != "once", Schedule: job.When.Phrase(), Zone: job.When.Zone,
		NextRun: trunc(job.NextRun), Paused: job.Paused, Running: running,
		LastRun: trunc(job.LastRun), LastStatus: job.LastStatus, LastMessage: job.LastMessage,
		CreatedAt: job.CreatedAt.UTC().Truncate(time.Second),
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The helper couldn't open its schedule."})
		return
	}
	jobs, err := h.Store.List(r.Context())
	if err != nil {
		slog.Error("schedule: couldn't list", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "The helper couldn't read its schedule."})
		return
	}
	views := make([]jobView, 0, len(jobs))
	for _, job := range jobs {
		views = append(views, h.view(job))
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": views})
}

func (h *Handler) pause(w http.ResponseWriter, r *http.Request) {
	job, ok := h.find(w, r)
	if !ok {
		return
	}
	if err := h.Store.SetPaused(r.Context(), job.ID, true, job.NextRun); err != nil {
		h.failed(w, err)
		return
	}
	job.Paused = true
	writeJSON(w, http.StatusOK, h.view(job))
}

// resume carries on from the next time due: a repeating job skips the
// runs it missed while paused; a one-off whose time passed can't resume.
func (h *Handler) resume(w http.ResponseWriter, r *http.Request) {
	job, ok := h.find(w, r)
	if !ok {
		return
	}
	next, due := job.When.Next(time.Now())
	if !due {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Its time has passed. Book it again in a chat."})
		return
	}
	if err := h.Store.SetPaused(r.Context(), job.ID, false, &next); err != nil {
		h.failed(w, err)
		return
	}
	job.Paused, job.NextRun = false, &next
	writeJSON(w, http.StatusOK, h.view(job))
}

func (h *Handler) runNow(w http.ResponseWriter, r *http.Request) {
	job, ok := h.find(w, r)
	if !ok {
		return
	}
	if h.Scheduler == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The scheduler isn't running."})
		return
	}
	if err := h.Scheduler.RunNow(r.Context(), job.ID); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, h.view(job))
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The helper couldn't open its schedule."})
		return
	}
	if err := h.Store.Delete(r.Context(), r.PathValue("id")); err != nil {
		h.failed(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) find(w http.ResponseWriter, r *http.Request) (Job, bool) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The helper couldn't open its schedule."})
		return Job{}, false
	}
	job, err := h.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.failed(w, err)
		return Job{}, false
	}
	return job, true
}

func (h *Handler) failed(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "It's no longer booked."})
		return
	}
	slog.Error("schedule: request failed", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "The helper couldn't change its schedule."})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
