package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// DefaultOpenAIModel is OpenAI's flagship, what ChatGPT chats use until the
// user picks another model.
const DefaultOpenAIModel = "gpt-6-astra"

// OpenAI runs chats through the Chat Completions API. The same adapter
// serves any OpenAI-compatible server (LM Studio, llama.cpp, vLLM,
// OpenRouter) given its base URL: that's the Custom provider.
type OpenAI struct {
	name   string
	client openai.Client
	model  string
}

// NewOpenAI builds a provider named name (ProviderOpenAI or
// ProviderCustom). An empty baseURL means OpenAI's API; apiKey may be empty
// for a local server that doesn't check one.
func NewOpenAI(name, apiKey, model, baseURL string) *OpenAI {
	return &OpenAI{name: name, client: newOpenAIClient(apiKey, baseURL), model: model}
}

func newOpenAIClient(apiKey, baseURL string) openai.Client {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(NormalizeBaseURL(baseURL)))
	}
	return openai.NewClient(opts...)
}

// NormalizeBaseURL adds the /v1 that OpenAI-compatible servers serve their
// API under when the address has no path at all ("http://pc:1234").
func NormalizeBaseURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1"
	}
	return u.String()
}

func (o *OpenAI) Name() string  { return o.name }
func (o *OpenAI) Model() string { return o.model }

// Prepare has nothing to do: the model runs elsewhere.
func (o *OpenAI) Prepare(context.Context, func(string)) error { return nil }

func (o *OpenAI) NewSession(system string, tools []Tool) Session {
	return &openAISession{
		name:     o.name,
		client:   &o.client,
		model:    o.model,
		tools:    openAITools(tools),
		messages: []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(system)},
	}
}

// openAITools converts the tool definitions. Not strict: OpenAI's strict
// mode needs every property required, and the helper's tools have optional
// ones (pool_status without a pool name covers every pool).
func openAITools(tools []Tool) []openai.ChatCompletionToolUnionParam {
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		var params shared.FunctionParameters
		if err := json.Unmarshal(t.Parameters, &params); err != nil || params == nil {
			params = shared.FunctionParameters{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: openai.String(t.Description),
			Parameters:  params,
		}))
	}
	return out
}

type openAISession struct {
	name     string
	client   *openai.Client
	model    string
	tools    []openai.ChatCompletionToolUnionParam
	messages []openai.ChatCompletionMessageParamUnion
}

func (s *openAISession) AddUserMessage(text string) {
	s.messages = append(s.messages, openai.UserMessage(text))
}

func (s *openAISession) AddAssistantMessage(text string) {
	s.messages = append(s.messages, openai.AssistantMessage(text))
}

func (s *openAISession) Step(ctx context.Context, onDelta func(string) error) (StepResult, error) {
	params := openai.ChatCompletionNewParams{
		Model:    s.model,
		Messages: s.messages,
		// Token counts arrive in a final chunk, and only when asked for.
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	}
	if len(s.tools) > 0 {
		params.Tools = s.tools
	}
	stream := s.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	acc := openai.ChatCompletionAccumulator{}
	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			if err := onDelta(chunk.Choices[0].Delta.Content); err != nil {
				return StepResult{}, err
			}
		}
	}
	if err := stream.Err(); err != nil {
		return StepResult{}, describeOpenAIError(err, s.model, s.name)
	}

	cached := acc.Usage.PromptTokensDetails.CachedTokens
	usage := Usage{
		InputTokens:     acc.Usage.PromptTokens - cached,
		CacheReadTokens: cached,
		OutputTokens:    acc.Usage.CompletionTokens,
	}
	if len(acc.Choices) == 0 {
		return StepResult{Usage: usage}, Problem("The model sent back an empty reply.")
	}
	choice := acc.Choices[0]

	// As with Claude, these replies stay out of the transcript: a cut-off
	// reply can end partway through a tool call's arguments.
	switch {
	case choice.FinishReason == "length":
		return StepResult{Usage: usage}, Problem("The reply hit its length limit before it finished.")
	case choice.FinishReason == "content_filter" || choice.Message.Refusal != "":
		return StepResult{Usage: usage}, Problem("The model declined to answer that. Try rephrasing it.")
	}

	s.messages = append(s.messages, choice.Message.ToParam())

	var calls []ToolCall
	for _, tc := range choice.Message.ToolCalls {
		if tc.Function.Name == "" {
			continue
		}
		args := json.RawMessage(tc.Function.Arguments)
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		calls = append(calls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: args})
	}
	return StepResult{ToolCalls: calls, Usage: usage}, nil
}

func (s *openAISession) AddToolResults(results []ToolResult) {
	for _, r := range results {
		s.messages = append(s.messages, openai.ToolMessage(r.Content, r.CallID))
	}
}

// OpenAIModels lists the chat models apiKey can use at baseURL, newest
// first; it's also how a key or server gets tested before it's saved.
// OpenAI's own list mixes in embedding, audio and image models, which are
// left out; a custom server's list is passed through as it is.
func OpenAIModels(ctx context.Context, provider, apiKey, baseURL string) ([]ModelInfo, error) {
	client := newOpenAIClient(apiKey, baseURL)
	pager := client.Models.ListAutoPaging(ctx)
	var models []openai.Model
	for pager.Next() {
		models = append(models, pager.Current())
	}
	if err := pager.Err(); err != nil {
		return nil, describeOpenAIError(err, "", provider)
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].Created > models[j].Created })

	out := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		if provider == ProviderOpenAI && !isOpenAIChatModel(m.ID) {
			continue
		}
		out = append(out, ModelInfo{ID: m.ID, DisplayName: m.ID})
	}
	return out, nil
}

func isOpenAIChatModel(id string) bool {
	chatFamily := false
	for _, prefix := range []string{"gpt-", "chatgpt-", "o1", "o3", "o4"} {
		if strings.HasPrefix(id, prefix) {
			chatFamily = true
			break
		}
	}
	if !chatFamily {
		return false
	}
	for _, other := range []string{"audio", "realtime", "transcribe", "tts", "image", "search", "embedding", "moderation", "instruct"} {
		if strings.Contains(id, other) {
			return false
		}
	}
	return true
}

// describeOpenAIError turns an API failure into something the person in
// the chat can act on, logging the details for the helper's logs.
func describeOpenAIError(err error, model, provider string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	who := "OpenAI"
	if provider == ProviderCustom {
		who = "The server"
	}
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		slog.Warn("openai: request failed", "provider", provider, "err", err)
		if provider == ProviderCustom {
			return Problem("Couldn't reach the server. Check its address and that it's running.")
		}
		return Problem("Couldn't reach OpenAI. Check the NAS's internet connection.")
	}
	slog.Warn("openai: API error", "provider", provider, "status", apiErr.StatusCode, "code", apiErr.Code, "err", err)
	switch {
	case apiErr.StatusCode == http.StatusUnauthorized:
		return Problem(who + " rejected the API key. Check it in the assistant's settings.")
	case apiErr.StatusCode == http.StatusTooManyRequests && apiErr.Code == "insufficient_quota":
		return Problem("Your OpenAI account is out of credit. Add some in OpenAI's billing settings.")
	case apiErr.StatusCode == http.StatusTooManyRequests:
		return Problem("You've hit " + possessive(who) + " rate limit. Wait a moment and try again.")
	case apiErr.StatusCode == http.StatusNotFound && model != "":
		return Problem(fmt.Sprintf("%s doesn't have the model %q.", who, model))
	case apiErr.StatusCode == http.StatusForbidden:
		return Problem("This API key doesn't have access to that.")
	case apiErr.StatusCode >= 500:
		return Problem(who + " is having trouble right now. Try again shortly.")
	}
	return Problem(fmt.Sprintf("%s returned an error (%d).", who, apiErr.StatusCode))
}

func possessive(who string) string {
	if who == "The server" {
		return "the server's"
	}
	return who + "'s"
}
