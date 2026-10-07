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
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// MaxJobs bounds what can be booked at once.
const MaxJobs = 50

// keepFinished is how long a one-off that's done with stays listed.
const keepFinished = 30 * 24 * time.Hour

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
	// Held is a pause Zephyr Pro lapsing made (Hold), lifted when it's
	// back (Release). A pause made by hand isn't, and outlasts it.
	Held    bool       `json:"held"`
	LastRun *time.Time `json:"last_run,omitempty"`
	// LastStatus is running, done, started, failed, skipped, missed or
	// interrupted (the helper stopped partway through a run).
	LastStatus  string `json:"last_status,omitempty"`
	LastMessage string `json:"last_message,omitempty"`
}

// Store keeps the jobs in their own SQLite file.
type Store struct {
	db *sql.DB
	// holding serialises deciding whether Zephyr Pro has lapsed and
	// holding or releasing to match (Converge), so a change made between
	// the two isn't undone.
	holding sync.Mutex
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
    held         INTEGER NOT NULL DEFAULT 0,
    last_run     INTEGER,
    last_status  TEXT NOT NULL DEFAULT '',
    last_message TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_next_run ON jobs(next_run);`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	// Files from before held (0.3.0-rc.13) gain it.
	if _, err := db.Exec(`ALTER TABLE jobs ADD COLUMN held INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		_ = db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Add books a job, giving it an id. One-offs done with a while ago go, and
// don't count towards MaxJobs before then.
func (s *Store) Add(ctx context.Context, job Job) (Job, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE next_run IS NULL AND COALESCE(last_run, created_at) < ?`,
		time.Now().Add(-keepFinished).UnixMilli()); err != nil {
		return Job{}, err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE next_run IS NOT NULL`).Scan(&count); err != nil {
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

const columns = `id, kind, tool, arguments, title, detail, risk, when_json, created_at, next_run, paused, last_run, last_status, last_message, held`

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

// Pause stops a job running until it's resumed. A pause made by hand
// outlasts a lapse of Zephyr Pro, so it's no longer the lapse's.
func (s *Store) Pause(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE jobs SET paused = 1, held = 0 WHERE id = ?`, id)
}

// Resume lets a job run again, next at next.
func (s *Store) Resume(ctx context.Context, id string, next time.Time) error {
	return s.exec(ctx, `UPDATE jobs SET paused = 0, held = 0, next_run = ? WHERE id = ?`, next.UnixMilli(), id)
}

// passedWhileHeld is why a one-off held past its time didn't run.
const passedWhileHeld = "It was due while Zephyr Pro was off, so it didn't run."

// Hold pauses everything booked that isn't paused already, as Zephyr Pro
// lapsing does, marking the pauses as the lapse's so that Release lifts
// those and not a pause made by hand. It says how many it paused.
func (s *Store) Hold(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET paused = 1, held = 1 WHERE paused = 0 AND next_run IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Release lifts the lapse's pauses, each job carrying on from its next
// time after now: a repeating one skips the runs the lapse kept from it,
// and a one-off whose time passed meanwhile is done with, as skipped. It
// says how many carry on, and how many were done with.
func (s *Store) Release(ctx context.Context, now time.Time) (resumed, expired int, err error) {
	jobs, err := s.query(ctx, `SELECT `+columns+` FROM jobs WHERE held = 1`)
	if err != nil {
		return 0, 0, err
	}
	for _, job := range jobs {
		var res sql.Result
		next, due := job.When.Next(now)
		if due {
			res, err = s.db.ExecContext(ctx, `UPDATE jobs SET paused = 0, held = 0, next_run = ? WHERE id = ? AND held = 1`,
				next.UnixMilli(), job.ID)
		} else {
			res, err = s.db.ExecContext(ctx, `
UPDATE jobs SET paused = 0, held = 0, next_run = NULL, last_run = ?, last_status = 'skipped', last_message = ?
WHERE id = ? AND held = 1`, now.UnixMilli(), passedWhileHeld, job.ID)
		}
		if err != nil {
			return resumed, expired, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			if due {
				resumed++
			} else {
				expired++
			}
		}
	}
	return resumed, expired, nil
}

// Converge holds everything booked while lapsed() says Zephyr Pro has
// lapsed, and releases the lapse's pauses once it doesn't, reading it and
// acting under one lock: the scheduler's tick and the app's word that Pro
// is back can't undo each other. It says how many it paused, let carry on,
// and found done with.
func (s *Store) Converge(ctx context.Context, lapsed func() bool, now time.Time) (paused, resumed, expired int, err error) {
	s.holding.Lock()
	defer s.holding.Unlock()
	if lapsed() {
		paused, err = s.Hold(ctx)
		return paused, 0, 0, err
	}
	if n, err := s.HeldCount(ctx); err != nil || n == 0 {
		return 0, 0, 0, err
	}
	resumed, expired, err = s.Release(ctx, now)
	return 0, resumed, expired, err
}

// HeldCount is how many jobs the lapse has paused.
func (s *Store) HeldCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE held = 1`).Scan(&n)
	return n, err
}

// Replace changes what a job does and when, as an edit approved on a new
// card does. It runs next at job.NextRun, keeping its history and whether
// it's paused.
func (s *Store) Replace(ctx context.Context, id string, job Job) error {
	if len(job.Arguments) == 0 {
		job.Arguments = json.RawMessage(`{}`)
	}
	when, err := json.Marshal(job.When)
	if err != nil {
		return err
	}
	return s.exec(ctx, `
UPDATE jobs SET kind = ?, tool = ?, arguments = ?, title = ?, detail = ?, risk = ?, when_json = ?, next_run = ?
WHERE id = ?`, string(job.Kind), job.Tool, string(job.Arguments), job.Title, job.Detail, job.Risk, string(when),
		millis(job.NextRun), id)
}

// Delete removes a job.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.exec(ctx, `DELETE FROM jobs WHERE id = ?`, id)
}

// Claim takes a due job's run, booking its next (nil when there won't be
// one) before it starts, so a restart partway through doesn't run it again.
// It's false when the run isn't there to take: the job was paused, moved
// or removed since it was read.
func (s *Store) Claim(ctx context.Context, id string, due, at time.Time, next *time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE jobs SET next_run = ?, last_run = ?, last_status = 'running', last_message = ''
WHERE id = ? AND paused = 0 AND next_run = ?`, millis(next), at.UnixMilli(), id, due.UnixMilli())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Started notes a run begun out of turn, as Run Now does, leaving the
// schedule as it is.
func (s *Store) Started(ctx context.Context, id string, at time.Time) error {
	return s.exec(ctx, `UPDATE jobs SET last_run = ?, last_status = 'running', last_message = '' WHERE id = ?`,
		at.UnixMilli(), id)
}

// Finish records how a run went.
func (s *Store) Finish(ctx context.Context, id, status, message string) error {
	return s.exec(ctx, `UPDATE jobs SET last_status = ?, last_message = ? WHERE id = ?`, status, message, id)
}

// Miss records a run missed at due, and books the next, nil when there
// won't be one. It's false when the job changed since it was read.
func (s *Store) Miss(ctx context.Context, id string, due, at time.Time, message string, next *time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE jobs SET next_run = ?, last_run = ?, last_status = 'missed', last_message = ?
WHERE id = ? AND paused = 0 AND next_run = ?`, millis(next), at.UnixMilli(), message, id, due.UnixMilli())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Unfinished is the jobs whose run never finished: the helper stopped
// partway through.
func (s *Store) Unfinished(ctx context.Context) ([]Job, error) {
	return s.query(ctx, `SELECT `+columns+` FROM jobs WHERE last_status = 'running'`)
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
			&created, &next, &job.Paused, &last, &job.LastStatus, &job.LastMessage, &job.Held); err != nil {
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
