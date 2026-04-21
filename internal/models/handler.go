// Package models implements the /model endpoints that let the iOS picker
// query and swap the active Ollama model at runtime without a helper restart.
//
// Persistence is intentionally client-side: iOS remembers the user's choice
// and re-applies it on reconnect if the helper reports a different model (e.g.
// after a container restart reset us back to the compose env default). This
// keeps the helper's compose config volume-free.
package models

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/donnie123421/zephyr-helper/internal/ollama"
)

type Handler struct {
	ollama *ollama.Client
}

func NewHandler(o *ollama.Client) *Handler {
	return &Handler{ollama: o}
}

type getResponse struct {
	Model string `json:"model"`
}

type setRequest struct {
	Model string `json:"model"`
}

func (h *Handler) HandleGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, getResponse{Model: h.ollama.Model()})
}

func (h *Handler) HandleSet(w http.ResponseWriter, r *http.Request) {
	var req setRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		http.Error(w, "model is required", http.StatusBadRequest)
		return
	}
	// Guard against obviously malformed names early — Ollama will still enforce
	// its own rules downstream, but a whitespace-containing "model" is almost
	// certainly a client bug.
	if strings.ContainsAny(model, " \t\n\r") {
		http.Error(w, "model name must not contain whitespace", http.StatusBadRequest)
		return
	}

	previous := h.ollama.Model()
	h.ollama.SetModel(model)
	slog.Info("model: switched via POST /model", "from", previous, "to", model)

	writeJSON(w, http.StatusOK, getResponse{Model: model})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
