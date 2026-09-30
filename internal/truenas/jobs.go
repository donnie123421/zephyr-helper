package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Job is a TrueNAS job, as core.get_jobs reports it.
type Job struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	State  string          `json:"state"`
	Result json.RawMessage `json:"result"`
	// Error is TrueNAS's message when the job failed.
	Error    string `json:"error"`
	Progress struct {
		Percent     *float64 `json:"percent"`
		Description string   `json:"description"`
	} `json:"progress"`
}

// Finished reports whether the job has ended, however it went.
func (j Job) Finished() bool {
	switch strings.ToUpper(j.State) {
	case "SUCCESS", "FAILED", "ABORTED":
		return true
	}
	return false
}

// Succeeded reports whether the job ended well.
func (j Job) Succeeded() bool { return strings.EqualFold(j.State, "SUCCESS") }

// Failure is why the job didn't succeed, in one line.
func (j Job) Failure() string {
	if strings.EqualFold(j.State, "ABORTED") {
		return "It was cancelled."
	}
	message := tidyReason(j.Error)
	if message == "" {
		return "TrueNAS didn't say why."
	}
	return message
}

// ErrJobGone: TrueNAS no longer lists the job. Transient jobs go the
// moment they finish, and TrueNAS keeps only its last thousand or so, so
// the job has ended, and how isn't known.
var ErrJobGone = errors.New("TrueNAS no longer lists the job")

// GetJob fetches one job by its id.
func (c *Client) GetJob(ctx context.Context, id int64) (Job, error) {
	raw, err := c.Call(ctx, "core.get_jobs", []any{[]any{"id", "=", id}})
	if err != nil {
		return Job{}, err
	}
	var jobs []Job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return Job{}, fmt.Errorf("decode job %d: %w", id, err)
	}
	if len(jobs) == 0 {
		return Job{}, fmt.Errorf("job %d: %w", id, ErrJobGone)
	}
	return jobs[0], nil
}

// WaitJob polls a job every interval until it finishes or ctx ends, and
// returns its last state either way: a job still running when ctx ends
// comes back unfinished, with ctx's error, and one TrueNAS stops listing
// comes back with ErrJobGone.
func (c *Client) WaitJob(ctx context.Context, id int64, interval time.Duration) (Job, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	last := Job{ID: id}
	for {
		job, err := c.GetJob(ctx, id)
		switch {
		case err == nil:
			last = job
			if job.Finished() {
				return job, nil
			}
		case errors.Is(err, ErrJobGone):
			return last, err
		}
		// Any other failed poll waits for the next: a dropped connection
		// reconnects on the next call.
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-ticker.C:
		}
	}
}

// JobID reads the job id a job method returns. The null a plain method
// returns isn't one.
func JobID(raw json.RawMessage) (int64, error) {
	var id int64
	if err := json.Unmarshal(raw, &id); err != nil || id <= 0 {
		return 0, fmt.Errorf("expected a job id, got %s", truncate(raw, 80))
	}
	return id, nil
}

func truncate(raw json.RawMessage, n int) string {
	if len(raw) <= n {
		return string(raw)
	}
	return string(raw[:n]) + "…"
}
