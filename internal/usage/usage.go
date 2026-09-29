// Package usage counts the tokens the assistant's hosted models use, so the
// app can show a month's tokens next to a link to the provider's own usage
// page. Prices change, so none are kept here: the provider's page has the
// cost.
package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Retention keeps a little over a year of counts. They're a few numbers
// per answer, so the table never matters.
const Retention = 400 * 24 * time.Hour

// Tokens are what one answer used. Input leaves out the prompt read from
// or written to the provider's cache, which is billed differently.
type Tokens struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// IsZero reports whether nothing was counted, as with Ollama.
func (t Tokens) IsZero() bool { return t == Tokens{} }

// Total is what one model used over a period.
type Total struct {
	Provider string
	Model    string
	Tokens
	// Answers counts the turns that used tokens.
	Answers int64
}

// Store keeps the counts in their own SQLite file.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open prepares the SQLite file at path. Pass ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// ":memory:" gives every connection its own database; one connection
	// keeps tests (and this low-traffic table) on a single one.
	db.SetMaxOpenConns(1)

	const schema = `
CREATE TABLE IF NOT EXISTS token_usage (
    at                 INTEGER NOT NULL,
    provider           TEXT NOT NULL,
    model              TEXT NOT NULL,
    input_tokens       INTEGER NOT NULL,
    output_tokens      INTEGER NOT NULL,
    cache_read_tokens  INTEGER NOT NULL,
    cache_write_tokens INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS token_usage_by_time ON token_usage (at);`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the underlying DB.
func (s *Store) Close() error { return s.db.Close() }

// Record counts one answer's tokens, and forgets counts past Retention.
func (s *Store) Record(ctx context.Context, provider, model string, t Tokens) error {
	if t.IsZero() {
		return nil
	}
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO token_usage (at, provider, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		now.Unix(), provider, model, t.Input, t.Output, t.CacheRead, t.CacheWrite); err != nil {
		return fmt.Errorf("record: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM token_usage WHERE at < ?`, now.Add(-Retention).Unix()); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	return nil
}

// Since totals each model's tokens from since onwards, the most used
// first.
func (s *Store) Since(ctx context.Context, since time.Time) ([]Total, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT provider, model, SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens),
       SUM(cache_write_tokens), COUNT(*)
FROM token_usage
WHERE at >= ?
GROUP BY provider, model
ORDER BY SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens) DESC, provider, model`,
		since.Unix())
	if err != nil {
		return nil, fmt.Errorf("totals: %w", err)
	}
	defer rows.Close()

	out := []Total{}
	for rows.Next() {
		var t Total
		if err := rows.Scan(&t.Provider, &t.Model, &t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.Answers); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
