package conversations

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var checkedAt = time.Date(2026, 9, 30, 4, 2, 11, 750_000_000, time.FixedZone("AEST", 10*3600))

func TestAToolUseKeepsASmallResultWhole(t *testing.T) {
	result := `{"pools":[{"name":"tank","status":"ONLINE","size":10995116277760}]}`
	use := NewToolUse("list_pools", json.RawMessage(`{}`), result, false, checkedAt)

	if string(use.Result) != result || use.Trimmed || use.Failed {
		t.Errorf("use = %+v", use)
	}
	if use.Arguments != nil {
		t.Errorf("arguments = %s, want none for {}", use.Arguments)
	}
	// Whole seconds in UTC, which is what the app's date decoding expects.
	if use.CheckedAt == nil || use.CheckedAt.Location() != time.UTC ||
		!use.CheckedAt.Equal(time.Date(2026, 9, 29, 18, 2, 11, 0, time.UTC)) {
		t.Errorf("checked at = %v", use.CheckedAt)
	}
}

func TestAToolUseShortensALongResult(t *testing.T) {
	type snapshot struct {
		Name string `json:"name"`
		Used int64  `json:"used_bytes"`
	}
	snaps := make([]snapshot, 2000)
	for i := range snaps {
		snaps[i] = snapshot{Name: fmt.Sprintf("tank/media@auto-%04d", i), Used: 9007199254740993}
	}
	full, _ := json.Marshal(map[string]any{"snapshots": snaps, "count": len(snaps)})
	if len(full) <= MaxToolResultBytes {
		t.Fatalf("test result is only %d bytes", len(full))
	}

	use := NewToolUse("list_snapshots", nil, string(full), false, checkedAt)
	if !use.Trimmed || len(use.Result) > MaxToolResultBytes || !json.Valid(use.Result) {
		t.Fatalf("trimmed = %v, %d bytes, valid = %v", use.Trimmed, len(use.Result), json.Valid(use.Result))
	}
	var kept struct {
		Count     int               `json:"count"`
		Snapshots []json.RawMessage `json:"snapshots"`
	}
	if err := json.Unmarshal(use.Result, &kept); err != nil {
		t.Fatalf("kept result: %v", err)
	}
	if kept.Count != 2000 || len(kept.Snapshots) < 2 {
		t.Fatalf("kept = %d snapshots, count %d", len(kept.Snapshots), kept.Count)
	}
	// The first entries are kept as they were, numbers included, and a
	// note says how many more there were.
	if first := string(kept.Snapshots[0]); first != `{"name":"tank/media@auto-0000","used_bytes":9007199254740993}` {
		t.Errorf("first snapshot = %s", first)
	}
	more := string(kept.Snapshots[len(kept.Snapshots)-1])
	if want := fmt.Sprintf(`"… and %d more"`, 2000-(len(kept.Snapshots)-1)); more != want {
		t.Errorf("last entry = %s, want %s", more, want)
	}
}

func TestAToolUseCutsLongText(t *testing.T) {
	log := strings.Repeat("é", MaxToolResultBytes)
	use := NewToolUse("app_logs", nil, fmt.Sprintf(`{"log":%q}`, log), false, checkedAt)
	var kept struct {
		Log string `json:"log"`
	}
	if err := json.Unmarshal(use.Result, &kept); err != nil || !use.Trimmed {
		t.Fatalf("use = %+v, err %v", use, err)
	}
	if !strings.HasSuffix(kept.Log, "é…") || len(kept.Log) >= len(log) {
		t.Errorf("log kept as %d bytes ending %q", len(kept.Log), kept.Log[len(kept.Log)-8:])
	}
}

func TestAToolUseMeasuresItsResultAsItWillBeWritten(t *testing.T) {
	// A tool passing TrueNAS's JSON through untouched could have < and &
	// unescaped; encoding/json writes each as six bytes.
	raw := `{"log":"` + strings.Repeat("<", MaxToolResultBytes-20) + `"}`
	use := NewToolUse("app_logs", nil, raw, false, checkedAt)
	frame, err := json.Marshal(use)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !use.Trimmed || len(use.Result) > MaxToolResultBytes || len(frame) > MaxToolResultBytes+200 {
		t.Errorf("trimmed = %v, result %d bytes, written as %d", use.Trimmed, len(use.Result), len(frame))
	}
}

func TestAToolUseDropsAResultItCantShorten(t *testing.T) {
	// Only lists and text are shortened, so a huge object can't be.
	settings := map[string]int{}
	for i := 0; i < 5000; i++ {
		settings[fmt.Sprintf("setting_%04d", i)] = i
	}
	full, _ := json.Marshal(settings)
	use := NewToolUse("security_settings", nil, string(full), false, checkedAt)
	if use.Result != nil || !use.Trimmed {
		t.Errorf("use kept %d bytes, trimmed = %v; want it dropped", len(use.Result), use.Trimmed)
	}
}

func TestAToolUseKeepsTextThatIsntJSON(t *testing.T) {
	use := NewToolUse("list_pools", nil, "upstream said no", true, checkedAt)
	if string(use.Result) != `"upstream said no"` || !use.Failed {
		t.Errorf("use = %+v", use)
	}
}

func TestAToolUseKeepsOnlyRealArguments(t *testing.T) {
	cases := map[string]string{
		``:                         "",
		`null`:                     "",
		`{}`:                       "",
		`[1]`:                      "",
		`{"pool_name":`:            "",
		`{ "pool_name" : "tank" }`: `{"pool_name":"tank"}`,
		`{"dataset":"` + strings.Repeat("x", maxToolArgumentsBytes) + `"}`: "",
	}
	for args, want := range cases {
		use := NewToolUse("pool_status", json.RawMessage(args), `{}`, false, checkedAt)
		if string(use.Arguments) != want {
			t.Errorf("arguments %.30q kept as %s, want %q", args, use.Arguments, want)
		}
	}
}

func TestChatsSavedBeforeToolResultsStillOpen(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	c, _ := store.Start(ctx, "Old chat")
	if _, err := store.db.Exec(`INSERT INTO messages (conversation_id, seq, role, content, tools, hidden, created_at)
VALUES (?, 1, 'assistant', 'tank is fine.', '["list_pools","pool_status"]', 0, 0)`, c.ID); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := store.Get(ctx, c.ID)
	if err != nil || len(got.Messages) != 1 {
		t.Fatalf("get = %+v, %v", got, err)
	}
	tools := got.Messages[0].Tools
	if fmt.Sprint(ToolNames(tools)) != "[list_pools pool_status]" || tools[0].Result != nil || tools[0].CheckedAt != nil {
		t.Errorf("tools = %+v", tools)
	}
}
