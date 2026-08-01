package chat

import "github.com/donnie123421/zephyr-helper/internal/ollama"

// maxHistoryChars bounds the conversation we replay to Ollama on each turn.
//
// Chat history used to grow for the life of the WebSocket: every user message,
// every assistant reply and every tool result (a pool listing is easily several
// KB) stayed in the slice and was re-sent on the next turn. A long session
// eventually overflowed the model's context window, at which point Ollama
// silently truncates from the front — dropping the system prompt, which is what
// carries the "never invent pool names" guidance.
//
// ~24k characters is roughly 6k tokens at the usual ~4 chars/token, which fits
// comfortably inside Ollama's default 4k–8k num_ctx with room for the model's
// own reply. Characters rather than tokens because we have no tokenizer here
// and don't want one — this is a backstop, not an accountant.
const maxHistoryChars = 24000

// trimHistory drops the oldest turns until the conversation fits the budget,
// always preserving the system prompt at index 0.
//
// Trimming happens at *user*-message boundaries. Cutting anywhere else can
// orphan a `tool` result whose preceding assistant tool_call was dropped, which
// models handle badly — llama3.1 in particular starts answering about the
// dangling tool output. Slicing to a user message keeps every assistant/tool
// exchange intact.
//
// The most recent turn is always kept even if it alone busts the budget:
// sending a truncated version of what the user just asked is worse than
// sending an over-long prompt and letting Ollama deal with it.
func trimHistory(history []ollama.Message) []ollama.Message {
	if len(history) < 2 {
		return history
	}

	// Index 0 is the system prompt; it is never a trim candidate.
	system := history[0]
	rest := history[1:]

	if historySize(system)+historySize(rest...) <= maxHistoryChars {
		return history
	}

	budget := maxHistoryChars - historySize(system)

	// Walk backwards accumulating until the budget is spent, then snap the cut
	// forward to the next user message so we never begin mid-exchange.
	cut := 0
	used := 0
	for i := len(rest) - 1; i >= 0; i-- {
		used += historySize(rest[i])
		if used > budget {
			cut = i + 1
			break
		}
	}

	for cut < len(rest) && rest[cut].Role != "user" {
		cut++
	}

	// Everything after the cut turned out to be non-user (or nothing survived).
	// Fall back to the last user message onward so the current question is
	// always present.
	if cut >= len(rest) {
		cut = lastUserIndex(rest)
	}

	trimmed := make([]ollama.Message, 0, len(rest)-cut+1)
	trimmed = append(trimmed, system)
	trimmed = append(trimmed, rest[cut:]...)
	return trimmed
}

// lastUserIndex returns the index of the final user message, or 0 when there
// isn't one (in which case the caller keeps everything it has).
func lastUserIndex(msgs []ollama.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return 0
}

// historySize approximates the prompt cost of some messages. Tool-call
// arguments count too — a model that calls a tool with a long argument blob
// pays for it on every subsequent turn.
func historySize(msgs ...ollama.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Role) + len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return n
}
