// Package tools is the tool palette the LLM can call during a chat turn.
//
// Every tool has three pieces:
//   - an Ollama-compatible JSON schema so the model knows when + how to use it,
//   - a handler that performs the work (usually a TrueNAS REST call), and
//   - a short human-readable status line the chat handler emits while the tool runs
//     so the iOS UI can show "Checking pools…" instead of spinning silently.
//
// All v1 tools are read-only. Write operations live behind a future approval
// flow — not plumbed here.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// Definition is the JSON-schema shape Ollama expects under `tools[i].function`.
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Tool pairs a definition with a handler.
type Tool struct {
	Def        Definition
	StatusLine string // e.g. "Checking pool status…" — shown to the user while the tool runs
	Handler    func(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry owns the set of registered tools and dispatches calls.
type Registry struct {
	tools map[string]Tool
}

// New builds a registry against the given TrueNAS client. If the client isn't
// configured (no URL/API key), an empty registry is returned — chat still
// works without tools.
func New(tn *truenas.Client) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	if !tn.Configured() {
		return r
	}
	for _, t := range builtins(tn) {
		r.tools[t.Def.Name] = t
	}
	return r
}

// Empty reports whether the registry has no tools (helper was started without
// TrueNAS credentials).
func (r *Registry) Empty() bool { return len(r.tools) == 0 }

// Definitions returns all tool definitions in the shape Ollama's /api/chat
// expects in its request body.
func (r *Registry) Definitions() []map[string]any {
	out := make([]map[string]any, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, map[string]any{
			"type":     "function",
			"function": t.Def,
		})
	}
	return out
}

// StatusLine returns the short human-readable line for a tool, or a generic
// fallback if the tool isn't known (the LLM can hallucinate tool names).
func (r *Registry) StatusLine(name string) string {
	if t, ok := r.tools[name]; ok && t.StatusLine != "" {
		return t.StatusLine
	}
	return fmt.Sprintf("Using tool: %s…", name)
}

// Dispatch runs the named tool with the given arguments, returning the
// JSON-encoded result the LLM should see as the tool message content. Unknown
// tool names aren't a fatal error — we return the error as the tool result so
// the model can recover.
func (r *Registry) Dispatch(ctx context.Context, name string, args json.RawMessage) string {
	t, ok := r.tools[name]
	if !ok {
		return toolErrorf("unknown tool: %s", name)
	}
	result, err := t.Handler(ctx, args)
	if err != nil {
		return toolErrorf("%s failed: %v", name, err)
	}
	return result
}

func toolErrorf(format string, a ...any) string {
	// Encode as JSON so the model parses it uniformly with success results.
	b, _ := json.Marshal(map[string]string{"error": fmt.Sprintf(format, a...)})
	return string(b)
}
