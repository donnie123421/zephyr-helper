package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	poll       time.Duration
}

var defaultRuleTiming = ruleTiming{upgradeFor: 15 * time.Minute, runningFor: 5 * time.Minute, poll: 5 * time.Second}

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
// one at a time. An app that isn't running again afterwards is rolled back
// to the version it had. Nothing runs while a pool is unhealthy.
func (s *Scheduler) runUpdateRule(ctx context.Context, job Job, runID string) (string, string) {
	var rule UpdateRule
	_ = json.Unmarshal(job.Arguments, &rule)

	if pool, status, err := s.unhealthyPool(ctx); err != nil {
		return s.ruleSkipped(ctx, job, runID, "TrueNAS couldn't say how the pools are: "+truenas.Reason(err)+".")
	} else if pool != "" {
		return s.ruleSkipped(ctx, job, runID, fmt.Sprintf("Pool %s is %s, and nothing is updated while a pool isn't healthy.", pool, strings.ToLower(status)))
	}

	apps, err := s.updatable(ctx, rule)
	if err != nil {
		return s.ruleSkipped(ctx, job, runID, "TrueNAS couldn't list the apps: "+truenas.Reason(err)+".")
	}
	if len(apps) == 0 {
		// Nothing to say in the feed on a quiet run.
		return "done", "No apps had updates."
	}

	var results []appResult
	for _, app := range apps {
		results = append(results, s.updateOne(ctx, app))
	}
	title, summary, bad := ruleSummary(results)
	severity := events.SeverityInfo
	status := "done"
	if bad {
		severity, status = events.SeverityWarning, "failed"
	}
	s.note(ctx, runID, events.AssistantRule, severity, title, summary, job, status, nil)
	return status, title
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
// version to roll back to, so they're left alone.
func (s *Scheduler) updatable(ctx context.Context, rule UpdateRule) ([]ruleApp, error) {
	raw, err := s.nas.Call(ctx, "app.query")
	if err != nil {
		return nil, err
	}
	var apps []struct {
		Name             string `json:"name"`
		State            string `json:"state"`
		Version          string `json:"version"`
		UpgradeAvailable bool   `json:"upgrade_available"`
		CustomApp        bool   `json:"custom_app"`
		Metadata         struct {
			Title string `json:"title"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &apps); err != nil {
		return nil, err
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
	var out []ruleApp
	for _, a := range apps {
		if !a.UpgradeAvailable || a.CustomApp || !inScope(a.Name) || !strings.EqualFold(a.State, "RUNNING") {
			continue
		}
		raw, err := s.nas.Call(ctx, "app.upgrade_summary", a.Name, map[string]any{"app_version": "latest"})
		if err != nil {
			continue
		}
		var summary struct {
			UpgradeVersion string `json:"upgrade_version"`
		}
		if json.Unmarshal(raw, &summary) != nil || summary.UpgradeVersion == "" || summary.UpgradeVersion == a.Version {
			continue
		}
		title := a.Metadata.Title
		if title == "" {
			title = a.Name
		}
		out = append(out, ruleApp{name: a.Name, title: title, version: a.Version})
	}
	return out, nil
}

// errStillWorking: a job outlasted the wait.
var errStillWorking = errors.New("TrueNAS was still working on it")

// updateOne updates an app, and rolls it back if it isn't running again.
func (s *Scheduler) updateOne(ctx context.Context, app ruleApp) appResult {
	t := s.ruleTiming
	upgradeErr := s.waitForJob(ctx, t.upgradeFor, "app.upgrade", app.name, map[string]any{"app_version": "latest"})
	if errors.Is(upgradeErr, errStillWorking) {
		// Rolling back under an update still going would make things worse.
		return appResult{title: app.title, outcome: fmt.Sprintf("was still updating after %d minutes, so it was left to finish", int(t.upgradeFor.Minutes())), bad: true}
	}
	state := s.waitForRunning(ctx, app.name, t.runningFor)
	if strings.EqualFold(state, "RUNNING") {
		if upgradeErr != nil {
			// Still on its old version, and still working.
			return appResult{title: app.title, outcome: "didn't update: " + upgradeErr.Error(), bad: true}
		}
		return appResult{title: app.title, outcome: "updated"}
	}
	// Not running: roll back to the version it had.
	if err := s.waitForJob(ctx, t.upgradeFor, "app.rollback", app.name,
		map[string]any{"app_version": app.version, "rollback_snapshot": true}); err != nil {
		return appResult{title: app.title, outcome: "isn't running after its update, and couldn't be rolled back: " + err.Error(), bad: true}
	}
	return appResult{title: app.title, outcome: "wasn't running after its update, so it was rolled back to " + app.version, bad: true}
}

// waitForJob starts a job and waits for it to end.
func (s *Scheduler) waitForJob(ctx context.Context, within time.Duration, method string, params ...any) error {
	raw, err := s.nas.Call(ctx, method, params...)
	if err != nil {
		return errors.New(truenas.Reason(err))
	}
	id, err := truenas.JobID(raw)
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	job, err := s.nas.WaitJob(waitCtx, id, s.ruleTiming.poll)
	if err != nil || !job.Finished() {
		return errStillWorking
	}
	if !job.Succeeded() {
		return errors.New(job.Failure())
	}
	return nil
}

// waitForRunning waits for an app to be running, and returns its last
// state.
func (s *Scheduler) waitForRunning(ctx context.Context, name string, within time.Duration) string {
	deadline := time.Now().Add(within)
	var state string
	for {
		raw, err := s.nas.Call(ctx, "app.query", []any{[]any{"name", "=", name}})
		if err == nil {
			var apps []struct {
				State string `json:"state"`
			}
			if json.Unmarshal(raw, &apps) == nil && len(apps) > 0 {
				state = apps[0].State
				if strings.EqualFold(state, "RUNNING") {
					return state
				}
			}
		}
		if time.Now().After(deadline) {
			return state
		}
		select {
		case <-ctx.Done():
			return state
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
