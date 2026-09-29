// Package conversations keeps the assistant's chats on the NAS, so the app
// can list them, reopen one and carry on where it left off.
//
// A chat is stored as the text both sides saw, not in a provider's own
// transcript format. Reopening one replays that text to whichever model is
// current, so a chat started on Ollama can carry on with Claude. What the
// tools returned isn't kept: the model looks again if it needs to.
package conversations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultKeep is how many chats the store holds. Starting one more deletes
// whichever has gone longest without a message.
const DefaultKeep = 500

// maxTitleRunes bounds a title, whether the app sent it or it came from the
// first message.
const maxTitleRunes = 80

// ErrNotFound means there's no conversation with that ID, usually because
// it was deleted.
var ErrNotFound = errors.New("conversation not found")

// Roles a message can have.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message is one side of a chat.
type Message struct {
	Role    string
	Content string
	// Tools are the tools the assistant ran for this reply, in order.
	Tools []string
	// Hidden marks context the app sent as the first message and doesn't
	// show: an alert, a log excerpt, the audit's findings. The model still
	// sees it when the chat is reopened.
	Hidden    bool
	CreatedAt time.Time
}

// Summary describes a conversation for the list.
type Summary struct {
	ID        string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
	// MessageCount leaves out hidden context.
	MessageCount int
	// Provider and Model answered most recently.
	Provider string
	Model    string
}

// Conversation is a whole chat.
type Conversation struct {
	Summary
	// Tokens used across the chat, for hosted models that count them.
	// Input includes cached reads and writes.
	InputTokens  int64
	OutputTokens int64
	Messages     []Message
}

// Store keeps conversations in their own SQLite file.
type Store struct {
	db   *sql.DB
	keep int
	now  func() time.Time
}

// Open prepares the SQLite file at path. Pass ":memory:" for tests.
func Open(path string) (*Store, error) {
	// Chats hold whatever people type, so the file is private to the
	// helper. SQLite gives its journal files the same permissions.
	if path != ":memory:" {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
		_ = f.Close()
	}
	// secure_delete zeroes what a deleted chat leaves behind, since people
	// delete a chat because they pasted something they shouldn't have.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=secure_delete(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One connection: ":memory:" gives each connection its own database,
	// and chats are too little traffic to need more.
	db.SetMaxOpenConns(1)

	const schema = `
CREATE TABLE IF NOT EXISTS conversations (
    id            TEXT PRIMARY KEY,
    title         TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    provider      TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS conversations_by_update ON conversations (updated_at);
CREATE TABLE IF NOT EXISTS messages (
    conversation_id TEXT NOT NULL,
    seq             INTEGER NOT NULL,
    role            TEXT NOT NULL,
    content         TEXT NOT NULL,
    tools           TEXT NOT NULL DEFAULT '[]',
    hidden          INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    PRIMARY KEY (conversation_id, seq)
);`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, keep: DefaultKeep, now: time.Now}, nil
}

// Close closes the underlying DB.
func (s *Store) Close() error { return s.db.Close() }

// Start creates an empty conversation. An empty title becomes "New chat".
func (s *Store) Start(ctx context.Context, title string) (Summary, error) {
	id, err := newID()
	if err != nil {
		return Summary{}, err
	}
	title = CleanTitle(title)
	if title == "" {
		title = "New chat"
	}
	now := s.now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Summary{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO conversations (id, title, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, title, now.UnixMilli(), now.UnixMilli()); err != nil {
		return Summary{}, fmt.Errorf("start: %w", err)
	}
	// Past the limit, the chats idle longest go, never the new one (a clock
	// that stepped back could otherwise make it look the oldest).
	const stale = `SELECT id FROM conversations WHERE id != ?
ORDER BY updated_at DESC, created_at DESC LIMIT -1 OFFSET ?`
	others := max(s.keep-1, 0)
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE conversation_id IN (`+stale+`)`, id, others); err != nil {
		return Summary{}, fmt.Errorf("prune messages: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id IN (`+stale+`)`, id, others); err != nil {
		return Summary{}, fmt.Errorf("prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Summary{}, err
	}
	return Summary{ID: id, Title: title, CreatedAt: fromMillis(now.UnixMilli()), UpdatedAt: fromMillis(now.UnixMilli())}, nil
}

// Append adds a message to the end of a conversation.
func (s *Store) Append(ctx context.Context, id string, m Message) error {
	tools, err := json.Marshal(nonNil(m.Tools))
	if err != nil {
		return err
	}
	now := s.now().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Checked first, so a message for a deleted chat isn't left behind.
	res, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE id = ?`, now, id)
	if err != nil {
		return fmt.Errorf("append: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO messages (conversation_id, seq, role, content, tools, hidden, created_at)
SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ?, ?, ? FROM messages WHERE conversation_id = ?`,
		id, m.Role, m.Content, string(tools), m.Hidden, now, id); err != nil {
		return fmt.Errorf("append: %w", err)
	}
	return tx.Commit()
}

// DropReplies deletes whatever follows the conversation's last user
// message: an answer that's being written again.
func (s *Store) DropReplies(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
DELETE FROM messages
WHERE conversation_id = ?
  AND seq > (SELECT COALESCE(MAX(seq), 0) FROM messages WHERE conversation_id = ? AND role = 'user')`,
		id, id)
	if err != nil {
		return fmt.Errorf("drop replies: %w", err)
	}
	return nil
}

// RecordTurn notes which model answered and adds the tokens it used.
func (s *Store) RecordTurn(ctx context.Context, id, provider, model string, inputTokens, outputTokens int64) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE conversations
SET provider = ?, model = ?, input_tokens = input_tokens + ?, output_tokens = output_tokens + ?
WHERE id = ?`,
		provider, model, inputTokens, outputTokens, id)
	if err != nil {
		return fmt.Errorf("record turn: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const summaryColumns = `c.id, c.title, c.created_at, c.updated_at, c.provider, c.model,
    (SELECT COUNT(*) FROM messages m WHERE m.conversation_id = c.id AND m.hidden = 0)`

// List returns every conversation, most recently active first.
func (s *Store) List(ctx context.Context) ([]Summary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+summaryColumns+`
FROM conversations c ORDER BY c.updated_at DESC, c.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()

	out := []Summary{}
	for rows.Next() {
		summary, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, rows.Err()
}

// Get returns a conversation with all its messages, hidden ones included.
func (s *Store) Get(ctx context.Context, id string) (Conversation, error) {
	var c Conversation
	row := s.db.QueryRowContext(ctx, `SELECT `+summaryColumns+`, c.input_tokens, c.output_tokens
FROM conversations c WHERE c.id = ?`, id)
	var created, updated int64
	err := row.Scan(&c.ID, &c.Title, &created, &updated, &c.Provider, &c.Model, &c.MessageCount,
		&c.InputTokens, &c.OutputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("get: %w", err)
	}
	c.CreatedAt, c.UpdatedAt = fromMillis(created), fromMillis(updated)

	rows, err := s.db.QueryContext(ctx, `
SELECT role, content, tools, hidden, created_at FROM messages
WHERE conversation_id = ? ORDER BY seq`, id)
	if err != nil {
		return Conversation{}, fmt.Errorf("get messages: %w", err)
	}
	defer rows.Close()
	c.Messages = []Message{}
	for rows.Next() {
		var m Message
		var tools string
		var at int64
		if err := rows.Scan(&m.Role, &m.Content, &tools, &m.Hidden, &at); err != nil {
			return Conversation{}, err
		}
		if err := json.Unmarshal([]byte(tools), &m.Tools); err != nil || m.Tools == nil {
			m.Tools = []string{}
		}
		m.CreatedAt = fromMillis(at)
		c.Messages = append(c.Messages, m)
	}
	return c, rows.Err()
}

// Rename changes a conversation's title.
func (s *Store) Rename(ctx context.Context, id, title string) (Summary, error) {
	title = CleanTitle(title)
	if title == "" {
		return Summary{}, errors.New("title is empty")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE conversations SET title = ? WHERE id = ?`, title, id)
	if err != nil {
		return Summary{}, fmt.Errorf("rename: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Summary{}, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+summaryColumns+` FROM conversations c WHERE c.id = ?`, id)
	return scanSummary(row)
}

// Delete removes a conversation and its messages.
func (s *Store) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE conversation_id = ?`, id); err != nil {
		return fmt.Errorf("delete messages: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.flushDeleted(ctx)
	return nil
}

// DeleteAll removes every conversation and reports how many went.
func (s *Store) DeleteAll(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM conversations`)
	if err != nil {
		return 0, fmt.Errorf("delete all: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages`); err != nil {
		return 0, fmt.Errorf("delete all messages: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.flushDeleted(ctx)
	return res.RowsAffected()
}

// flushDeleted moves a deletion out of the write-ahead log and empties it,
// so earlier copies of the deleted pages don't linger there. A failure only
// means they go at the next automatic checkpoint.
func (s *Store) flushDeleted(ctx context.Context) {
	_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
}

// TitleFrom makes a title from the first thing someone asked: its first
// line, cut short at a word.
func TitleFrom(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if title := CleanTitle(line); title != "" {
			return title
		}
	}
	return ""
}

// CleanTitle puts a title on one line and cuts it short at a word.
func CleanTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	runes := []rune(title)
	if len(runes) <= maxTitleRunes {
		return title
	}
	cut := string(runes[:maxTitleRunes])
	if i := strings.LastIndex(cut, " "); i > len(cut)/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:-") + "…"
}

type scanner interface {
	Scan(dest ...any) error
}

func scanSummary(row scanner) (Summary, error) {
	var s Summary
	var created, updated int64
	err := row.Scan(&s.ID, &s.Title, &created, &updated, &s.Provider, &s.Model, &s.MessageCount)
	if errors.Is(err, sql.ErrNoRows) {
		return Summary{}, ErrNotFound
	}
	if err != nil {
		return Summary{}, err
	}
	s.CreatedAt, s.UpdatedAt = fromMillis(created), fromMillis(updated)
	return s, nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("conversation id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func nonNil(tools []string) []string {
	if tools == nil {
		return []string{}
	}
	return tools
}
