package actions

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/tools"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

type noJobs struct{}

func (noJobs) WaitJob(ctx context.Context, id int64, _ time.Duration) (truenas.Job, error) {
	return truenas.Job{ID: id}, truenas.ErrJobGone
}

func openLog(t *testing.T) (*Log, *events.Store) {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(context.Background(), store, noJobs{}), store
}

// A job an action saw through, or that failed while it waited, is recorded
// once, as the action: the jobs poller leaves it alone.
func TestJobsAnActionSawThroughAreLeftToItsEntry(t *testing.T) {
	log, store := openLog(t)
	card := tools.Card{Title: "Start Plex", Risk: tools.RiskRoutine}
	log.Record(context.Background(), Entry{ID: "a", Tool: "app_action", Card: card,
		Err: fmt.Errorf("it didn't start: %w", tools.JobFailure(101, "Failed 'up' action for 'plex' app"))})
	log.Record(context.Background(), Entry{ID: "b", Tool: "run_backup_task", Card: card,
		Outcome: tools.Outcome{Message: "The cloud sync “Offsite” ran.", Ran: []int64{102}}})
	log.Record(context.Background(), Entry{ID: "c", Tool: "app_action", Card: card, Err: errors.New("no job here")})

	for _, id := range []int64{101, 102} {
		if !log.Following(id) {
			t.Errorf("job %d isn't left to the action's entry", id)
		}
	}
	if log.Following(103) {
		t.Error("an unrelated job is skipped")
	}
	evs, err := store.List(context.Background(), events.ListFilter{})
	if err != nil || len(evs) != 3 {
		t.Fatalf("events = %v, %v", evs, err)
	}
}

// A job TrueNAS stops listing ends the follow without an entry.
func TestAJobThatVanishesEndsTheFollow(t *testing.T) {
	log, store := openLog(t)
	log.Record(context.Background(), Entry{ID: "a", Tool: "update_app", Card: tools.Card{Title: "Update Plex"},
		Outcome: tools.Outcome{Message: "Started updating Plex.", Job: &tools.Job{ID: 7, Done: "Updated Plex"}}})
	log.Wait()
	evs, _ := store.List(context.Background(), events.ListFilter{})
	if len(evs) != 1 {
		t.Errorf("events = %d, want only the start", len(evs))
	}
}
