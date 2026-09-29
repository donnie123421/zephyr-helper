package conversations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "conversations.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// tick gives the store a clock that moves a minute every time it's read.
func tick(store *Store) {
	now := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	store.now = func() time.Time {
		now = now.Add(time.Minute)
		return now
	}
}

func mustAppend(t *testing.T, store *Store, id string, m Message) {
	t.Helper()
	if err := store.Append(context.Background(), id, m); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func TestConversationRoundTrip(t *testing.T) {
	store := openStore(t)
	tick(store)
	ctx := context.Background()

	started, err := store.Start(ctx, "How to Fix: SSH Root Login")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	mustAppend(t, store, started.ID, Message{Role: RoleUser, Content: "Zephyr's audit flagged…", Hidden: true})
	mustAppend(t, store, started.ID, Message{Role: RoleAssistant, Content: "Remove root's key.", Tools: []string{"list_users", "security_settings"}})
	mustAppend(t, store, started.ID, Message{Role: RoleUser, Content: "Will that lock me out?"})
	if err := store.RecordTurn(ctx, started.ID, "anthropic", "claude-opus-5", 120, 30); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := store.RecordTurn(ctx, started.ID, "anthropic", "claude-opus-5", 80, 10); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := store.Get(ctx, started.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "How to Fix: SSH Root Login" || got.Provider != "anthropic" || got.Model != "claude-opus-5" {
		t.Errorf("summary = %+v", got.Summary)
	}
	if got.InputTokens != 200 || got.OutputTokens != 40 {
		t.Errorf("tokens = %d in, %d out; want 200, 40", got.InputTokens, got.OutputTokens)
	}
	if got.MessageCount != 2 {
		t.Errorf("message count = %d, want 2 (hidden context isn't counted)", got.MessageCount)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	if m := got.Messages[0]; m.Role != RoleUser || !m.Hidden {
		t.Errorf("first message = %+v, want hidden user context", m)
	}
	if m := got.Messages[1]; m.Content != "Remove root's key." || fmt.Sprint(m.Tools) != "[list_users security_settings]" {
		t.Errorf("reply = %+v", m)
	}
	if m := got.Messages[2]; m.Content != "Will that lock me out?" || m.Hidden || len(m.Tools) != 0 || m.Tools == nil {
		t.Errorf("follow-up = %+v", m)
	}
	if !got.UpdatedAt.After(got.CreatedAt) {
		t.Errorf("updated %v isn't after created %v", got.UpdatedAt, got.CreatedAt)
	}
}

func TestListIsMostRecentlyActiveFirst(t *testing.T) {
	store := openStore(t)
	tick(store)
	ctx := context.Background()

	older, _ := store.Start(ctx, "older")
	newer, _ := store.Start(ctx, "newer")
	mustAppend(t, store, older.ID, Message{Role: RoleUser, Content: "back again"})

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].ID != older.ID || list[1].ID != newer.ID {
		t.Fatalf("list = %+v, want the chat with the latest message first", list)
	}
	if list[0].MessageCount != 1 || list[1].MessageCount != 0 {
		t.Errorf("counts = %d, %d", list[0].MessageCount, list[1].MessageCount)
	}
}

func TestEmptyListIsNotNil(t *testing.T) {
	list, err := openStore(t).List(context.Background())
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("list = %#v, %v; want an empty slice", list, err)
	}
}

func TestAppendingToADeletedConversationLeavesNothingBehind(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	c, _ := store.Start(ctx, "gone")
	if err := store.Delete(ctx, c.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Append(ctx, c.ID, Message{Role: RoleUser, Content: "hello?"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("append = %v, want ErrNotFound", err)
	}
	if err := store.RecordTurn(ctx, c.ID, "ollama", "qwen3", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record = %v, want ErrNotFound", err)
	}
	var orphans int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("orphaned messages = %d (%v)", orphans, err)
	}
}

func TestStartKeepsTheMostRecentlyActive(t *testing.T) {
	store := openStore(t)
	tick(store)
	store.keep = 3
	ctx := context.Background()

	a, _ := store.Start(ctx, "a")
	mustAppend(t, store, a.ID, Message{Role: RoleUser, Content: "first"})
	b, _ := store.Start(ctx, "b")
	mustAppend(t, store, b.ID, Message{Role: RoleUser, Content: "idle"})
	_, _ = store.Start(ctx, "c")
	// a is the oldest chat but the most recently used, so it stays.
	mustAppend(t, store, a.ID, Message{Role: RoleUser, Content: "still going"})
	_, _ = store.Start(ctx, "d")

	list, _ := store.List(ctx)
	var ids []string
	for _, s := range list {
		ids = append(ids, s.Title)
	}
	if fmt.Sprint(ids) != "[d a c]" {
		t.Fatalf("kept %v, want [d a c]", ids)
	}
	if _, err := store.Get(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("b should be gone, got %v", err)
	}
	var leftover int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, b.ID).Scan(&leftover)
	if leftover != 0 {
		t.Errorf("b's messages survived: %d", leftover)
	}
}

func TestRenameAndDelete(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	c, _ := store.Start(ctx, "")
	if c.Title != "New chat" {
		t.Errorf("untitled chat = %q", c.Title)
	}

	renamed, err := store.Rename(ctx, c.ID, "  Pool   plans \n")
	if err != nil || renamed.Title != "Pool plans" {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
	if _, err := store.Rename(ctx, c.ID, "   "); err == nil {
		t.Error("renaming to nothing should fail")
	}
	if _, err := store.Rename(ctx, "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename missing = %v", err)
	}
	if err := store.Delete(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing = %v", err)
	}

	_, _ = store.Start(ctx, "another")
	n, err := store.DeleteAll(ctx)
	if err != nil || n != 2 {
		t.Fatalf("delete all = %d, %v", n, err)
	}
	if list, _ := store.List(ctx); len(list) != 0 {
		t.Errorf("left after delete all: %+v", list)
	}
}

func TestTitles(t *testing.T) {
	long := strings.Repeat("word ", 30)
	cases := []struct {
		name, in, want string
	}{
		{"first line, spaces collapsed", "  How big is   tank?\nand more", "How big is tank?"},
		{"skips blank lines", "\n \n  Is my pool healthy?", "Is my pool healthy?"},
		{"nothing to use", " \n ", ""},
	}
	for _, c := range cases {
		if got := TitleFrom(c.in); got != c.want {
			t.Errorf("%s: TitleFrom(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}

	cut := TitleFrom(long)
	if !strings.HasSuffix(cut, "word…") || utf8.RuneCountInString(cut) > maxTitleRunes+1 {
		t.Errorf("long title cut to %q", cut)
	}
	unbroken := CleanTitle(strings.Repeat("é", 200))
	if utf8.RuneCountInString(unbroken) != maxTitleRunes+1 || !strings.HasSuffix(unbroken, "é…") {
		t.Errorf("unbroken title cut to %q", unbroken)
	}
}

func TestTheFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
}

func TestStartNeverPrunesTheNewChat(t *testing.T) {
	store := openStore(t)
	store.keep = 2
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	_, _ = store.Start(ctx, "a")
	now = now.Add(time.Hour)
	_, _ = store.Start(ctx, "b")

	// The clock steps back, so the new chat looks the oldest.
	now = now.Add(-3 * time.Hour)
	c, err := store.Start(ctx, "c")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := store.Get(ctx, c.ID); err != nil {
		t.Fatalf("the new chat was pruned: %v", err)
	}
	if list, _ := store.List(ctx); len(list) != 2 {
		t.Errorf("kept %d chats, want 2", len(list))
	}
}

func TestDeletedTextIsGoneFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	c, _ := store.Start(ctx, "oops")
	secret := "my-pasted-password-" + strings.Repeat("x", 40)
	mustAppend(t, store, c.ID, Message{Role: RoleUser, Content: secret})
	mustAppend(t, store, c.ID, Message{Role: RoleAssistant, Content: "Don't paste passwords here."})
	if err := store.Delete(ctx, c.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = store.Close()

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if strings.Contains(string(data), secret) {
			t.Errorf("%s still holds the deleted text", e.Name())
		}
	}
}
