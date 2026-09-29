package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultAnthropicModel is what Claude chats use until the user picks
// another model.
const DefaultAnthropicModel = "claude-opus-5"

// anthropicMaxTokens caps one reply, thinking included. Chat answers are
// short, so this only stops a runaway reply.
const anthropicMaxTokens = 16000

// Anthropic runs chats on Claude with the user's own API key. Thinking and
// effort are left at each model's default, which every model in the picker
// accepts.
type Anthropic struct {
	client anthropic.Client
	model  string
}

// NewAnthropic builds a provider for model using apiKey. An empty baseURL
// means Anthropic's API; a gateway can stand in for it without a helper
// change.
func NewAnthropic(apiKey, model, baseURL string) *Anthropic {
	if model == "" {
		model = DefaultAnthropicModel
	}
	return &Anthropic{client: newAnthropicClient(apiKey, baseURL), model: model}
}

func newAnthropicClient(apiKey, baseURL string) anthropic.Client {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return anthropic.NewClient(opts...)
}

func (a *Anthropic) Name() string  { return ProviderAnthropic }
func (a *Anthropic) Model() string { return a.model }

// Prepare has nothing to do: the model is hosted.
func (a *Anthropic) Prepare(context.Context, func(string)) error { return nil }

func (a *Anthropic) NewSession(system string, tools []Tool) Session {
	return &anthropicSession{
		client: &a.client,
		model:  a.model,
		// The tools and system prompt open every request unchanged, so a
		// cache breakpoint here lets every chat reuse them.
		system: []anthropic.TextBlockParam{{
			Text:         system,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		tools: anthropicTools(tools),
	}
}

// anthropicTools converts the tool definitions. Strict mode guarantees the
// model's arguments match each schema; the helper's schemas already meet
// its rules (additionalProperties false, no numeric or length limits).
func anthropicTools(tools []Tool) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		if err := json.Unmarshal(t.Parameters, &schema); err != nil {
			slog.Warn("claude: tool schema unreadable; sending it without parameters", "tool", t.Name, "err", err)
		}
		if schema.Properties == nil {
			schema.Properties = map[string]any{}
		}
		tool := anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties:  schema.Properties,
				Required:    schema.Required,
				ExtraFields: map[string]any{"additionalProperties": false},
			},
			Strict: anthropic.Bool(true),
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tool})
	}
	return out
}

type anthropicSession struct {
	client   *anthropic.Client
	model    string
	system   []anthropic.TextBlockParam
	tools    []anthropic.ToolUnionParam
	messages []anthropic.MessageParam
}

func (s *anthropicSession) AddUserMessage(text string) {
	s.messages = append(s.messages, anthropic.NewUserMessage(anthropic.NewTextBlock(text)))
}

func (s *anthropicSession) AddAssistantMessage(text string) {
	s.messages = append(s.messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(text)))
}

func (s *anthropicSession) Step(ctx context.Context, onDelta func(string) error) (StepResult, error) {
	stream := s.client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     s.model,
		MaxTokens: anthropicMaxTokens,
		System:    s.system,
		Tools:     s.tools,
		Messages:  s.messages,
		// A second breakpoint on the conversation so far, so each request
		// only pays full price for what's new since the last one.
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	})
	defer stream.Close()

	reply := anthropic.Message{}
	for stream.Next() {
		event := stream.Current()
		if err := reply.Accumulate(event); err != nil {
			return StepResult{}, fmt.Errorf("claude stream: %w", err)
		}
		if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if text, ok := delta.Delta.AsAny().(anthropic.TextDelta); ok && text.Text != "" {
				if err := onDelta(text.Text); err != nil {
					return StepResult{}, err
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return StepResult{}, describeAnthropicError(err, s.model)
	}

	usage := Usage{
		InputTokens:      reply.Usage.InputTokens,
		OutputTokens:     reply.Usage.OutputTokens,
		CacheReadTokens:  reply.Usage.CacheReadInputTokens,
		CacheWriteTokens: reply.Usage.CacheCreationInputTokens,
	}

	// These replies stay out of the transcript: a refusal or a cut-off
	// reply can end partway through a tool call, which must never run or be
	// replayed. The chat carries on from the user's last message.
	switch reply.StopReason {
	case anthropic.StopReasonRefusal:
		return StepResult{Usage: usage}, Problem("Claude declined to answer that. Try rephrasing it.")
	case anthropic.StopReasonMaxTokens:
		return StepResult{Usage: usage}, Problem("Claude's reply hit its length limit before it finished.")
	case anthropic.StopReasonModelContextWindowExceeded:
		return StepResult{Usage: usage}, Problem("This chat has grown too long for the model. Start a new chat.")
	}

	// Replayed exactly as it came back, thinking blocks included.
	s.messages = append(s.messages, reply.ToParam())

	var calls []ToolCall
	for _, block := range reply.Content {
		if use, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
			args := json.RawMessage(use.JSON.Input.Raw())
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			calls = append(calls, ToolCall{ID: use.ID, Name: use.Name, Arguments: args})
		}
	}
	return StepResult{ToolCalls: calls, Usage: usage}, nil
}

func (s *anthropicSession) AddToolResults(results []ToolResult) {
	if len(results) == 0 {
		return
	}
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(results))
	for _, r := range results {
		blocks = append(blocks, anthropic.NewToolResultBlock(r.CallID, r.Content, r.IsError))
	}
	s.messages = append(s.messages, anthropic.NewUserMessage(blocks...))
}

// ModelInfo is a model a key can use, for the app's picker.
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// AnthropicModels lists the models apiKey can use, newest first. It's also
// how a key gets tested before it's saved.
func AnthropicModels(ctx context.Context, apiKey, baseURL string) ([]ModelInfo, error) {
	client := newAnthropicClient(apiKey, baseURL)
	pager := client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	var out []ModelInfo
	for pager.Next() {
		m := pager.Current()
		out = append(out, ModelInfo{ID: m.ID, DisplayName: m.DisplayName})
	}
	if err := pager.Err(); err != nil {
		return nil, describeAnthropicError(err, "")
	}
	return out, nil
}

// describeAnthropicError turns an API failure into something the person in
// the chat can act on, logging the details for the helper's logs.
func describeAnthropicError(err error, model string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		slog.Warn("claude: request failed", "err", err)
		return Problem("Couldn't reach Anthropic. Check the NAS's internet connection.")
	}
	slog.Warn("claude: API error", "status", apiErr.StatusCode, "request_id", apiErr.RequestID, "err", err)
	switch apiErr.StatusCode {
	case http.StatusUnauthorized:
		return Problem("Anthropic rejected the API key. Check it in the assistant's settings.")
	case http.StatusPaymentRequired:
		return Problem("Your Anthropic account is out of credit. Add some in the Anthropic Console.")
	case http.StatusForbidden:
		if model == "" {
			return Problem("This API key doesn't have permission for that.")
		}
		return Problem(fmt.Sprintf("This API key doesn't have access to %s.", model))
	case http.StatusNotFound:
		if model == "" {
			return Problem("Anthropic couldn't find that.")
		}
		return Problem(fmt.Sprintf("Anthropic doesn't recognise the model %q.", model))
	case http.StatusTooManyRequests:
		return Problem("You've hit Anthropic's rate limit. Wait a moment and try again.")
	}
	if apiErr.StatusCode >= 500 {
		return Problem("Anthropic is having trouble right now. Try again shortly.")
	}
	return Problem(fmt.Sprintf("Anthropic returned an error (%d).", apiErr.StatusCode))
}

// Problem is an error whose message is written for the person using the
// app, so the chat and settings screens can show it as it is.
type Problem string

func (p Problem) Error() string { return string(p) }
