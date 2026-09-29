package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/donnie123421/zephyr-helper/internal/ollama"
)

// Ollama runs chats on the NAS through Ollama, as the helper always has.
type Ollama struct {
	client *ollama.Client

	// mu serializes pulls across chats so several connecting clients don't
	// each start a download. readyModel is the model last pulled in this
	// process; when the active model differs (the app's picker switched
	// it), the next chat pulls again.
	mu         sync.Mutex
	readyModel string
}

func NewOllama(client *ollama.Client) *Ollama {
	return &Ollama{client: client}
}

func (o *Ollama) Name() string  { return ProviderOllama }
func (o *Ollama) Model() string { return o.client.Model() }

// Prepare pulls the active model unless this process already has.
func (o *Ollama) Prepare(ctx context.Context, onStatus func(string)) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	model := o.client.Model()
	if o.readyModel == model {
		onStatus("Model ready.")
		return nil
	}
	onStatus(fmt.Sprintf("Downloading model %s — this can take several minutes.", model))

	lastPct := -1
	err := o.client.Pull(ctx, func(s ollama.PullStatus) {
		// Only forward progress when the rounded percent moves, plus
		// phase changes that carry no percent ("verifying sha256 digest").
		if s.Total > 0 {
			pct := int((s.Completed * 100) / s.Total)
			if pct == lastPct {
				return
			}
			lastPct = pct
			onStatus(fmt.Sprintf("%s (%d%%)", s.Status, pct))
			return
		}
		onStatus(s.Status)
	})
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}

	o.readyModel = model
	onStatus("Model ready.")
	return nil
}

// forget makes the next Prepare pull model again, after Ollama reported it
// missing even though this process pulled it (deleted from Ollama since).
func (o *Ollama) forget(model string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.readyModel == model {
		o.readyModel = ""
	}
}

func (o *Ollama) NewSession(system string, tools []Tool) Session {
	defs := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, map[string]any{
			"type":     "function",
			"function": ollamaFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	return &ollamaSession{
		owner:   o,
		tools:   defs,
		history: []ollama.Message{{Role: "system", Content: system}},
	}
}

// ollamaFunction is the `function` object Ollama's /api/chat expects for
// each tool.
type ollamaFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ollamaSession struct {
	owner   *Ollama
	tools   []map[string]any
	history []ollama.Message
}

func (s *ollamaSession) AddUserMessage(text string) {
	s.history = append(s.history, ollama.Message{Role: "user", Content: text})
	// Bound the replayed conversation before it goes back to Ollama, or a
	// long chat overruns the context window and the system prompt is the
	// first thing Ollama drops.
	if trimmed := trimHistory(s.history); len(trimmed) != len(s.history) {
		slog.Info("chat: history trimmed", "from_messages", len(s.history), "to_messages", len(trimmed))
		s.history = trimmed
	}
}

func (s *ollamaSession) AddAssistantMessage(text string) {
	s.history = append(s.history, ollama.Message{Role: "assistant", Content: text})
}

func (s *ollamaSession) Step(ctx context.Context, onDelta func(string) error) (StepResult, error) {
	result, err := s.owner.client.Chat(ctx, s.history, s.tools, onDelta)
	if errors.Is(err, ollama.ErrModelMissing) {
		model := s.owner.client.Model()
		s.owner.forget(model)
		return StepResult{}, fmt.Errorf("%w: %s", ErrModelNotReady, model)
	}
	if err != nil {
		return StepResult{}, err
	}

	s.history = append(s.history, result.Message)
	calls := make([]ToolCall, 0, len(result.ToolCalls))
	for _, tc := range result.ToolCalls {
		calls = append(calls, ToolCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return StepResult{ToolCalls: calls}, nil
}

func (s *ollamaSession) AddToolResults(results []ToolResult) {
	for _, r := range results {
		s.history = append(s.history, ollama.Message{Role: "tool", Content: r.Content})
	}
}
