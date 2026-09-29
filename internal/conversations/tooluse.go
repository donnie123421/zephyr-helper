package conversations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"
)

// MaxToolResultBytes bounds what's kept of one tool's result. The model saw
// all of it; the app shows what the assistant looked at, and a phone
// screen's worth fits in far less.
const MaxToolResultBytes = 32 << 10

// maxToolArgumentsBytes bounds the arguments kept with a tool. Real ones
// are a name or two.
const maxToolArgumentsBytes = 2 << 10

// ToolUse is a tool the assistant ran for a reply and what it returned, so
// the app can show what the assistant looked at: live in the chat's
// tool_used frames, and later with the saved chat. The same JSON goes to
// the app and into the store.
type ToolUse struct {
	Name string `json:"name"`
	// Arguments the model passed, as a JSON object; absent when it passed
	// none.
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Result is the JSON the tool returned, shortened to fit
	// MaxToolResultBytes when it was longer (Trimmed). Absent when even
	// that didn't fit, and for chats saved before results were kept.
	Result  json.RawMessage `json:"result,omitempty"`
	Failed  bool            `json:"failed,omitempty"`
	Trimmed bool            `json:"trimmed,omitempty"`
	// CheckedAt is when the tool ran, to the second, which is what the
	// app's date decoding expects. Absent for chats saved before results
	// were kept.
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

// NewToolUse records a tool the assistant ran at time at.
func NewToolUse(name string, arguments json.RawMessage, result string, failed bool, at time.Time) ToolUse {
	checked := at.UTC().Truncate(time.Second)
	use := ToolUse{Name: name, Arguments: keptArguments(arguments), Failed: failed, CheckedAt: &checked}
	use.Result, use.Trimmed = keptResult(result)
	return use
}

// ToolNames lists the tools by name, in order.
func ToolNames(uses []ToolUse) []string {
	names := make([]string, 0, len(uses))
	for _, use := range uses {
		names = append(names, use.Name)
	}
	return names
}

// keptArguments keeps the model's arguments when they're a JSON object
// with something in it.
func keptArguments(raw json.RawMessage) json.RawMessage {
	if len(raw) > maxToolArgumentsBytes {
		return nil
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil || len(args) == 0 {
		return nil
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return nil
	}
	return compact.Bytes()
}

// shrinkSteps are the limits keptResult tries in turn: how many entries of
// each list it keeps, and how many characters of each string.
var shrinkSteps = []struct{ items, runes int }{
	{100, 2000}, {50, 1000}, {25, 500}, {10, 200}, {5, 100}, {2, 60},
}

// keptResult is what's kept of a tool's result: all of it when it fits,
// otherwise a copy with long lists cut to their first entries (and a note
// of how many more there were) and long text cut short. It reports whether
// the result was shortened, or dropped because even that didn't fit.
func keptResult(result string) (json.RawMessage, bool) {
	raw := []byte(result)
	if !json.Valid(raw) {
		// Every tool answers in JSON, but anything else is kept as text.
		raw, _ = json.Marshal(result)
	}
	// Measured as it will be written: encoding/json escapes <, > and & in
	// a RawMessage too, and each grows to six bytes.
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return nil, true
	}
	var escaped bytes.Buffer
	json.HTMLEscape(&escaped, compact.Bytes())
	raw = escaped.Bytes()
	if len(raw) <= MaxToolResultBytes {
		return raw, false
	}

	// Numbers stay as they were written rather than go through float64.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, true
	}
	for _, step := range shrinkSteps {
		shrunk, err := json.Marshal(shrink(value, step.items, step.runes))
		if err == nil && len(shrunk) <= MaxToolResultBytes {
			return shrunk, true
		}
	}
	return nil, true
}

func shrink(value any, items, runes int) any {
	switch v := value.(type) {
	case []any:
		keep := min(len(v), items)
		out := make([]any, 0, keep+1)
		for _, entry := range v[:keep] {
			out = append(out, shrink(entry, items, runes))
		}
		if more := len(v) - keep; more > 0 {
			out = append(out, fmt.Sprintf("… and %d more", more))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, entry := range v {
			out[key] = shrink(entry, items, runes)
		}
		return out
	case string:
		if utf8.RuneCountInString(v) > runes {
			return string([]rune(v)[:runes]) + "…"
		}
	}
	return value
}

// encodeTools is how a message's tools are kept in the messages table.
func encodeTools(uses []ToolUse) (string, error) {
	if uses == nil {
		uses = []ToolUse{}
	}
	b, err := json.Marshal(uses)
	return string(b), err
}

// decodeTools reads a message's tools back. Chats saved before results
// were kept have just the names.
func decodeTools(stored string) []ToolUse {
	var entries []json.RawMessage
	_ = json.Unmarshal([]byte(stored), &entries)
	uses := make([]ToolUse, 0, len(entries))
	for _, entry := range entries {
		var name string
		if json.Unmarshal(entry, &name) == nil {
			uses = append(uses, ToolUse{Name: name})
			continue
		}
		var use ToolUse
		if json.Unmarshal(entry, &use) == nil && use.Name != "" {
			uses = append(uses, use)
		}
	}
	return uses
}
