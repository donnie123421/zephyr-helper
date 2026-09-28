package llm

import (
	"strings"
	"testing"

	"github.com/donnie123421/zephyr-helper/internal/ollama"
)

func sys() ollama.Message {
	return ollama.Message{Role: "system", Content: "you are zephyr"}
}

func user(s string) ollama.Message {
	return ollama.Message{Role: "user", Content: s}
}

func assistant(s string) ollama.Message {
	return ollama.Message{Role: "assistant", Content: s}
}

func toolCall(name string) ollama.Message {
	return ollama.Message{
		Role: "assistant",
		ToolCalls: []ollama.ToolCall{
			{Function: ollama.ToolCallFunction{Name: name, Arguments: []byte(`{}`)}},
		},
	}
}

func toolResult(s string) ollama.Message {
	return ollama.Message{Role: "tool", Content: s}
}

// big returns a message whose content alone is a meaningful slice of the
// budget, so a handful of turns is enough to force trimming.
func big(role, prefix string) ollama.Message {
	return ollama.Message{Role: role, Content: prefix + strings.Repeat("x", maxHistoryChars/4)}
}

func TestTrimHistoryLeavesShortConversationsAlone(t *testing.T) {
	in := []ollama.Message{sys(), user("hi"), assistant("hello"), user("pools?")}
	out := trimHistory(in)
	if len(out) != len(in) {
		t.Fatalf("trimmed a short conversation: %d -> %d", len(in), len(out))
	}
}

func TestTrimHistoryKeepsSystemPrompt(t *testing.T) {
	in := []ollama.Message{sys()}
	for i := 0; i < 12; i++ {
		in = append(in, big("user", "q"), big("assistant", "a"))
	}
	out := trimHistory(in)

	if len(out) >= len(in) {
		t.Fatalf("expected trimming, got %d -> %d", len(in), len(out))
	}
	if out[0].Role != "system" {
		t.Errorf("first message = %q, want system", out[0].Role)
	}
	if out[0].Content != in[0].Content {
		t.Errorf("system prompt mutated")
	}
	if got := historySize(out...); got > maxHistoryChars {
		t.Errorf("trimmed size = %d, over budget %d", got, maxHistoryChars)
	}
}

// The important invariant: a `tool` message must never be the first thing
// after the system prompt, because its originating assistant tool_call would
// have been dropped and the model gets a result with no request.
func TestTrimHistoryNeverOrphansToolResults(t *testing.T) {
	in := []ollama.Message{sys()}
	for i := 0; i < 10; i++ {
		in = append(in,
			big("user", "q"),
			toolCall("list_pools"),
			big("tool", "result"),
			assistant("here are your pools"),
		)
	}

	out := trimHistory(in)
	if len(out) >= len(in) {
		t.Fatalf("expected trimming, got %d -> %d", len(in), len(out))
	}
	if out[0].Role != "system" {
		t.Fatalf("first message = %q, want system", out[0].Role)
	}
	if out[1].Role != "user" {
		t.Errorf("conversation resumes at %q, want a user turn (orphaned exchange)", out[1].Role)
	}
	for i, m := range out {
		if m.Role != "tool" {
			continue
		}
		// Every tool result must be preceded (eventually) by an assistant
		// message carrying tool calls.
		if i == 0 || out[i-1].Role == "system" {
			t.Errorf("tool result at %d has no preceding request", i)
		}
	}
}

// A single enormous turn must survive intact — truncating the question the
// user just asked is worse than an over-long prompt.
func TestTrimHistoryKeepsCurrentTurnEvenWhenOversized(t *testing.T) {
	huge := ollama.Message{Role: "user", Content: strings.Repeat("y", maxHistoryChars*2)}
	in := []ollama.Message{sys(), user("old"), assistant("old reply"), huge}

	out := trimHistory(in)
	if len(out) < 2 {
		t.Fatalf("everything was trimmed away: %d messages", len(out))
	}
	if out[0].Role != "system" {
		t.Errorf("lost the system prompt")
	}
	if out[len(out)-1].Content != huge.Content {
		t.Errorf("dropped the current user turn")
	}
}

func TestTrimHistoryHandlesTinyInputs(t *testing.T) {
	for _, in := range [][]ollama.Message{nil, {}, {sys()}} {
		if out := trimHistory(in); len(out) != len(in) {
			t.Errorf("trimHistory(%d msgs) = %d, want unchanged", len(in), len(out))
		}
	}
}
