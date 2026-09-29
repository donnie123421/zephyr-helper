package conversations

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Handler serves the saved conversations under /conversations. With no
// Store (the file wouldn't open) every route answers 503, so the app can
// tell that from an older helper that has no such routes.
type Handler struct {
	Store *Store
}

// RegisterMux mounts the endpoints, each wrapped by wrap (bearer auth).
func (h *Handler) RegisterMux(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	route := func(pattern string, handle http.HandlerFunc) {
		mux.Handle(pattern, wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h.Store == nil {
				writeError(w, http.StatusServiceUnavailable, Unavailable)
				return
			}
			handle(w, r)
		})))
	}
	route("GET /conversations", h.list)
	route("DELETE /conversations", h.deleteAll)
	route("GET /conversations/{id}", h.get)
	route("PATCH /conversations/{id}", h.rename)
	route("DELETE /conversations/{id}", h.delete)
}

// Unavailable is what the app is told when the helper can't open its saved
// conversations.
const Unavailable = "The helper couldn't open its saved conversations. Restart it, and check that its data folder is writable."

// Times go out in whole seconds, which is what the app's date decoding
// expects.

type summaryView struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	MessageCount int       `json:"message_count"`
	Provider     string    `json:"provider,omitempty"`
	Model        string    `json:"model,omitempty"`
}

type messageView struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Tools     []string  `json:"tools"`
	CreatedAt time.Time `json:"created_at"`
}

type conversationView struct {
	summaryView
	InputTokens  int64         `json:"input_tokens"`
	OutputTokens int64         `json:"output_tokens"`
	Messages     []messageView `json:"messages"`
}

func viewOfSummary(s Summary) summaryView {
	return summaryView{
		ID:           s.ID,
		Title:        s.Title,
		CreatedAt:    s.CreatedAt.Truncate(time.Second),
		UpdatedAt:    s.UpdatedAt.Truncate(time.Second),
		MessageCount: s.MessageCount,
		Provider:     s.Provider,
		Model:        s.Model,
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	summaries, err := h.Store.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	views := make([]summaryView, 0, len(summaries))
	for _, s := range summaries {
		views = append(views, viewOfSummary(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": views})
}

// get leaves out hidden context: the app doesn't show it, and it can be
// long. The model still gets it when the chat is reopened.
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	c, err := h.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	view := conversationView{
		summaryView:  viewOfSummary(c.Summary),
		InputTokens:  c.InputTokens,
		OutputTokens: c.OutputTokens,
		Messages:     []messageView{},
	}
	for _, m := range c.Messages {
		if m.Hidden {
			continue
		}
		view.Messages = append(view.Messages, messageView{
			Role:      m.Role,
			Content:   m.Content,
			Tools:     m.Tools,
			CreatedAt: m.CreatedAt.Truncate(time.Second),
		})
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "The request body isn't valid JSON.")
		return
	}
	if CleanTitle(req.Title) == "" {
		writeError(w, http.StatusBadRequest, "Give the conversation a name.")
		return
	}
	summary, err := h.Store.Rename(r.Context(), r.PathValue("id"), req.Title)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewOfSummary(summary))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.Delete(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteAll(w http.ResponseWriter, r *http.Request) {
	n, err := h.Store.DeleteAll(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	slog.Info("conversations: deleted all", "count", n)
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": n})
}

// fail answers with the error. A missing conversation gets a JSON 404, so
// the app can tell it from an older helper that has no such route.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "This conversation has been deleted.")
		return
	}
	slog.Error("conversations: store failed", "err", err)
	writeError(w, http.StatusInternalServerError, "The helper couldn't read its saved conversations.")
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
