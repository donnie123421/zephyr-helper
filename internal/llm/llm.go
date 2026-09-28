// Package llm puts every model the assistant can use behind one interface,
// so the chat loop and its tools don't care whether the answer comes from
// Ollama on the NAS or a hosted model like Claude.
//
// Each session keeps its transcript in its own provider's format. Claude's
// history has to be replayed exactly as it came back (thinking blocks
// included), which a provider-neutral message list can't carry, so what's
// shared is the loop, not the history.
package llm

import (
	"context"
	"encoding/json"
	"errors"
)

// Provider names, as stored in the config and reported to the app.
const (
	ProviderOllama    = "ollama"
	ProviderAnthropic = "anthropic"
)

// ErrModelNotReady means the model isn't available yet: Ollama hasn't
// pulled it. The chat loop runs Prepare and retries the step once.
var ErrModelNotReady = errors.New("llm: model not ready")

// Provider is a configured model the assistant can talk to.
type Provider interface {
	// Name is one of the Provider* constants.
	Name() string
	// Model is the model a new session uses.
	Model() string
	// Prepare readies the model before a chat, reporting progress lines
	// through onStatus. Ollama pulls the model here; hosted models have
	// nothing to do.
	Prepare(ctx context.Context, onStatus func(string)) error
	// NewSession starts a conversation.
	NewSession(system string, tools []Tool) Session
}

// Session is one conversation with a model.
type Session interface {
	AddUserMessage(text string)
	// Step makes one model call, streaming reply text through onDelta, and
	// adds the reply to the transcript. It returns the tools the model
	// asked to run; none means the model has answered.
	Step(ctx context.Context, onDelta func(string) error) (StepResult, error)
	// AddToolResults answers the tool calls from the last step.
	AddToolResults(results []ToolResult)
}

// Tool is a tool definition in the form every provider accepts.
type Tool struct {
	Name        string
	Description string
	// Parameters is the tool's input as a JSON Schema object.
	Parameters json.RawMessage
}

// ToolCall is one tool the model asked to run.
type ToolCall struct {
	// ID links the result back to the call. Ollama has none.
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolResult answers one ToolCall.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// StepResult is what one model call produced.
type StepResult struct {
	ToolCalls []ToolCall
	Usage     Usage
}

// Usage counts the tokens a call used. Ollama doesn't report any.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// Add accumulates another call's usage into u.
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
}

// IsZero reports whether no tokens were counted.
func (u Usage) IsZero() bool { return u == Usage{} }
