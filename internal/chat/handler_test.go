package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/donnie123421/zephyr-helper/internal/llm"
	"github.com/donnie123421/zephyr-helper/internal/tools"
)

// scriptedSession replays canned steps and records what the loop fed it.
type scriptedSession struct {
	steps   []scriptedStep
	results [][]llm.ToolResult
}

type scriptedStep struct {
	deltas []string
	result llm.StepResult
	err    error
}

func (s *scriptedSession) AddUserMessage(string)      {}
func (s *scriptedSession) AddAssistantMessage(string) {}

func (s *scriptedSession) Step(_ context.Context, onDelta func(string) error) (llm.StepResult, error) {
	if len(s.steps) == 0 {
		return llm.StepResult{}, errors.New("no step scripted")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	for _, d := range step.deltas {
		if err := onDelta(d); err != nil {
			return llm.StepResult{}, err
		}
	}
	return step.result, step.err
}

func (s *scriptedSession) AddToolResults(r []llm.ToolResult) { s.results = append(s.results, r) }

type fakeProvider struct {
	prepared int
}

func (p *fakeProvider) Name() string                                    { return "fake" }
func (p *fakeProvider) Model() string                                   { return "fake-1" }
func (p *fakeProvider) NewSession(string, []llm.Tool) llm.Session       { return nil }
func (p *fakeProvider) Prepare(_ context.Context, _ func(string)) error { p.prepared++; return nil }

func testRegistry() *tools.Registry {
	return tools.NewWith(
		tools.Tool{
			Def:        tools.Definition{Name: "list_pools", Parameters: json.RawMessage(`{}`)},
			StatusLine: "Checking pools…",
			Handler: func(context.Context, json.RawMessage) (string, error) {
				return `{"pools":["tank"]}`, nil
			},
		},
		tools.Tool{
			Def: tools.Definition{Name: "broken", Parameters: json.RawMessage(`{}`)},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				return "", errors.New("boom")
			},
		},
	)
}

// frames captures what the loop sends, as JSON for easy matching.
type frames []map[string]any

func (f *frames) send(v any) error {
	data, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	*f = append(*f, m)
	return nil
}

func (f frames) types() []string {
	var out []string
	for _, m := range f {
		out = append(out, fmt.Sprint(m["type"]))
	}
	return out
}

func TestRunTurnDispatchesToolsAndReportsUsage(t *testing.T) {
	h := &Handler{tools: testRegistry()}
	session := &scriptedSession{steps: []scriptedStep{
		{
			deltas: []string{"Checking. "},
			result: llm.StepResult{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "list_pools", Arguments: json.RawMessage(`{}`)},
					{ID: "call_2", Name: "broken", Arguments: json.RawMessage(`{}`)},
				},
				Usage: llm.Usage{InputTokens: 100, OutputTokens: 10},
			},
		},
		{
			deltas: []string{"tank is fine."},
			result: llm.StepResult{Usage: llm.Usage{InputTokens: 150, OutputTokens: 20, CacheReadTokens: 90}},
		},
	}}
	var out frames
	onStatus := func(s string) { _ = out.send(outboundEvent{Type: "status", Content: s}) }

	if err := h.runTurn(context.Background(), out.send, onStatus, &fakeProvider{}, session); err != nil {
		t.Fatalf("runTurn: %v", err)
	}

	want := []string{"delta", "status", "tool_used", "status", "tool_used", "delta", "usage"}
	if got := out.types(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
	// The answer after the tools starts a new paragraph.
	if got := out[5]["content"]; got != "\n\ntank is fine." {
		t.Errorf("second step's text = %q", got)
	}
	usage := out[len(out)-1]
	if usage["input_tokens"] != float64(250) || usage["output_tokens"] != float64(30) ||
		usage["cache_read_tokens"] != float64(90) || usage["model"] != "fake-1" {
		t.Errorf("usage frame = %v", usage)
	}

	if len(session.results) != 1 || len(session.results[0]) != 2 {
		t.Fatalf("tool results = %+v", session.results)
	}
	ok, failed := session.results[0][0], session.results[0][1]
	if ok.CallID != "call_1" || ok.IsError || ok.Content != `{"pools":["tank"]}` {
		t.Errorf("list_pools result = %+v", ok)
	}
	if failed.CallID != "call_2" || !failed.IsError {
		t.Errorf("broken result = %+v, want an error result", failed)
	}
}

func TestRunTurnPreparesAndRetriesWhenTheModelIsMissing(t *testing.T) {
	h := &Handler{tools: testRegistry()}
	provider := &fakeProvider{}
	session := &scriptedSession{steps: []scriptedStep{
		{err: fmt.Errorf("%w: qwen", llm.ErrModelNotReady)},
		{deltas: []string{"hello"}},
	}}
	var out frames
	if err := h.runTurn(context.Background(), out.send, func(string) {}, provider, session); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if provider.prepared != 1 {
		t.Errorf("prepared %d times, want 1", provider.prepared)
	}
}

func TestRunTurnReportsUsageEvenWhenItFails(t *testing.T) {
	h := &Handler{tools: testRegistry()}
	session := &scriptedSession{steps: []scriptedStep{
		{result: llm.StepResult{Usage: llm.Usage{InputTokens: 5}}, err: llm.Problem("Claude declined to answer that.")},
	}}
	var out frames
	err := h.runTurn(context.Background(), out.send, func(string) {}, &fakeProvider{}, session)
	if err == nil || err.Error() != "Claude declined to answer that." {
		t.Fatalf("err = %v", err)
	}
	if got := out.types(); len(got) != 1 || got[0] != "usage" {
		t.Errorf("frames = %v, want just the usage", got)
	}
}

func TestRunTurnStopsRunawayToolLoops(t *testing.T) {
	h := &Handler{tools: testRegistry()}
	loop := scriptedStep{result: llm.StepResult{ToolCalls: []llm.ToolCall{{ID: "x", Name: "list_pools"}}}}
	session := &scriptedSession{}
	for i := 0; i < maxToolIterations; i++ {
		session.steps = append(session.steps, loop)
	}
	var out frames
	if err := h.runTurn(context.Background(), out.send, func(string) {}, &fakeProvider{}, session); err == nil {
		t.Fatal("expected the iteration cap to stop the loop")
	}
}

func TestRunTurnStartsANewParagraphOnlyWhenNeeded(t *testing.T) {
	cases := []struct{ first, want string }{
		{"Checking.", "\n\nDone."},
		{"Checking.\n", "\nDone."},
		{"Checking.\n\n", "Done."},
	}
	for _, c := range cases {
		h := &Handler{tools: testRegistry()}
		session := &scriptedSession{steps: []scriptedStep{
			{deltas: []string{c.first}, result: llm.StepResult{ToolCalls: []llm.ToolCall{{ID: "1", Name: "list_pools"}}}},
			{deltas: []string{"Done."}},
		}}
		var out frames
		if err := h.runTurn(context.Background(), out.send, func(string) {}, &fakeProvider{}, session); err != nil {
			t.Fatalf("runTurn: %v", err)
		}
		var deltas []string
		for _, f := range out {
			if f["type"] == "delta" {
				deltas = append(deltas, f["content"].(string))
			}
		}
		if len(deltas) != 2 || deltas[1] != c.want {
			t.Errorf("after %q, next delta = %q, want %q", c.first, deltas, c.want)
		}
	}
}
