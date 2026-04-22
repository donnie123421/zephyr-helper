package events

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultMaxListLimit is applied when a caller passes a zero or absurdly
// large limit. A feed page of 200 is enough for infinite scroll, small
// enough that we never ship megabytes of JSON over the LAN.
const DefaultMaxListLimit = 200

// ErrNotFound is returned by Get + Ack when the event id is unknown.
var ErrNotFound = errors.New("events: not found")

// Store persists events in a SQLite database. Safe for concurrent use:
// all writes are serialized through a single *sql.DB handle and SQLite's
// own WAL mode; dedupe lookups and inserts run inside transactions so
// concurrent pollers can't double-insert the same key.
type Store struct {
	db *sql.DB

	// mu protects the subscriber slice. Live notifications are part of the
	// WS stream API — the Store fans out newly-inserted events to anyone
	// who's called Subscribe. We keep this tiny (no ring buffer yet; the
	// iOS client reconciles via GET /events on connect).
	mu   sync.Mutex
	subs []chan<- Event
}

// Open prepares the SQLite file at `path` for reads and writes. `path`
// should live inside the helper's ix-volume so the event history survives
// container restarts + upgrades. Pass `:memory:` for tests.
func Open(path string) (*Store, error) {
	// WAL + synchronous=NORMAL is the recommended combination for
	// write-heavy SQLite in a single-writer context; gives us durability
	// across crashes without fsync on every statement.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite only allows one writer at a time; keep the pool small to
	// avoid wasteful contention on locked writes.
	db.SetMaxOpenConns(4)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close flushes pending writes and closes the underlying DB.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS events (
    id            TEXT PRIMARY KEY,
    occurred_at   INTEGER NOT NULL,
    first_seen    INTEGER NOT NULL,
    last_seen     INTEGER NOT NULL,
    count         INTEGER NOT NULL DEFAULT 1,
    kind          TEXT NOT NULL,
    severity      TEXT NOT NULL,
    title         TEXT NOT NULL,
    summary       TEXT NOT NULL,
    body          BLOB,
    dedupe_key    TEXT NOT NULL,
    correlates_to TEXT,
    acked_at      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_events_dedupe ON events(dedupe_key, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_feed   ON events(occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_parent ON events(correlates_to);
`
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Ingest stores an event, merging into an existing row when its dedupe
// key matches one seen within `mergeWindow`. Returns the stored event
// (with its real id + counts) so callers can push it to live subscribers.
func (s *Store) Ingest(ctx context.Context, in Event, mergeWindow time.Duration) (Event, error) {
	if in.DedupeKey == "" {
		return Event{}, errors.New("events: dedupe_key required")
	}
	if in.OccurredAt.IsZero() {
		in.OccurredAt = time.Now().UTC()
	}
	if in.FirstSeen.IsZero() {
		in.FirstSeen = in.OccurredAt
	}
	in.LastSeen = in.OccurredAt
	if in.Count == 0 {
		in.Count = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var existing Event
	var existingOccurredMs, existingFirstMs, existingLastMs int64
	var existingAckedMs sql.NullInt64
	var existingParent sql.NullString
	err = tx.QueryRowContext(ctx, `
        SELECT id, occurred_at, first_seen, last_seen, count, kind, severity, title,
               summary, body, dedupe_key, correlates_to, acked_at
        FROM events
        WHERE dedupe_key = ? AND occurred_at >= ?
        ORDER BY occurred_at DESC
        LIMIT 1
    `, in.DedupeKey, in.OccurredAt.Add(-mergeWindow).UnixMilli()).Scan(
		&existing.ID, &existingOccurredMs, &existingFirstMs, &existingLastMs,
		&existing.Count, &existing.Kind, &existing.Severity, &existing.Title,
		&existing.Summary, &existing.Body, &existing.DedupeKey, &existingParent, &existingAckedMs,
	)

	switch {
	case err == sql.ErrNoRows:
		// Fresh row.
		if in.ID == "" {
			id, idErr := newID()
			if idErr != nil {
				return Event{}, idErr
			}
			in.ID = id
		}
		_, err := tx.ExecContext(ctx, `
            INSERT INTO events (id, occurred_at, first_seen, last_seen, count, kind,
                                severity, title, summary, body, dedupe_key, correlates_to, acked_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        `, in.ID, in.OccurredAt.UnixMilli(), in.FirstSeen.UnixMilli(), in.LastSeen.UnixMilli(),
			in.Count, string(in.Kind), string(in.Severity), in.Title, in.Summary, []byte(in.Body),
			in.DedupeKey, nullableString(in.CorrelatesTo), nullableMillis(in.AckedAt))
		if err != nil {
			return Event{}, fmt.Errorf("insert: %w", err)
		}

	case err == nil:
		// Merge: bump count, extend last_seen, keep first_seen.
		existing.OccurredAt = time.UnixMilli(existingOccurredMs).UTC()
		existing.FirstSeen = time.UnixMilli(existingFirstMs).UTC()
		existing.Count++
		existing.LastSeen = in.OccurredAt
		if existingParent.Valid {
			existing.CorrelatesTo = &existingParent.String
		}
		if existingAckedMs.Valid {
			t := time.UnixMilli(existingAckedMs.Int64).UTC()
			existing.AckedAt = &t
		}
		_, err := tx.ExecContext(ctx, `
            UPDATE events SET count = ?, last_seen = ?, body = ?
            WHERE id = ?
        `, existing.Count, existing.LastSeen.UnixMilli(), []byte(in.Body), existing.ID)
		if err != nil {
			return Event{}, fmt.Errorf("merge update: %w", err)
		}
		in = existing
		in.Body = json.RawMessage(in.Body)

	default:
		return Event{}, fmt.Errorf("dedupe lookup: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("commit: %w", err)
	}

	s.broadcast(in)
	return in, nil
}

// List returns events newest-first, subject to the filter. An empty
// filter returns up to DefaultMaxListLimit most-recent events.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Event, error) {
	limit := f.Limit
	if limit <= 0 || limit > DefaultMaxListLimit {
		limit = DefaultMaxListLimit
	}

	// Build the WHERE clause dynamically but with typed params — no user
	// input is spliced into the SQL string, so we're free of injection
	// risk even though the set of columns is known at compile time.
	clauses := []string{"1=1"}
	args := []any{}
	if !f.Since.IsZero() {
		clauses = append(clauses, "occurred_at > ?")
		args = append(args, f.Since.UnixMilli())
	}
	if f.Kind != "" {
		clauses = append(clauses, "kind = ?")
		args = append(args, string(f.Kind))
	}
	if f.Severity != "" {
		clauses = append(clauses, "severity = ?")
		args = append(args, string(f.Severity))
	}
	args = append(args, limit)

	query := `
        SELECT id, occurred_at, first_seen, last_seen, count, kind, severity,
               title, summary, body, dedupe_key, correlates_to, acked_at
        FROM events WHERE ` + joinClauses(clauses) + `
        ORDER BY occurred_at DESC LIMIT ?
    `

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get returns a single event by id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (Event, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT id, occurred_at, first_seen, last_seen, count, kind, severity,
               title, summary, body, dedupe_key, correlates_to, acked_at
        FROM events WHERE id = ?
    `, id)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	return e, err
}

// Ack marks an event acknowledged. Idempotent — acking an already-acked
// event is a no-op but still returns nil.
func (s *Store) Ack(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE events SET acked_at = ? WHERE id = ?`,
		at.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AckAll marks every un-acked event as acknowledged in a single UPDATE.
// Returns the number of rows the helper actually flipped so the caller
// can report it. Safe to call concurrently with ingest — SQLite
// serializes the write.
func (s *Store) AckAll(ctx context.Context, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE events SET acked_at = ? WHERE acked_at IS NULL`,
		at.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("ack all: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SetCorrelation links a child event to a parent. Idempotent —
// calling twice with the same pair is a no-op beyond the UPDATE. Returns
// ErrNotFound when the child doesn't exist. The parent is only
// referenced by id; a dangling parent is treated as application error,
// not data corruption.
func (s *Store) SetCorrelation(ctx context.Context, childID, parentID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE events SET correlates_to = ? WHERE id = ?`, parentID, childID)
	if err != nil {
		return fmt.Errorf("set correlation: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Prune deletes events older than `cutoff`. Safe to call concurrently with
// ingest; SQLite serializes the write. Returns the number of deleted rows
// so the caller can log it.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE occurred_at < ?`, cutoff.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Subscribe returns a channel that receives every newly-ingested event.
// The caller must drain it — a blocked subscriber will be skipped on the
// next ingest (we don't want a stalled WS client to back up every other
// writer). Call the returned cancel func to unsubscribe.
func (s *Store) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 16
	}
	ch := make(chan Event, buffer)
	s.mu.Lock()
	s.subs = append(s.subs, ch)
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, c := range s.subs {
			if c == ch {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				close(ch)
				return
			}
		}
	}
	return ch, cancel
}

func (s *Store) broadcast(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- e:
		default:
			// Subscriber backed up — drop this frame for them. They can
			// reconcile via GET /events when they catch up.
		}
	}
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(row rowScanner) (Event, error) {
	var (
		e                                          Event
		occurredMs, firstMs, lastMs                int64
		parent                                     sql.NullString
		ackedMs                                    sql.NullInt64
		kindStr, severityStr                       string
		body                                       []byte
	)
	if err := row.Scan(
		&e.ID, &occurredMs, &firstMs, &lastMs, &e.Count, &kindStr, &severityStr,
		&e.Title, &e.Summary, &body, &e.DedupeKey, &parent, &ackedMs,
	); err != nil {
		return Event{}, err
	}
	e.OccurredAt = time.UnixMilli(occurredMs).UTC()
	e.FirstSeen = time.UnixMilli(firstMs).UTC()
	e.LastSeen = time.UnixMilli(lastMs).UTC()
	e.Kind = Kind(kindStr)
	e.Severity = Severity(severityStr)
	if len(body) > 0 {
		e.Body = json.RawMessage(body)
	}
	if parent.Valid {
		e.CorrelatesTo = &parent.String
	}
	if ackedMs.Valid {
		t := time.UnixMilli(ackedMs.Int64).UTC()
		e.AckedAt = &t
	}
	return e, nil
}

func joinClauses(c []string) string {
	out := c[0]
	for _, s := range c[1:] {
		out += " AND " + s
	}
	return out
}

func nullableString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func nullableMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

// newID returns a compact time-sortable id. Not a strict ULID — we keep
// this internal to the helper and don't want a dependency just for this.
// Format: <unix-millis-hex>-<8 bytes hex>, 28 chars total.
func newID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("random id: %w", err)
	}
	return fmt.Sprintf("%012x-%s", time.Now().UnixMilli(), hex.EncodeToString(buf[:])), nil
}
