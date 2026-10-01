// Package tools is the tool palette the LLM can call during a chat turn.
//
// Every tool has three pieces:
//   - an Ollama-compatible JSON schema so the model knows when + how to use it,
//   - a handler that performs the work (usually a TrueNAS REST call), and
//   - a short human-readable status line the chat handler emits while the tool runs
//     so the iOS UI can show "Checking pools…" instead of spinning silently.
//
// Most tools only read. Actions change the NAS: they're offered only in chats
// with actions switched on, and each one runs only after the user approves a
// card the tool writes itself (see Action).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

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
	// Action, set instead of Handler, makes the tool change the NAS.
	Action *Action
}

// Action is a tool that changes the NAS. Nothing runs without the user's
// approval: Describe writes the card they're shown, and Run does it once
// they approve.
type Action struct {
	// Describe checks the arguments against the NAS and writes the card.
	// Its error goes back to the model, and no card is shown.
	Describe func(ctx context.Context, args json.RawMessage) (Card, error)
	// Run does what the card said.
	Run func(ctx context.Context, args json.RawMessage) (Outcome, error)
}

// Risk says who an action disturbs.
type Risk string

const (
	// RiskRoutine: nobody is interrupted (a snapshot, a scrub, a backup).
	RiskRoutine Risk = "routine"
	// RiskDisruptive: something people use stops for a while (an app, a
	// VM or a service), and the card says what.
	RiskDisruptive Risk = "disruptive"
)

// Card is what the user approves. Its words come from the tool, built from
// checked arguments and what the NAS reports, never from the model, so a
// crafted dataset or app name can't make a card say what it isn't.
type Card struct {
	Title  string
	Detail string
	Risk   Risk
	// Args, when set, are what Run gets instead of the model's arguments:
	// the defaults Describe settled on, such as a snapshot's name or an
	// update's version, so what runs is what the card said.
	Args json.RawMessage
}

// Outcome is what an approved action did.
type Outcome struct {
	// Message says what happened, for the user and the model.
	Message string
	// Job is set when TrueNAS carries on after Run returns: how it ends
	// lands in the activity feed.
	Job *Job
	// Ran are the TrueNAS jobs the action saw through, so the activity
	// feed records them once, as this action.
	Ran []int64
}

// Job is a TrueNAS job an action left running.
type Job struct {
	ID int64
	// Done and Failed title the activity entry when the job ends.
	Done, Failed string
	// Report, when set, writes the entry instead, from what the NAS shows
	// once the job has ended: a scrub's job ends the same way whether it
	// found errors or not.
	Report func(ctx context.Context, ended truenas.Job) Ending
}

// Ending is how an action's job turned out, for its activity entry.
type Ending struct {
	Title, Summary string
	// Bad marks an ending worth a warning, and a notification.
	Bad bool
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

// NewWith builds a registry from the given tools rather than the TrueNAS
// builtins.
func NewWith(ts ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range ts {
		r.tools[t.Def.Name] = t
	}
	return r
}

type forLaterKey struct{}

// ForLater marks a Describe made to book an action for later. What it would
// act on being in that state already (an app running, a scrub under way)
// doesn't stop it being booked: things will have changed by then, and the
// action is checked against the NAS again when it runs.
func ForLater(ctx context.Context) context.Context {
	return context.WithValue(ctx, forLaterKey{}, true)
}

func isForLater(ctx context.Context) bool {
	v, _ := ctx.Value(forLaterKey{}).(bool)
	return v
}

// Add registers more tools, such as those other packages provide.
func (r *Registry) Add(ts ...Tool) {
	for _, t := range ts {
		r.tools[t.Def.Name] = t
	}
}

// Empty reports whether the registry has no tools (helper was started without
// TrueNAS credentials).
func (r *Registry) Empty() bool { return len(r.tools) == 0 }

// Len is how many tools there are, actions included.
func (r *Registry) Len() int { return len(r.tools) }

// HasActions reports whether any tool changes the NAS.
func (r *Registry) HasActions() bool {
	for _, t := range r.tools {
		if t.Action != nil {
			return true
		}
	}
	return false
}

// List returns the tool definitions, sorted by name, leaving out actions
// unless withActions. The list is part of every prompt, and hosted model
// APIs only reuse a cached prompt whose prefix is byte-identical — map
// order would make every request a cache miss.
func (r *Registry) List(withActions bool) []Definition {
	names := make([]string, 0, len(r.tools))
	for name, t := range r.tools {
		if t.Action != nil && !withActions {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]Definition, 0, len(names))
	for _, name := range names {
		out = append(out, r.tools[name].Def)
	}
	return out
}

// Action returns the named tool's action, or nil when it isn't one.
func (r *Registry) Action(name string) *Action {
	return r.tools[name].Action
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
// JSON-encoded result the LLM should see as the tool message content, and
// whether it's an error. Unknown tool names aren't a fatal error — we return
// the error as the tool result so the model can recover. Actions never run
// here: they go through the user's approval.
func (r *Registry) Dispatch(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	t, ok := r.tools[name]
	if !ok || t.Handler == nil {
		return ToolError(fmt.Sprintf("unknown tool: %s", name)), true
	}
	result, err := t.Handler(ctx, args)
	if err != nil {
		return toolErrorf("%s failed: %v", name, err), true
	}
	return result, false
}

func toolErrorf(format string, a ...any) string {
	return ToolError(fmt.Sprintf(format, a...))
}

// ToolError is a tool result reporting an error, encoded as JSON so the
// model parses it uniformly with success results.
func ToolError(message string) string {
	b, _ := json.Marshal(map[string]string{"error": message})
	return string(b)
}
