package schedule

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// MaxJobs bounds what can be booked at once.
const MaxJobs = 50

// ErrNotFound: no job has that id.
var ErrNotFound = errors.New("schedule: no such job")

// Kind is what a job does.
type Kind string

const (
	// KindAction runs one of the assistant's actions.
	KindAction Kind = "action"
	// KindUpdateRule updates apps that have updates, with a safeguard.
	KindUpdateRule Kind = "update_rule"
	// KindCheckIn has the model look the NAS over and write a briefing.
	KindCheckIn Kind = "checkin"
)

// Job is something booked, as the card that booked it described it.
type Job struct {
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	// Tool and Arguments are the action and what it was asked, for an
	// action; Arguments holds a rule's settings.
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Title, Detail and Risk are the card it was approved on.
	Title     string    `json:"title"`
	Detail    string    `json:"detail"`
	Risk      string    `json:"risk"`
	When      When      `json:"when"`
	CreatedAt time.Time `json:"created_at"`
	// NextRun is nil once a one-off has run, or been missed.
	NextRun *time.Time `json:"next_run,omitempty"`
	Paused  bool       `json:"paused"`
	LastRun *time.Time `json:"last_run,omitempty"`
	// LastStatus is done, started, failed, skipped or missed.
	LastStatus  string `json:"last_status,omitempty"`
	LastMessage string `json:"last_message,omitempty"`
}

// Store keeps the jobs in their own SQLite file.
type Store struct {
	db *sql.DB
}

// Open prepares the SQLite file at path. Pass ":memory:" for tests.
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
		_ = f.Close()
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    tool         TEXT NOT NULL DEFAULT '',
    arguments    TEXT NOT NULL DEFAULT '{}',
    title        TEXT NOT NULL,
    detail       TEXT NOT NULL,
    risk         TEXT NOT NULL,
    when_json    TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    next_run     INTEGER,
    paused       INTEGER NOT NULL DEFAULT 0,
    last_run     INTEGER,
    last_status  TEXT NOT NULL DEFAULT '',
    last_message TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_next_run ON jobs(next_run);`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Add books a job, giving it an id.
func (s *Store) Add(ctx context.Context, job Job) (Job, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil {
		return Job{}, err
	}
	if count >= MaxJobs {
		return Job{}, fmt.Errorf("there are already %d things booked; remove some in Scheduled first", count)
	}
	if job.ID == "" {
		job.ID = newID()
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now()
	}
	if len(job.Arguments) == 0 {
		job.Arguments = json.RawMessage(`{}`)
	}
	when, err := json.Marshal(job.When)
	if err != nil {
		return Job{}, err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO jobs (id, kind, tool, arguments, title, detail, risk, when_json, created_at, next_run, paused)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, string(job.Kind), job.Tool, string(job.Arguments), job.Title, job.Detail, job.Risk,
		string(when), job.CreatedAt.UnixMilli(), millis(job.NextRun), job.Paused)
	if err != nil {
		return Job{}, fmt.Errorf("add job: %w", err)
	}
	return job, nil
}

const columns = `id, kind, tool, arguments, title, detail, risk, when_json, created_at, next_run, paused, last_run, last_status, last_message`

// List is every job, the next to run first, then those done.
func (s *Store) List(ctx context.Context) ([]Job, error) {
	return s.query(ctx, `SELECT `+columns+` FROM jobs ORDER BY next_run IS NULL, next_run, created_at`)
}

// Due is the jobs not paused whose next run is at or before now.
func (s *Store) Due(ctx context.Context, now time.Time) ([]Job, error) {
	return s.query(ctx, `SELECT `+columns+` FROM jobs WHERE paused = 0 AND next_run IS NOT NULL AND next_run <= ? ORDER BY next_run`,
		now.UnixMilli())
}

// Get finds one job.
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	jobs, err := s.query(ctx, `SELECT `+columns+` FROM jobs WHERE id = ?`, id)
	if err != nil {
		return Job{}, err
	}
	if len(jobs) == 0 {
		return Job{}, ErrNotFound
	}
	return jobs[0], nil
}

// SetPaused pauses or resumes a job, with its next run when resumed.
func (s *Store) SetPaused(ctx context.Context, id string, paused bool, next *time.Time) error {
	return s.exec(ctx, `UPDATE jobs SET paused = ?, next_run = ? WHERE id = ?`, paused, millis(next), id)
}

// Delete removes a job.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.exec(ctx, `DELETE FROM jobs WHERE id = ?`, id)
}

// Finish records a run and the next, nil when there won't be one.
func (s *Store) Finish(ctx context.Context, id string, ran time.Time, status, message string, next *time.Time) error {
	return s.exec(ctx, `UPDATE jobs SET last_run = ?, last_status = ?, last_message = ?, next_run = ? WHERE id = ?`,
		ran.UnixMilli(), status, message, millis(next), id)
}

func (s *Store) exec(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) query(ctx context.Context, query string, args ...any) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var (
			job              Job
			kind, args, when string
			created          int64
			next, last       sql.NullInt64
		)
		if err := rows.Scan(&job.ID, &kind, &job.Tool, &args, &job.Title, &job.Detail, &job.Risk, &when,
			&created, &next, &job.Paused, &last, &job.LastStatus, &job.LastMessage); err != nil {
			return nil, err
		}
		job.Kind = Kind(kind)
		job.Arguments = json.RawMessage(args)
		_ = json.Unmarshal([]byte(when), &job.When)
		job.CreatedAt = time.UnixMilli(created)
		job.NextRun = timeOf(next)
		job.LastRun = timeOf(last)
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func millis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func timeOf(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64)
	return &t
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
