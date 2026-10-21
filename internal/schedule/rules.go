package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// ruleTiming is how long the update rule waits on TrueNAS.
type ruleTiming struct {
	// upgradeFor bounds an app's update job.
	upgradeFor time.Duration
	// runningFor is how long an updated app gets to be running again
	// before it's rolled back.
	runningFor time.Duration
	// settle is how long an app has to stay running to count: one that
	// crashes as it starts shows as running at first.
	settle time.Duration
	poll   time.Duration
	// catalogFor bounds refreshing TrueNAS's app catalog.
	catalogFor time.Duration
}

var defaultRuleTiming = ruleTiming{upgradeFor: 15 * time.Minute, runningFor: 5 * time.Minute, settle: 30 * time.Second,
	poll: 5 * time.Second, catalogFor: 5 * time.Minute}

// UpdateRule is an update rule's scope, fixed when it was approved: the
// apps it may update, or all of them.
type UpdateRule struct {
	Apps []string `json:"apps,omitempty"`
}

// appResult is what the rule did to one app.
type appResult struct {
	title, outcome string
	bad            bool
}

// runUpdateRule updates the apps in the rule's scope that have updates,
// one at a time. An app that isn't running on its new version afterwards
// is rolled back to the version it had. Nothing runs while a pool is
// unhealthy.
func (s *Scheduler) runUpdateRule(ctx context.Context, job Job, runID string) (string, string) {
	var rule UpdateRule
	_ = json.Unmarshal(job.Arguments, &rule)

	pool, status, err := s.unhealthyPool(ctx)
	switch {
	case ctx.Err() != nil:
		return stopped, ""
	case err != nil:
		return s.ruleSkipped(ctx, job, runID, "TrueNAS couldn't say how the pools are: "+truenas.Reason(err)+".")
	case pool != "":
		return s.ruleSkipped(ctx, job, runID, fmt.Sprintf("Pool %s is %s, and nothing is updated while a pool isn't healthy.", pool, strings.ToLower(status)))
	}

	s.refreshCatalog(ctx)
	apps, unchecked, err := s.updatable(ctx, rule)
	switch {
	case ctx.Err() != nil:
		return stopped, ""
	case err != nil:
		return s.ruleSkipped(ctx, job, runID, "TrueNAS couldn't list the apps: "+truenas.Reason(err)+".")
	case len(apps) == 0 && len(unchecked) == 0:
		// Nothing to say in the feed on a quiet run.
		return "done", "No apps had updates."
	}

	var results []appResult
	var left string
	for i, app := range apps {
		// Removed, or paused, since the run began: the rest wait. (Run
		// Now runs a paused rule, so only a pause since counts.)
		if current, err := s.store.Get(ctx, job.ID); errors.Is(err, ErrNotFound) || (err == nil && current.Paused && !job.Paused) {
			left = fmt.Sprintf("It was paused or removed while it ran, so %s %s left alone.",
				joinNames(titles(apps[i:])), map[bool]string{true: "was", false: "were"}[len(apps[i:]) == 1])
			break
		}
		result, finished := s.updateOne(ctx, app)
		if !finished {
			// What's done is known: say so. The run stays unfinished, to
			// show as interrupted when the helper starts again.
			if len(results) > 0 {
				title, summary, _ := ruleSummary(results)
				s.note(ctx, runID, events.AssistantRule, events.SeverityWarning, title,
					summary+" The helper stopped while updating "+app.title+".", job, "interrupted", nil)
			}
			return stopped, ""
		}
		results = append(results, result)
		if ctx.Err() != nil && i < len(apps)-1 {
			left = "The helper was stopping, so the rest were left for the next run."
			break
		}
	}
	for _, name := range unchecked {
		results = append(results, appResult{title: name, outcome: "couldn't be checked for an update", bad: true})
	}
	if len(results) == 0 {
		return s.ruleSkipped(ctx, job, runID, left)
	}
	title, summary, bad := ruleSummary(results)
	if left != "" {
		summary += " " + left
	}
	severity := events.SeverityInfo
	status = "done"
	if bad {
		severity, status = events.SeverityWarning, "failed"
	}
	s.note(ctx, runID, events.AssistantRule, severity, title, summary, job, status, nil)
	return status, title
}

// refreshCatalog has TrueNAS refresh its app catalog, which is how it
// learns of new versions. On its own it refreshes once a day, counted from
// when it started, so at 3 am it can be most of a day behind, and a run then
// finds nothing to update that Run Now later in the day would. If the
// refresh fails, the run goes by what TrueNAS already knows.
func (s *Scheduler) refreshCatalog(ctx context.Context) {
	if end := s.runJob(ctx, s.ruleTiming.catalogFor, "catalog.sync"); !end.ok() && !end.stopped {
		slog.Warn("schedule: couldn't refresh the app catalog; going by what TrueNAS knows", "why", end.reason())
	}
}

func titles(apps []ruleApp) []string {
	out := make([]string, len(apps))
	for i, a := range apps {
		out[i] = a.title
	}
	return out
}

func (s *Scheduler) ruleSkipped(ctx context.Context, job Job, runID, why string) (string, string) {
	s.note(ctx, runID, events.AssistantRule, events.SeverityWarning, "Didn't update apps: "+job.Title, why, job, "skipped", nil)
	return "skipped", why
}

// unhealthyPool names a pool that isn't healthy, if any.
func (s *Scheduler) unhealthyPool(ctx context.Context) (name, status string, err error) {
	raw, err := s.nas.Call(ctx, "pool.query")
	if err != nil {
		return "", "", err
	}
	var pools []struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Healthy *bool  `json:"healthy"`
	}
	if err := json.Unmarshal(raw, &pools); err != nil {
		return "", "", err
	}
	for _, p := range pools {
		if (p.Healthy != nil && !*p.Healthy) || !strings.EqualFold(p.Status, "ONLINE") {
			return p.Name, p.Status, nil
		}
	}
	return "", "", nil
}

// ruleApp is an app the rule can update.
type ruleApp struct {
	name, title, version string
}

// updatable lists the apps in scope with an update the rule can undo: a
// catalog app's new version, running now (TrueNAS updates only running
// apps). Custom apps, and apps whose only update is newer images, have no
// version to roll back to, so they're left alone. unchecked names apps
// TrueNAS couldn't say about.
func (s *Scheduler) updatable(ctx context.Context, rule UpdateRule) (apps []ruleApp, unchecked []string, err error) {
	raw, err := s.nas.Call(ctx, "app.query")
	if err != nil {
		return nil, nil, err
	}
	var rows []struct {
		Name             string `json:"name"`
		State            string `json:"state"`
		Version          string `json:"version"`
		UpgradeAvailable bool   `json:"upgrade_available"`
		CustomApp        bool   `json:"custom_app"`
		Metadata         struct {
			Title string `json:"title"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, nil, err
	}
	inScope := func(name string) bool {
		if len(rule.Apps) == 0 {
			return true
		}
		for _, a := range rule.Apps {
			if a == name {
				return true
			}
		}
		return false
	}
	for _, a := range rows {
		if !a.UpgradeAvailable || a.CustomApp || !inScope(a.Name) || !strings.EqualFold(a.State, "RUNNING") {
			continue
		}
		title := a.Metadata.Title
		if title == "" {
			title = a.Name
		}
		raw, err := s.nas.Call(ctx, "app.upgrade_summary", a.Name, map[string]any{"app_version": "latest"})
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			unchecked = append(unchecked, title)
			continue
		}
		var summary struct {
			UpgradeVersion string `json:"upgrade_version"`
		}
		if json.Unmarshal(raw, &summary) != nil || summary.UpgradeVersion == "" || summary.UpgradeVersion == a.Version {
			continue
		}
		apps = append(apps, ruleApp{name: a.Name, title: title, version: a.Version})
	}
	return apps, unchecked, nil
}

// updateOne updates an app, and rolls it back if it isn't running on its
// new version. finished is false when the helper stopped partway, and how
// it went isn't known.
func (s *Scheduler) updateOne(ctx context.Context, app ruleApp) (result appResult, finished bool) {
	t := s.ruleTiming
	result.title = app.title
	upgrade := s.runJob(ctx, t.upgradeFor, "app.upgrade", app.name, map[string]any{"app_version": "latest"})
	switch {
	case upgrade.stopped:
		return result, false
	case upgrade.unsent != nil:
		// Whether it began isn't known, so nothing is undone.
		result.outcome, result.bad = "couldn't be updated: "+upgrade.unsent.Error(), true
		return result, true
	case upgrade.stillWorking:
		// Rolling back under an update still going would make things worse.
		result.outcome = fmt.Sprintf("was still updating after %d minutes, so it was left to finish", int(t.upgradeFor.Minutes()))
		result.bad = true
		return result, true
	}

	state, version, done := s.settle(ctx, app.name, t.runningFor)
	if !done {
		return result, false
	}
	running := strings.EqualFold(state, "RUNNING")
	result.bad = true
	switch {
	case state == "" && version == "":
		result.outcome = "couldn't be checked after its update"
		return result, true
	case version == "" || version == app.version:
		// Still on the version it had, as far as can be told: nothing to
		// roll back.
		reason := "TrueNAS finished, but it's still on " + app.version
		if upgrade.failed != nil || upgrade.gone {
			reason = upgrade.reason()
		}
		if running {
			result.outcome = "didn't update: " + reason
		} else {
			result.outcome = "didn't update, and isn't running: " + reason
		}
		return result, true
	}
	result.bad = false
	if running {
		result.outcome = "updated"
		return result, true
	}

	// On its new version, but not running: back to the version it had.
	rollback := s.runJob(ctx, t.upgradeFor, "app.rollback", app.name,
		map[string]any{"app_version": app.version, "rollback_snapshot": true})
	result.bad = true
	switch {
	case rollback.stopped:
		return result, false
	case rollback.ok():
		result.outcome = "wasn't running after its update, so it was rolled back to " + app.version
	default:
		result.outcome = "isn't running after its update, and couldn't be rolled back: " + rollback.reason()
	}
	return result, true
}

// jobEnd is how a TrueNAS job the rule started went.
type jobEnd struct {
	// unsent: the call failed, so whether the job began isn't known.
	unsent error
	// failed: TrueNAS says it failed.
	failed error
	// stillWorking: it outlasted the wait.
	stillWorking bool
	// stopped: the helper is stopping.
	stopped bool
	// gone: TrueNAS no longer lists it, so how it ended isn't known.
	gone bool
}

func (e jobEnd) ok() bool {
	return e.unsent == nil && e.failed == nil && !e.stillWorking && !e.stopped && !e.gone
}

func (e jobEnd) reason() string {
	switch {
	case e.unsent != nil:
		return e.unsent.Error()
	case e.failed != nil:
		return e.failed.Error()
	case e.stillWorking:
		return "TrueNAS was still working on it"
	case e.gone:
		return "TrueNAS no longer lists the job, so how it ended isn't known"
	}
	return ""
}

// runJob starts a job and waits for it to end.
func (s *Scheduler) runJob(ctx context.Context, within time.Duration, method string, params ...any) jobEnd {
	raw, err := s.nas.Call(ctx, method, params...)
	if err != nil {
		if ctx.Err() != nil {
			return jobEnd{stopped: true}
		}
		return jobEnd{unsent: errors.New(truenas.Reason(err))}
	}
	id, err := truenas.JobID(raw)
	if err != nil {
		return jobEnd{unsent: err}
	}
	waitCtx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	job, err := s.nas.WaitJob(waitCtx, id, s.ruleTiming.poll)
	switch {
	case ctx.Err() != nil:
		return jobEnd{stopped: true}
	case errors.Is(err, truenas.ErrJobGone):
		return jobEnd{gone: true}
	case err != nil || !job.Finished():
		return jobEnd{stillWorking: true}
	case !job.Succeeded():
		return jobEnd{failed: errors.New(job.Failure())}
	}
	return jobEnd{}
}

// settle waits for an app to be running, and still running a little later
// (one that crashes as it starts shows as running at first), and returns
// its state and version then. done is false when the helper is stopping.
func (s *Scheduler) settle(ctx context.Context, name string, within time.Duration) (state, version string, done bool) {
	deadline := time.Now().Add(within)
	var runningSince time.Time
	for {
		raw, err := s.nas.Call(ctx, "app.query", []any{[]any{"name", "=", name}})
		if ctx.Err() != nil {
			return "", "", false
		}
		if err == nil {
			var apps []struct {
				State   string `json:"state"`
				Version string `json:"version"`
			}
			if json.Unmarshal(raw, &apps) == nil && len(apps) > 0 {
				state, version = apps[0].State, apps[0].Version
				switch {
				case !strings.EqualFold(state, "RUNNING"):
					runningSince = time.Time{}
				case runningSince.IsZero():
					runningSince = time.Now()
				}
				if !runningSince.IsZero() && time.Since(runningSince) >= s.ruleTiming.settle {
					return state, version, true
				}
			}
		}
		if time.Now().After(deadline) {
			return state, version, true
		}
		select {
		case <-ctx.Done():
			return "", "", false
		case <-time.After(s.ruleTiming.poll):
		}
	}
}

// ruleSummary sums up a run: "Updated Plex and Sonarr", or what went
// wrong first.
func ruleSummary(results []appResult) (title, summary string, bad bool) {
	var updated, lines []string
	for _, r := range results {
		lines = append(lines, r.title+" "+r.outcome+".")
		if r.bad {
			bad = true
		} else {
			updated = append(updated, r.title)
		}
	}
	switch {
	case !bad:
		title = "Updated " + joinNames(updated)
	case len(updated) > 0:
		title = fmt.Sprintf("Updated %s; %d didn't go to plan", joinNames(updated), len(results)-len(updated))
	default:
		title = "Automatic app updates didn't go to plan"
	}
	return title, strings.Join(lines, " "), bad
}

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return "no apps"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
