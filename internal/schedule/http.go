package schedule

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/tools"
)

// Handler serves what's booked to the app's Scheduled screen: the list,
// and pause, resume, run now and remove. It books and edits too, through
// the same tools a chat does: the app asks for the card (preview), shows
// it, and books it only as it was shown.
type Handler struct {
	Store     *Store
	Scheduler *Scheduler
	// Booking is nil when the helper can't book: it isn't connected to
	// TrueNAS.
	Booking *Booking
}

// RegisterMux adds the routes, each wrapped (in auth).
func (h *Handler) RegisterMux(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /schedules", wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /schedules/{id}/pause", wrap(http.HandlerFunc(h.pause)))
	mux.Handle("POST /schedules/{id}/resume", wrap(http.HandlerFunc(h.resume)))
	mux.Handle("POST /schedules/{id}/run", wrap(http.HandlerFunc(h.runNow)))
	mux.Handle("DELETE /schedules/{id}", wrap(http.HandlerFunc(h.remove)))
	mux.Handle("POST /schedules/preview", wrap(http.HandlerFunc(h.preview)))
	mux.Handle("POST /schedules", wrap(http.HandlerFunc(h.create)))
	mux.Handle("PUT /schedules/{id}", wrap(http.HandlerFunc(h.edit)))
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
	Zone    string     `json:"zone"`
	NextRun *time.Time `json:"next_run,omitempty"`
	Paused  bool       `json:"paused"`
	// Held: paused because Zephyr Pro has lapsed, until it's back.
	Held        bool       `json:"held"`
	Running     bool       `json:"running"`
	LastRun     *time.Time `json:"last_run,omitempty"`
	LastStatus  string     `json:"last_status,omitempty"`
	LastMessage string     `json:"last_message,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	// Tool, Arguments and When are what it does and when, for the app's
	// editor.
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	When      When            `json:"when"`
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
		NextRun: trunc(job.NextRun), Paused: job.Paused, Held: job.Held, Running: running,
		LastRun: trunc(job.LastRun), LastStatus: job.LastStatus, LastMessage: job.LastMessage,
		CreatedAt: job.CreatedAt.UTC().Truncate(time.Second),
		Tool:      job.Tool, Arguments: job.Arguments, When: job.When,
	}
}

// bookingRequest is what the app asks to book: one of the booking tools,
// with what a chat would send it. Title and Detail are the card the app
// showed, for create and edit: it's booked only if the card is the same
// now.
type bookingRequest struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Title     string          `json:"title"`
	Detail    string          `json:"detail"`
}

// cardView is a booking's card as the app shows it, with the NAS's time
// zone, which its times are in, and its first run.
type cardView struct {
	Title    string     `json:"title"`
	Detail   string     `json:"detail"`
	Risk     string     `json:"risk"`
	Zone     string     `json:"zone"`
	FirstRun *time.Time `json:"first_run,omitempty"`
}

// card reads the request and writes its card, or answers why it can't.
func (h *Handler) card(w http.ResponseWriter, r *http.Request) (bookingRequest, tools.Card, Kind, cardView, bool) {
	var req bookingRequest
	if h.Booking == nil || h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The helper can't book anything: it isn't connected to TrueNAS."})
		return req, tools.Card{}, "", cardView{}, false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.Tool == "" || len(req.Arguments) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Send tool and arguments."})
		return req, tools.Card{}, "", cardView{}, false
	}
	b := *h.Booking
	if b.Now == nil {
		b.Now = time.Now
	}
	card, kind, err := b.describe(r.Context(), req.Tool, req.Arguments)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": forPeople(err)})
		return req, tools.Card{}, "", cardView{}, false
	}
	view := cardView{Title: card.Title, Detail: card.Detail, Risk: string(card.Risk)}
	if job, err := b.jobFor(card.Args, kind); err == nil {
		view.Zone, view.FirstRun = job.When.Zone, job.NextRun
	}
	return req, card, kind, view, true
}

// forPeople is a booking tool's refusal for the app: the tools write for
// the model, and point it at the tool that lists what there is ("; call
// list_apps for their names"), which isn't for people.
func forPeople(err error) string {
	text := err.Error()
	if i := strings.Index(text, "; call "); i >= 0 {
		text = text[:i]
	}
	return upperFirst(strings.TrimSuffix(text, ".")) + "."
}

// preview is the card a booking would be approved on.
func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	if _, _, _, view, ok := h.card(w, r); ok {
		writeJSON(w, http.StatusOK, view)
	}
}

// shown says whether the card is the one the app showed. It's 409 with
// the card as it is now when it isn't: the NAS, or the day, changed.
func shown(w http.ResponseWriter, req bookingRequest, view cardView) bool {
	if req.Title == view.Title && req.Detail == view.Detail {
		return true
	}
	writeJSON(w, http.StatusConflict, map[string]any{"error": "It's changed since you looked. Check it again.", "card": view})
	return false
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	req, card, kind, view, ok := h.card(w, r)
	if !ok || !shown(w, req, view) {
		return
	}
	b := *h.Booking
	if b.Now == nil {
		b.Now = time.Now
	}
	job, err := b.bookJob(r.Context(), card.Args, kind)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": forPeople(err)})
		return
	}
	writeJSON(w, http.StatusCreated, h.view(job))
}

// edit replaces what a booking does and when, approved on its new card.
func (h *Handler) edit(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The helper couldn't open its schedule."})
		return
	}
	old, err := h.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.failed(w, err)
		return
	}
	// Done with, it's not brought back: that's booking it again.
	if old.NextRun == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "It's already done. Book it again instead."})
		return
	}
	req, card, kind, view, ok := h.card(w, r)
	if !ok || !shown(w, req, view) {
		return
	}
	b := *h.Booking
	if b.Now == nil {
		b.Now = time.Now
	}
	job, err := b.jobFor(card.Args, kind)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": forPeople(err)})
		return
	}
	if err := h.Store.Replace(r.Context(), old.ID, job); err != nil {
		h.failed(w, err)
		return
	}
	edited, err := h.Store.Get(r.Context(), old.ID)
	if err != nil {
		h.failed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.view(edited))
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
	if err := h.Store.Pause(r.Context(), job.ID); err != nil {
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
	if err := h.Store.Resume(r.Context(), job.ID, next); err != nil {
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
