package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// The tools here change the NAS. Each runs only after the user approves the
// card its Describe writes. Cards name things as TrueNAS records them, and
// say what the change does to the people using the NAS.

// actionTools returns every action.
func actionTools(tn *truenas.Client) []Tool {
	return []Tool{
		acknowledgeAlerts(tn),
		createSnapshot(tn),
		runBackupTask(tn),
		scrubPool(tn),
		appAction(tn),
		updateApp(tn),
		rollbackApp(tn),
		vmAction(tn),
		containerAction(tn),
		serviceAction(tn),
	}
}

// maxAlertsPerCard bounds one acknowledge_alerts card.
const maxAlertsPerCard = 20

// ----- acknowledge_alerts -----------------------------------------------------

func acknowledgeAlerts(tn *truenas.Client) Tool {
	type args struct {
		UUIDs []string `json:"uuids"`
	}
	// chosen reads the arguments and finds each alert among the active ones.
	chosen := func(ctx context.Context, raw json.RawMessage) ([]map[string]any, error) {
		var a args
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		var uuids []string
		for _, id := range a.UUIDs {
			if id = strings.TrimSpace(id); id != "" && !seen[id] {
				seen[id] = true
				uuids = append(uuids, id)
			}
		}
		switch {
		case len(uuids) == 0:
			return nil, errors.New("name each alert by its uuid from list_alerts")
		case len(uuids) > maxAlertsPerCard:
			return nil, fmt.Errorf("at most %d alerts at a time", maxAlertsPerCard)
		}
		raw, err := call(ctx, tn, "alert.list")
		if err != nil {
			return nil, err
		}
		var all []map[string]any
		if err := json.Unmarshal(raw, &all); err != nil {
			return nil, fmt.Errorf("decode alerts: %w", err)
		}
		byID := map[string]map[string]any{}
		for _, alert := range all {
			if dismissed, _ := alert["dismissed"].(bool); !dismissed {
				byID[str(alert, "uuid")] = alert
			}
		}
		out := make([]map[string]any, 0, len(uuids))
		for _, id := range uuids {
			alert, ok := byID[id]
			if !ok {
				return nil, fmt.Errorf("no active alert has the uuid %q; call list_alerts for the current ones", id)
			}
			out = append(out, alert)
		}
		return out, nil
	}

	return Tool{
		Def: Definition{
			Name: "acknowledge_alerts",
			Description: "Acknowledge (dismiss) active TrueNAS alerts the user has dealt with, so they " +
				"stop showing as active. Pass each alert's uuid from list_alerts; one card covers them " +
				"all, up to 20.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"uuids": {"type": "array", "items": {"type": "string"}, "description": "The uuid of each alert, from list_alerts."}
				},
				"required": ["uuids"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Acknowledging alerts…",
		Action: &Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (Card, error) {
				alerts, err := chosen(ctx, raw)
				if err != nil {
					return Card{}, err
				}
				title := "Acknowledge an alert"
				if len(alerts) > 1 {
					title = fmt.Sprintf("Acknowledge %d alerts", len(alerts))
				}
				var lines []string
				for _, alert := range alerts {
					text := str(alert, "formatted")
					if text == "" {
						text = str(alert, "text")
					}
					lines = append(lines, "• "+firstLine(text, 140))
				}
				return Card{Title: title, Detail: strings.Join(lines, "\n"), Risk: RiskRoutine}, nil
			},
			Run: func(ctx context.Context, raw json.RawMessage) (Outcome, error) {
				alerts, err := chosen(ctx, raw)
				if err != nil {
					return Outcome{}, err
				}
				var refused []string
				for _, alert := range alerts {
					if _, err := call(ctx, tn, "alert.dismiss", str(alert, "uuid")); err != nil {
						refused = append(refused, err.Error())
					}
				}
				done := len(alerts) - len(refused)
				switch {
				case done == 0:
					return Outcome{}, errors.New(refused[0])
				case len(refused) > 0:
					return Outcome{Message: fmt.Sprintf("Acknowledged %d of %d alerts. TrueNAS refused the rest: %s", done, len(alerts), refused[0])}, nil
				case done == 1:
					return Outcome{Message: "Acknowledged the alert."}, nil
				default:
					return Outcome{Message: fmt.Sprintf("Acknowledged %d alerts.", done)}, nil
				}
			},
		},
	}
}

// ----- create_snapshot ---------------------------------------------------------

// snapshotName is what ZFS allows in a snapshot's name.
var snapshotName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,199}$`)

func createSnapshot(tn *truenas.Client) Tool {
	type args struct {
		Dataset   string `json:"dataset"`
		Name      string `json:"name"`
		Recursive bool   `json:"recursive"`
	}
	type plan struct {
		dataset   string
		name      string
		recursive bool
		children  int
	}
	// planned checks the dataset and names the snapshot.
	planned := func(ctx context.Context, raw json.RawMessage, now time.Time) (plan, error) {
		var a args
		if err := decodeArgs(raw, &a); err != nil {
			return plan{}, err
		}
		dataset := strings.TrimSpace(a.Dataset)
		if dataset == "" {
			return plan{}, errors.New("say which dataset, by its full name from list_datasets, such as tank/media")
		}
		rows, err := query(ctx, tn, "pool.dataset.query", []any{[]any{"id", "=", dataset}})
		if err != nil {
			return plan{}, errors.New(truenas.Reason(err))
		}
		if len(rows) == 0 {
			return plan{}, fmt.Errorf("there's no dataset named %q; call list_datasets for their names", dataset)
		}
		p := plan{dataset: str(rows[0], "id"), recursive: a.Recursive}
		if children, ok := rows[0]["children"].([]any); ok {
			p.children = len(children)
		}
		p.name = strings.TrimSpace(a.Name)
		if p.name == "" {
			p.name = "zephyr-" + nasTime(ctx, tn, now).Format("2006-01-02-1504")
		}
		if !snapshotName.MatchString(p.name) {
			return plan{}, fmt.Errorf("%q can't name a snapshot: use letters, numbers, - _ . and :", p.name)
		}
		id := []any{[]any{"id", "=", p.dataset + "@" + p.name}}
		existing, err := query(ctx, tn, "pool.snapshot.query", id)
		if truenas.IsMethodNotFound(err) {
			// 25.04 keeps snapshots under zfs.snapshot.
			existing, err = query(ctx, tn, "zfs.snapshot.query", id)
		}
		if err == nil && len(existing) > 0 {
			return plan{}, fmt.Errorf("%s already has a snapshot named %s", p.dataset, p.name)
		}
		return p, nil
	}

	return Tool{
		Def: Definition{
			Name: "create_snapshot",
			Description: "Take a ZFS snapshot of a dataset now. It's named zephyr-<date>-<time> unless " +
				"you pass a name. Set recursive to include the datasets inside it too.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"dataset": {"type": "string", "description": "The dataset's full name from list_datasets, such as tank/media."},
					"name": {"type": "string", "description": "The snapshot's name, without the dataset and @. Omit to use zephyr-<date>-<time>."},
					"recursive": {"type": "boolean", "description": "Also snapshot every dataset inside this one. Default false."}
				},
				"required": ["dataset"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Taking a snapshot…",
		Action: &Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (Card, error) {
				p, err := planned(ctx, raw, time.Now())
				if err != nil {
					return Card{}, err
				}
				scope := "Of " + plain(p.dataset) + " alone, not the datasets inside it."
				switch {
				case p.recursive && p.children > 0:
					scope = fmt.Sprintf("Of %s and everything inside it.", plain(p.dataset))
				case p.children == 0:
					scope = "Of " + plain(p.dataset) + "."
				}
				detail := fmt.Sprintf("Named %s. %s It takes no space at first, and grows as files change.", plain(p.name), scope)
				settled, _ := json.Marshal(args{Dataset: p.dataset, Name: p.name, Recursive: p.recursive})
				return Card{Title: "Take a snapshot of " + plain(p.dataset), Detail: detail, Risk: RiskRoutine, Args: settled}, nil
			},
			Run: func(ctx context.Context, raw json.RawMessage) (Outcome, error) {
				p, err := planned(ctx, raw, time.Now())
				if err != nil {
					return Outcome{}, err
				}
				body := map[string]any{"dataset": p.dataset, "name": p.name, "recursive": p.recursive}
				_, err = call(ctx, tn, "pool.snapshot.create", body)
				if truenas.IsMethodNotFound(err) {
					// 25.04 has it as zfs.snapshot.create.
					_, err = call(ctx, tn, "zfs.snapshot.create", body)
				}
				if err != nil {
					return Outcome{}, err
				}
				return Outcome{Message: fmt.Sprintf("Took snapshot %s@%s.", plain(p.dataset), plain(p.name))}, nil
			},
		},
	}
}

// nasTime is now in the NAS's time zone, so a snapshot's name matches its
// clock. UTC when the zone can't be read.
func nasTime(ctx context.Context, tn *truenas.Client, now time.Time) time.Time {
	raw, err := tn.Call(ctx, "system.general.config")
	if err != nil {
		return now.UTC()
	}
	var general struct {
		Timezone string `json:"timezone"`
	}
	if json.Unmarshal(raw, &general) != nil || general.Timezone == "" {
		return now.UTC()
	}
	loc, err := time.LoadLocation(general.Timezone)
	if err != nil {
		return now.UTC()
	}
	return now.In(loc)
}

// ----- scrub_pool ----------------------------------------------------------------

func scrubPool(tn *truenas.Client) Tool {
	type args struct {
		Pool   string `json:"pool"`
		Action string `json:"action"`
	}
	type plan struct {
		id     int64
		name   string
		cancel bool
		pool   map[string]any
		scan   map[string]any
	}
	planned := func(ctx context.Context, raw json.RawMessage) (plan, error) {
		var a args
		if err := decodeArgs(raw, &a); err != nil {
			return plan{}, err
		}
		action := strings.ToLower(strings.TrimSpace(a.Action))
		if action != "" && action != "start" && action != "cancel" {
			return plan{}, fmt.Errorf("action is start or cancel, not %q", a.Action)
		}
		name := strings.TrimSpace(a.Pool)
		if name == "" {
			return plan{}, errors.New("say which pool, by its name from list_pools")
		}
		rows, err := query(ctx, tn, "pool.query", []any{[]any{"name", "=", name}})
		if err != nil {
			return plan{}, errors.New(truenas.Reason(err))
		}
		if len(rows) == 0 {
			return plan{}, fmt.Errorf("there's no pool named %q; call list_pools for their names", name)
		}
		p := plan{name: str(rows[0], "name"), cancel: action == "cancel", pool: rows[0]}
		p.id, _ = num(rows[0], "id")
		p.scan, _ = rows[0]["scan"].(map[string]any)
		scanning := strings.EqualFold(str(p.scan, "state"), "SCANNING")
		scrubbing := scanning && strings.EqualFold(str(p.scan, "function"), "SCRUB")
		switch {
		case p.cancel && !scrubbing:
			return plan{}, fmt.Errorf("%s isn't running a scrub", p.name)
		case !p.cancel && scanning:
			what := strings.ToLower(str(p.scan, "function"))
			if what == "" {
				what = "scan"
			}
			if p.scan["pause"] != nil {
				return plan{}, fmt.Errorf("%s has a paused %s%s; resume it in TrueNAS", p.name, what, progress(p.scan))
			}
			return plan{}, fmt.Errorf("%s is already running a %s%s", p.name, what, progress(p.scan))
		}
		return p, nil
	}

	return Tool{
		Def: Definition{
			Name: "scrub_pool",
			Description: "Start a scrub on a pool, which reads every block and checks it against its " +
				"checksum, or cancel the scrub running on it.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pool": {"type": "string", "description": "The pool's name from list_pools."},
					"action": {"type": "string", "enum": ["start", "cancel"], "description": "Default start."}
				},
				"required": ["pool"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Starting the scrub…",
		Action: &Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (Card, error) {
				p, err := planned(ctx, raw)
				if err != nil {
					return Card{}, err
				}
				name := plain(p.name)
				if p.cancel {
					detail := "The next scrub starts again from the beginning."
					if done := progress(p.scan); done != "" {
						detail = "It's" + strings.TrimPrefix(done, ",") + ". " + detail
					}
					return Card{Title: "Cancel the scrub on " + name, Detail: detail, Risk: RiskRoutine}, nil
				}
				detail := name + " will be slower for a few hours while ZFS reads and checks every block"
				if used, ok := num(p.pool, "allocated"); ok && used > 0 {
					detail += " (" + bytesText(used) + ")"
				}
				detail += "."
				if last := lastScrub(p.scan, time.Now()); last != "" {
					detail += " " + last
				}
				if healthy, ok := p.pool["healthy"].(bool); ok && !healthy {
					detail += fmt.Sprintf(" %s isn't healthy (%s), and a scrub adds to its load.", name, plain(str(p.pool, "status")))
				}
				return Card{Title: "Start a scrub on " + name, Detail: detail, Risk: RiskRoutine}, nil
			},
			Run: func(ctx context.Context, raw json.RawMessage) (Outcome, error) {
				p, err := planned(ctx, raw)
				if err != nil {
					return Outcome{}, err
				}
				if p.cancel {
					run, err := cancelScrub(ctx, tn, p.name)
					if err != nil {
						return Outcome{}, err
					}
					return run.outcome("Cancelled the scrub on "+plain(p.name)+".", "Cancelling the scrub on "+plain(p.name)+".",
						"Cancelled the scrub on "+plain(p.name), "Couldn't cancel the scrub on "+plain(p.name)), nil
				}
				jobID, err := startScrub(ctx, tn, p.name)
				if err != nil {
					return Outcome{}, err
				}
				// The job lasts as long as the scrub, and ends the same way
				// whatever it found, so the pool says how it went.
				return Outcome{
					Message: "Started a scrub on " + plain(p.name) + ".",
					Job:     &Job{ID: jobID, Report: scrubReport(tn, p.name)},
				}, nil
			},
		},
	}
}

// startScrub starts a scrub and returns its job, which lasts as long as
// the scrub: zpool.scrub.run on 26, pool.scrub.scrub before. (The older
// pool.scrub, keyed on the pool's id, is a transient job: TrueNAS drops it
// the moment it ends, taking any refusal with it.)
func startScrub(ctx context.Context, tn *truenas.Client, name string) (int64, error) {
	// threshold is how many days since the last scrub make one due, and a
	// scrub that isn't due is skipped without a word. 1 makes it due now.
	raw, err := tn.Call(ctx, "zpool.scrub.run", map[string]any{
		"pool_name": name, "scan_type": "SCRUB", "action": "START", "threshold": 1,
	})
	if truenas.IsMethodNotFound(err) {
		// Before 26 a refusal (already scrubbing, too many scrubs) fails
		// the job straight away.
		run, err := runJob(ctx, tn, quickCheck, "pool.scrub.scrub", name, "START")
		if err != nil {
			return 0, err
		}
		return run.id, nil
	}
	if err != nil {
		return 0, errors.New(truenas.Reason(err))
	}
	jobID, err := truenas.JobID(raw)
	if err != nil {
		return 0, fmt.Errorf("zpool.scrub.run: %w", err)
	}
	// zpool.scrub.run succeeds even when it declines (the pool unhealthy,
	// a resilver running, too many scrubs): it returns at once, while a
	// scrub that starts keeps its job running until it's done. So the
	// pool scanning, or the job still running, means it started; a job
	// that ended says so only if the pool's last scrub ended after the call
	// (a small pool scrubs in seconds).
	called := time.Now()
	for i := 0; i < 5; i++ {
		rows, err := query(ctx, tn, "pool.query", []any{[]any{"name", "=", name}})
		var scan map[string]any
		if err == nil && len(rows) > 0 {
			scan, _ = rows[0]["scan"].(map[string]any)
			if strings.EqualFold(str(scan, "state"), "SCANNING") {
				return jobID, nil
			}
		}
		if job, err := tn.GetJob(ctx, jobID); err == nil && job.Finished() {
			if !job.Succeeded() {
				return 0, jobError{id: jobID, reason: job.Failure()}
			}
			if ended, ok := trueNASTime(scan["end_time"]); ok && strings.EqualFold(str(scan, "function"), "SCRUB") &&
				ended.After(called.Add(-5*time.Second)) {
				return jobID, nil
			}
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(scanCheck):
		}
	}
	if job, err := tn.GetJob(ctx, jobID); err == nil && !job.Finished() {
		return jobID, nil
	}
	return 0, errors.New("TrueNAS didn't start it. The pool may be unhealthy, or already running a scrub or resilver")
}

// cancelScrub stops a scrub: CANCEL to 26's zpool.scrub.run, STOP to
// pool.scrub.scrub before.
func cancelScrub(ctx context.Context, tn *truenas.Client, name string) (jobRun, error) {
	run, err := runJob(ctx, tn, shortJobWait, "zpool.scrub.run", map[string]any{"pool_name": name, "scan_type": "SCRUB", "action": "CANCEL"})
	if !truenas.IsMethodNotFound(err) {
		return run, err
	}
	return runJob(ctx, tn, shortJobWait, "pool.scrub.scrub", name, "STOP")
}

// scrubReport says how a scrub the assistant started went, from the pool's
// last scan once the scrub's job has ended.
func scrubReport(tn *truenas.Client, name string) func(context.Context, truenas.Job) Ending {
	return func(ctx context.Context, ended truenas.Job) Ending {
		label := plain(name)
		if !ended.Succeeded() {
			return Ending{Title: "The scrub on " + label + " stopped", Summary: ended.Failure(), Bad: true}
		}
		rows, err := query(ctx, tn, "pool.query", []any{[]any{"name", "=", name}})
		if err != nil || len(rows) == 0 {
			return Ending{Title: "The scrub on " + label + " ended"}
		}
		scan, _ := rows[0]["scan"].(map[string]any)
		switch strings.ToUpper(str(scan, "state")) {
		case "CANCELED", "CANCELLED":
			return Ending{Title: "The scrub on " + label + " was cancelled"}
		case "SCANNING":
			return Ending{Title: "The scrub on " + label + " was paused"}
		}
		if errs, ok := num(scan, "errors"); ok && errs > 0 {
			return Ending{
				Title:   fmt.Sprintf("The scrub on %s found %d errors", label, errs),
				Summary: "Check the pool in the Storage tab: ZFS repairs what it can from the pool's redundancy.",
				Bad:     true,
			}
		}
		return Ending{Title: "The scrub on " + label + " finished and found no errors"}
	}
}

// progress says how far a scan has got: ", 43% done".
func progress(scan map[string]any) string {
	if pct, ok := scan["percentage"].(float64); ok && pct > 0 {
		return fmt.Sprintf(", %.0f%% done", pct)
	}
	return ""
}

// lastScrub says when the pool's last scrub finished and what it found.
func lastScrub(scan map[string]any, now time.Time) string {
	if !strings.EqualFold(str(scan, "function"), "SCRUB") || !strings.EqualFold(str(scan, "state"), "FINISHED") {
		return ""
	}
	ended, ok := trueNASTime(scan["end_time"])
	if !ok {
		return ""
	}
	found := "found no errors"
	if errs, ok := num(scan, "errors"); ok && errs > 0 {
		found = fmt.Sprintf("found %d errors", errs)
	}
	return fmt.Sprintf("Its last scrub finished %s and %s.", ago(ended, now), found)
}

// ----- run_backup_task -----------------------------------------------------------

// backupKinds are the backup tasks the assistant can run, by the kind the
// model names.
var backupKinds = map[string]struct {
	query, run, noun string
}{
	"replication": {"replication.query", "replication.run", "replication"},
	"cloud_sync":  {"cloudsync.query", "cloudsync.sync", "cloud sync"},
	"rsync":       {"rsynctask.query", "rsynctask.run", "rsync task"},
}

func runBackupTask(tn *truenas.Client) Tool {
	type args struct {
		Kind string `json:"kind"`
		ID   int64  `json:"id"`
	}
	type plan struct {
		kind string
		id   int64
		task map[string]any
	}
	planned := func(ctx context.Context, raw json.RawMessage) (plan, error) {
		var a args
		if err := decodeArgs(raw, &a); err != nil {
			return plan{}, err
		}
		kind, ok := backupKinds[a.Kind]
		if !ok {
			return plan{}, fmt.Errorf("kind is replication, cloud_sync or rsync, not %q", a.Kind)
		}
		rows, err := query(ctx, tn, kind.query, []any{[]any{"id", "=", a.ID}})
		if err != nil {
			return plan{}, errors.New(truenas.Reason(err))
		}
		if len(rows) == 0 {
			return plan{}, fmt.Errorf("there's no %s with id %d; call list_backup_tasks for their ids", kind.noun, a.ID)
		}
		task := rows[0]
		name := quoted(taskName(a.Kind, task))
		if backupRunning(task) {
			return plan{}, fmt.Errorf("the %s %s is already running", kind.noun, name)
		}
		if locked, _ := task["locked"].(bool); locked {
			// TrueNAS would report success and copy nothing.
			return plan{}, fmt.Errorf("the %s %s can't run while its dataset is locked; unlock it first", kind.noun, name)
		}
		if enabled, ok := task["enabled"].(bool); ok && !enabled && a.Kind == "replication" {
			// Before 26, TrueNAS refuses to run a replication that's off.
			return plan{}, fmt.Errorf("the replication %s is turned off; turn it on in Backup & Replication first", name)
		}
		return plan{kind: a.Kind, id: a.ID, task: task}, nil
	}

	return Tool{
		Def: Definition{
			Name: "run_backup_task",
			Description: "Run a replication, cloud sync or rsync task now, outside its schedule. Take the " +
				"kind and id from list_backup_tasks.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"kind": {"type": "string", "enum": ["replication", "cloud_sync", "rsync"]},
					"id": {"type": "integer", "description": "The task's id from list_backup_tasks."}
				},
				"required": ["kind", "id"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Starting the backup…",
		Action: &Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (Card, error) {
				p, err := planned(ctx, raw)
				if err != nil {
					return Card{}, err
				}
				noun := backupKinds[p.kind].noun
				name := quoted(taskName(p.kind, p.task))
				detail := taskRoute(p.kind, p.task)
				if last := lastRun(p.task, time.Now()); last != "" {
					detail += " " + last
				}
				if enabled, ok := p.task["enabled"].(bool); ok && !enabled {
					detail += " The task is turned off, so it doesn't run on its own schedule."
				}
				return Card{Title: fmt.Sprintf("Run the %s %s now", noun, name), Detail: strings.TrimSpace(detail), Risk: RiskRoutine}, nil
			},
			Run: func(ctx context.Context, raw json.RawMessage) (Outcome, error) {
				p, err := planned(ctx, raw)
				if err != nil {
					return Outcome{}, err
				}
				kind := backupKinds[p.kind]
				name := quoted(taskName(p.kind, p.task))
				run, err := runJob(ctx, tn, quickCheck, kind.run, p.id)
				if err != nil {
					return Outcome{}, err
				}
				return run.outcome(
					fmt.Sprintf("The %s %s ran.", kind.noun, name),
					fmt.Sprintf("Started the %s %s.", kind.noun, name),
					fmt.Sprintf("The %s %s finished", kind.noun, name),
					fmt.Sprintf("The %s %s failed", kind.noun, name),
				), nil
			},
		},
	}
}

// backupRunning reports whether a backup task is running or waiting to:
// running it again would queue another run behind it.
func backupRunning(task map[string]any) bool {
	if job, ok := task["job"].(map[string]any); ok {
		switch strings.ToUpper(str(job, "state")) {
		case "RUNNING", "WAITING":
			return true
		}
	}
	// A replication's job is forgotten when the middleware restarts; its
	// state isn't.
	if st, ok := task["state"].(map[string]any); ok {
		switch strings.ToUpper(str(st, "state")) {
		case "RUNNING", "WAITING":
			return true
		}
	}
	return false
}

// taskName is what TrueNAS calls a backup task.
func taskName(kind string, task map[string]any) string {
	var name string
	switch kind {
	case "replication":
		name = str(task, "name")
	case "cloud_sync":
		name = str(task, "description")
	case "rsync":
		name = str(task, "desc")
		if name == "" {
			name = str(task, "path")
		}
	}
	if strings.TrimSpace(name) == "" {
		id, _ := num(task, "id")
		name = fmt.Sprintf("#%d", id)
	}
	return name
}

// taskRoute says what a backup task copies, and where.
func taskRoute(kind string, task map[string]any) string {
	pull := strings.EqualFold(str(task, "direction"), "PULL")
	switch kind {
	case "replication":
		var sources []string
		if list, ok := task["source_datasets"].([]any); ok {
			for _, s := range list {
				if name, ok := s.(string); ok {
					sources = append(sources, plain(name))
				}
			}
		}
		target := plain(str(task, "target_dataset"))
		where := "to " + target
		if !strings.EqualFold(str(task, "transport"), "LOCAL") {
			where += " on another system"
		}
		if pull {
			return fmt.Sprintf("Copies %s from another system into %s.", strings.Join(sources, ", "), target)
		}
		return fmt.Sprintf("Copies %s %s.", strings.Join(sources, ", "), where)
	case "cloud_sync":
		path := plain(str(task, "path"))
		provider := ""
		if creds, ok := task["credentials"].(map[string]any); ok {
			provider = plain(str(creds, "name"))
		}
		if provider == "" {
			provider = "the cloud"
		}
		if pull {
			return fmt.Sprintf("Copies from %s into %s.", provider, path)
		}
		return fmt.Sprintf("Copies %s to %s.", path, provider)
	case "rsync":
		path := plain(str(task, "path"))
		remote := plain(str(task, "remotehost"))
		if pull {
			return fmt.Sprintf("Copies from %s into %s.", remote, path)
		}
		return fmt.Sprintf("Copies %s to %s.", path, remote)
	}
	return ""
}

// lastRun says how a backup task's last run went, and when.
func lastRun(task map[string]any, now time.Time) string {
	job, _ := task["job"].(map[string]any)
	state := strings.ToUpper(str(job, "state"))
	when, ok := trueNASTime(job["time_finished"])
	if st, isMap := task["state"].(map[string]any); state == "" && isMap {
		// Replication keeps its last run in state.
		state = strings.ToUpper(str(st, "state"))
		when, ok = trueNASTime(st["datetime"])
	}
	var how string
	switch state {
	case "SUCCESS", "FINISHED":
		how = "succeeded"
	case "FAILED", "ERROR":
		how = "failed"
	case "ABORTED":
		how = "was cancelled"
	default:
		return ""
	}
	if ok {
		return fmt.Sprintf("Its last run, %s, %s.", ago(when, now), how)
	}
	return fmt.Sprintf("Its last run %s.", how)
}

// ----- apps ---------------------------------------------------------------------

// findApp looks an app up by name.
func findApp(ctx context.Context, tn *truenas.Client, name string) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("say which app, by its name from list_apps")
	}
	rows, err := query(ctx, tn, "app.query", []any{[]any{"name", "=", name}})
	if err != nil {
		return nil, errors.New(truenas.Reason(err))
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("there's no app named %q; call list_apps for their names", name)
	}
	return rows[0], nil
}

// appTitle is an app's name as its catalog entry has it ("Plex"), with
// the name it was installed under when that's different ("Plex (plex2)"),
// so two installs of one app can't be mistaken for each other.
func appTitle(app map[string]any) string {
	name := plain(str(app, "name"))
	if meta, ok := app["metadata"].(map[string]any); ok {
		if title := plain(str(meta, "title")); title != "" {
			if strings.EqualFold(title, name) {
				return title
			}
			return title + " (" + name + ")"
		}
	}
	return name
}

func appAction(tn *truenas.Client) Tool {
	type args struct {
		App    string `json:"app"`
		Action string `json:"action"`
	}
	planned := func(ctx context.Context, raw json.RawMessage) (string, map[string]any, error) {
		var a args
		if err := decodeArgs(raw, &a); err != nil {
			return "", nil, err
		}
		action := strings.ToLower(strings.TrimSpace(a.Action))
		switch action {
		case "start", "stop", "restart":
		default:
			return "", nil, fmt.Errorf("action is start, stop or restart, not %q", a.Action)
		}
		app, err := findApp(ctx, tn, a.App)
		if err != nil {
			return "", nil, err
		}
		state := strings.ToUpper(str(app, "state"))
		switch {
		case action == "start" && (state == "RUNNING" || state == "DEPLOYING"):
			return "", nil, fmt.Errorf("%s is already %s", appTitle(app), strings.ToLower(state))
		case action == "stop" && state == "STOPPED":
			return "", nil, fmt.Errorf("%s is already stopped", appTitle(app))
		case action == "restart" && state == "STOPPED":
			return "", nil, fmt.Errorf("%s is stopped; start it instead", appTitle(app))
		}
		return action, app, nil
	}

	return Tool{
		Def: Definition{
			Name:        "app_action",
			Description: "Start, stop or restart an installed app. Restart stops it and starts it again.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"app": {"type": "string", "description": "The app's name from list_apps."},
					"action": {"type": "string", "enum": ["start", "stop", "restart"]}
				},
				"required": ["app", "action"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Working on the app…",
		Action: &Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (Card, error) {
				action, app, err := planned(ctx, raw)
				if err != nil {
					return Card{}, err
				}
				title := appTitle(app)
				switch action {
				case "start":
					return Card{Title: "Start " + title, Detail: title + " starts up. It can take a minute to be ready.", Risk: RiskRoutine}, nil
				case "stop":
					return Card{Title: "Stop " + title, Detail: title + " stops until it's started again, so anyone using it is cut off.", Risk: RiskDisruptive}, nil
				default:
					return Card{Title: "Restart " + title, Detail: title + " stops and starts again, which takes a minute or so. Anyone using it is cut off meanwhile.", Risk: RiskDisruptive}, nil
				}
			},
			Run: func(ctx context.Context, raw json.RawMessage) (Outcome, error) {
				action, app, err := planned(ctx, raw)
				if err != nil {
					return Outcome{}, err
				}
				name, title := str(app, "name"), appTitle(app)
				switch action {
				case "start":
					run, err := runJob(ctx, tn, shortJobWait, "app.start", name)
					if err != nil {
						return Outcome{}, err
					}
					return run.outcome("Started "+title+".", "Starting "+title+".", "Started "+title, title+" didn't start"), nil
				case "stop":
					run, err := runJob(ctx, tn, shortJobWait, "app.stop", name)
					if err != nil {
						return Outcome{}, err
					}
					return run.outcome("Stopped "+title+".", "Stopping "+title+".", "Stopped "+title, title+" didn't stop"), nil
				default:
					// The stop has to finish before the start.
					stop, err := runJob(ctx, tn, shortJobWait, "app.stop", name)
					if err != nil {
						return Outcome{}, fmt.Errorf("it didn't stop: %w", err)
					}
					if !stop.finished {
						return Outcome{}, fmt.Errorf("%s is taking a while to stop, so it wasn't started again. Start it once it has stopped", title)
					}
					run, err := runJob(ctx, tn, shortJobWait, "app.start", name)
					if err != nil {
						return Outcome{}, fmt.Errorf("it stopped, but didn't start again: %w", err)
					}
					out := run.outcome("Restarted "+title+".", "Stopped "+title+"; it's starting again.", "Restarted "+title, title+" didn't start again")
					out.Ran = append(out.Ran, stop.id)
					return out, nil
				}
			},
		},
	}
}

func updateApp(tn *truenas.Client) Tool {
	type args struct {
		App     string `json:"app"`
		Version string `json:"version,omitempty"`
	}
	type plan struct {
		app            map[string]any
		from, to       string
		fromApp, toApp string
		// custom apps update by pulling newer images.
		custom bool
	}
	planned := func(ctx context.Context, raw json.RawMessage) (plan, error) {
		var a args
		if err := decodeArgs(raw, &a); err != nil {
			return plan{}, err
		}
		app, err := findApp(ctx, tn, a.App)
		if err != nil {
			return plan{}, err
		}
		title := appTitle(app)
		if strings.EqualFold(str(app, "state"), "STOPPED") {
			return plan{}, fmt.Errorf("%s is stopped, and TrueNAS only updates a running app; start it first", title)
		}
		if available, _ := app["upgrade_available"].(bool); !available {
			return plan{}, fmt.Errorf("%s has no update available", title)
		}
		if custom, _ := app["custom_app"].(bool); custom {
			// Its compose file names the images; newer ones are pulled.
			return plan{app: app, custom: true}, nil
		}
		version := a.Version
		if version == "" {
			version = "latest"
		}
		raw2, err := call(ctx, tn, "app.upgrade_summary", str(app, "name"), map[string]any{"app_version": version})
		if err != nil {
			return plan{}, err
		}
		var summary struct {
			UpgradeVersion      string `json:"upgrade_version"`
			UpgradeHumanVersion string `json:"upgrade_human_version"`
		}
		if err := json.Unmarshal(raw2, &summary); err != nil || summary.UpgradeVersion == "" {
			return plan{}, fmt.Errorf("TrueNAS didn't say which version %s would update to", appTitle(app))
		}
		if summary.UpgradeVersion == str(app, "version") {
			// Only its images have updates, as for apps from TrueNAS's
			// Custom App form: newer ones are pulled, as for custom apps.
			return plan{app: app, custom: true}, nil
		}
		p := plan{app: app, to: summary.UpgradeHumanVersion, toApp: summary.UpgradeVersion, from: str(app, "human_version"), fromApp: str(app, "version")}
		if p.to == "" {
			p.to = p.toApp
		}
		if p.from == "" {
			p.from = p.fromApp
		}
		return p, nil
	}

	return Tool{
		Def: Definition{
			Name: "update_app",
			Description: "Update an installed catalog app to its newest version (list_apps shows which have " +
				"updates). The app restarts. If the update misbehaves, rollback_app takes it back.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"app": {"type": "string", "description": "The app's name from list_apps."}
				},
				"required": ["app"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Updating the app…",
		Action: &Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (Card, error) {
				p, err := planned(ctx, raw)
				if err != nil {
					return Card{}, err
				}
				title := appTitle(p.app)
				if p.custom {
					detail := fmt.Sprintf("Pulls the newer images its compose file names, and restarts %s with them, so anyone using it is cut off for a minute or two.", title)
					return Card{Title: "Update " + title, Detail: detail, Risk: RiskDisruptive}, nil
				}
				detail := fmt.Sprintf("From %s to %s. %s restarts, so anyone using it is cut off for a minute or two. If the update misbehaves, it can be rolled back.",
					plain(p.from), plain(p.to), title)
				settled, _ := json.Marshal(args{App: str(p.app, "name"), Version: p.toApp})
				return Card{Title: "Update " + title, Detail: detail, Risk: RiskDisruptive, Args: settled}, nil
			},
			Run: func(ctx context.Context, raw json.RawMessage) (Outcome, error) {
				p, err := planned(ctx, raw)
				if err != nil {
					return Outcome{}, err
				}
				title := appTitle(p.app)
				if p.custom {
					run, err := runJob(ctx, tn, quickCheck, "app.upgrade", str(p.app, "name"))
					if err != nil {
						return Outcome{}, err
					}
					return run.outcome("Updated "+title+".", "Started updating "+title+".", "Updated "+title, title+" didn't update"), nil
				}
				run, err := runJob(ctx, tn, quickCheck, "app.upgrade", str(p.app, "name"), map[string]any{"app_version": p.toApp})
				if err != nil {
					return Outcome{}, err
				}
				to := plain(p.to)
				return run.outcome("Updated "+title+" to "+to+".", "Started updating "+title+" to "+to+".",
					"Updated "+title+" to "+to, title+" didn't update"), nil
			},
		},
	}
}

func rollbackApp(tn *truenas.Client) Tool {
	type args struct {
		App     string `json:"app"`
		Version string `json:"version,omitempty"`
	}
	type plan struct {
		app     map[string]any
		version string
	}
	planned := func(ctx context.Context, raw json.RawMessage) (plan, error) {
		var a args
		if err := decodeArgs(raw, &a); err != nil {
			return plan{}, err
		}
		app, err := findApp(ctx, tn, a.App)
		if err != nil {
			return plan{}, err
		}
		if strings.EqualFold(str(app, "state"), "STOPPED") {
			return plan{}, fmt.Errorf("%s is stopped, and TrueNAS only rolls back a running app; start it first", appTitle(app))
		}
		raw2, err := call(ctx, tn, "app.rollback_versions", str(app, "name"))
		if err != nil {
			return plan{}, err
		}
		var versions []string
		if err := json.Unmarshal(raw2, &versions); err != nil {
			return plan{}, fmt.Errorf("decode rollback versions: %w", err)
		}
		if len(versions) == 0 {
			return plan{}, fmt.Errorf("%s has no earlier version to roll back to", appTitle(app))
		}
		version := a.Version
		if version == "" {
			version = newestVersion(versions)
		} else if !contains(versions, version) {
			return plan{}, fmt.Errorf("%s can roll back to %s, not %s", appTitle(app), strings.Join(versions, ", "), version)
		}
		return plan{app: app, version: version}, nil
	}

	return Tool{
		Def: Definition{
			Name: "rollback_app",
			Description: "Roll an app back to the version it had before an update, the way back from an " +
				"update that went wrong. Omit version to go back to the most recent earlier one.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"app": {"type": "string", "description": "The app's name from list_apps."},
					"version": {"type": "string", "description": "The app version to go back to. Omit for the most recent earlier one."}
				},
				"required": ["app"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Rolling the app back…",
		Action: &Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (Card, error) {
				p, err := planned(ctx, raw)
				if err != nil {
					return Card{}, err
				}
				title := appTitle(p.app)
				detail := fmt.Sprintf("From version %s back to %s. %s restarts, so anyone using it is cut off for a minute or two. The data TrueNAS keeps for it goes back to how it was at %s; folders it uses on your own datasets aren't changed.",
					plain(str(p.app, "version")), plain(p.version), title, plain(p.version))
				settled, _ := json.Marshal(args{App: str(p.app, "name"), Version: p.version})
				return Card{Title: "Roll " + title + " back to " + plain(p.version), Detail: detail, Risk: RiskDisruptive, Args: settled}, nil
			},
			Run: func(ctx context.Context, raw json.RawMessage) (Outcome, error) {
				p, err := planned(ctx, raw)
				if err != nil {
					return Outcome{}, err
				}
				title := appTitle(p.app)
				run, err := runJob(ctx, tn, quickCheck, "app.rollback", str(p.app, "name"),
					map[string]any{"app_version": p.version, "rollback_snapshot": true})
				if err != nil {
					return Outcome{}, err
				}
				v := plain(p.version)
				return run.outcome("Rolled "+title+" back to "+v+".", "Started rolling "+title+" back to "+v+".",
					"Rolled "+title+" back to "+v, title+" didn't roll back"), nil
			},
		},
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// newestVersion picks the highest of dotted version numbers.
func newestVersion(versions []string) string {
	best := versions[0]
	for _, v := range versions[1:] {
		if versionLess(best, v) {
			best = v
		}
	}
	return best
}

// versionLess compares dotted versions part by part, numerically where
// both parts are numbers.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		var na, nb int
		_, errA := fmt.Sscanf(pa[i], "%d", &na)
		_, errB := fmt.Sscanf(pb[i], "%d", &nb)
		if errA == nil && errB == nil {
			if na != nb {
				return na < nb
			}
			continue
		}
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

// ----- VMs and containers --------------------------------------------------------

// machine is a VM or a (26) container, which start and stop alike.
type machine struct {
	noun, query, start, stop, restart string
	// startParams are what start takes after the id.
	startParams []any
}

var (
	vmMachine = machine{
		noun: "VM", query: "vm.query", start: "vm.start", stop: "vm.stop", restart: "vm.restart",
		// Don't start a VM the NAS hasn't the memory for.
		startParams: []any{map[string]any{"overcommit": false}},
	}
	containerMachine = machine{noun: "container", query: "container.query", start: "container.start", stop: "container.stop"}
)

func machineTool(tn *truenas.Client, m machine, name, description string) Tool {
	type args struct {
		Name   string `json:"name"`
		Action string `json:"action"`
	}
	planned := func(ctx context.Context, raw json.RawMessage) (string, map[string]any, error) {
		var a args
		if err := decodeArgs(raw, &a); err != nil {
			return "", nil, err
		}
		action := strings.ToLower(strings.TrimSpace(a.Action))
		switch action {
		case "start", "stop", "restart":
		default:
			return "", nil, fmt.Errorf("action is start, stop or restart, not %q", a.Action)
		}
		wanted := strings.TrimSpace(a.Name)
		if wanted == "" {
			return "", nil, fmt.Errorf("say which %s, by its name", m.noun)
		}
		rows, err := query(ctx, tn, m.query, []any{[]any{"name", "=", wanted}})
		if truenas.IsMethodNotFound(err) {
			return "", nil, fmt.Errorf("this TrueNAS version has no %ss", m.noun)
		}
		if err != nil {
			return "", nil, errors.New(truenas.Reason(err))
		}
		if len(rows) == 0 {
			return "", nil, fmt.Errorf("there's no %s named %q", m.noun, wanted)
		}
		status, _ := rows[0]["status"].(map[string]any)
		running := strings.EqualFold(str(status, "state"), "RUNNING")
		switch {
		case action == "start" && running:
			return "", nil, fmt.Errorf("%s is already running", quoted(str(rows[0], "name")))
		case action != "start" && !running:
			return "", nil, fmt.Errorf("%s isn't running", quoted(str(rows[0], "name")))
		}
		return action, rows[0], nil
	}

	return Tool{
		Def: Definition{
			Name:        name,
			Description: description,
			Parameters: json.RawMessage(fmt.Sprintf(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "The %s's name."},
					"action": {"type": "string", "enum": ["start", "stop", "restart"]}
				},
				"required": ["name", "action"],
				"additionalProperties": false
			}`, m.noun)),
		},
		StatusLine: fmt.Sprintf("Working on the %s…", m.noun),
		Action: &Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (Card, error) {
				action, row, err := planned(ctx, raw)
				if err != nil {
					return Card{}, err
				}
				label := fmt.Sprintf("the %s %s", m.noun, quoted(str(row, "name")))
				// TrueNAS switches off a guest that ignores the request to
				// shut down once its shutdown timeout has passed.
				grace := "If it hasn't shut down after its shutdown timeout, it's switched off."
				if secs, ok := num(row, "shutdown_timeout"); ok && secs > 0 {
					grace = fmt.Sprintf("If it hasn't shut down after %d seconds, it's switched off.", secs)
				}
				switch action {
				case "start":
					return Card{Title: "Start " + label, Detail: "It boots up, as if its power button were pressed.", Risk: RiskRoutine}, nil
				case "stop":
					return Card{Title: "Stop " + label, Detail: "It's asked to shut down cleanly, and whatever runs in it stops until it's started again. " + grace, Risk: RiskDisruptive}, nil
				default:
					return Card{Title: "Restart " + label, Detail: "It's asked to shut down cleanly, then starts again, so whatever runs in it is unavailable for a while. " + grace, Risk: RiskDisruptive}, nil
				}
			},
			Run: func(ctx context.Context, raw json.RawMessage) (Outcome, error) {
				action, row, err := planned(ctx, raw)
				if err != nil {
					return Outcome{}, err
				}
				id, _ := num(row, "id")
				label := fmt.Sprintf("the %s %s", m.noun, quoted(str(row, "name")))
				switch action {
				case "start":
					if err := startMachine(ctx, tn, m, id); err != nil {
						return Outcome{}, err
					}
					return Outcome{Message: "Started " + label + "."}, nil
				case "stop":
					run, err := runJob(ctx, tn, shortJobWait, m.stop, id, stopOptions)
					if err != nil {
						return Outcome{}, err
					}
					return run.outcome("Stopped "+label+".", "Asked "+label+" to shut down.", "Stopped "+label, "Couldn't stop "+label), nil
				default:
					if m.restart != "" {
						run, err := runJob(ctx, tn, shortJobWait, m.restart, id)
						if err != nil {
							return Outcome{}, err
						}
						return run.outcome("Restarted "+label+".", "Restarting "+label+".", "Restarted "+label, "Couldn't restart "+label), nil
					}
					// TrueNAS switches the guest off once its shutdown
					// timeout has passed, so wait that long, and a margin.
					stop, err := runJob(ctx, tn, shutdownWait(row), m.stop, id, stopOptions)
					if err != nil {
						return Outcome{}, fmt.Errorf("it didn't stop: %w", err)
					}
					if !stop.finished {
						return Outcome{}, errors.New("it's taking a while to shut down, so it wasn't started again. Start it once it has stopped")
					}
					if err := startMachine(ctx, tn, m, id); err != nil {
						return Outcome{}, fmt.Errorf("it stopped, but didn't start again: %w", err)
					}
					return Outcome{Message: "Restarted " + label + ".", Ran: []int64{stop.id}}, nil
				}
			},
		},
	}
}

// shutdownWait is how long stopping a VM or container can take: its
// shutdown timeout (90 seconds unless set), after which TrueNAS switches it
// off, and a margin.
func shutdownWait(row map[string]any) time.Duration {
	secs, ok := num(row, "shutdown_timeout")
	if !ok || secs <= 0 {
		secs = 90
	}
	return time.Duration(secs)*guestSecond + shutdownGrace
}

// stopOptions stop a VM or container cleanly, switching it off if it
// ignores the request past its shutdown timeout: without that, TrueNAS
// reports a stop the guest ignored as done.
var stopOptions = map[string]any{"force": false, "force_after_timeout": true}

// startMachine starts a VM or container, waiting for it when TrueNAS
// makes the start a job.
func startMachine(ctx context.Context, tn *truenas.Client, m machine, id int64) error {
	raw, err := call(ctx, tn, m.start, append([]any{id}, m.startParams...)...)
	if err != nil {
		return err
	}
	jobID, err := truenas.JobID(raw)
	if err != nil {
		// Not a job: it started (or said why not) there and then.
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, shortJobWait)
	defer cancel()
	if job, _ := tn.WaitJob(waitCtx, jobID, jobPoll); job.Finished() && !job.Succeeded() {
		return errors.New(job.Failure())
	}
	return nil
}

func vmAction(tn *truenas.Client) Tool {
	return machineTool(tn, vmMachine, "vm_action",
		"Start, stop or restart a virtual machine (list_vms has their names). Stopping asks it to shut "+
			"down cleanly, like pressing its power button.")
}

func containerAction(tn *truenas.Client) Tool {
	return machineTool(tn, containerMachine, "container_action",
		"Start, stop or restart a container (TrueNAS 26 and later; list_containers has their names). "+
			"Restart stops it cleanly and starts it again.")
}

// ----- services -----------------------------------------------------------------

// serviceNames are what the user calls TrueNAS's services.
var serviceNames = map[string]string{
	"cifs": "SMB", "nfs": "NFS", "ssh": "SSH", "ftp": "FTP", "iscsitarget": "iSCSI",
	"ups": "UPS", "snmp": "SNMP", "nvmet": "NVMe-oF", "webshare": "WebShare",
}

// serviceImpact says who a service's restart or stop affects.
var serviceImpact = map[string]string{
	"cifs":        "Windows and Mac file shares drop, and files open over the network may need reopening.",
	"nfs":         "NFS clients lose their mounts until it's back.",
	"ssh":         "New SSH logins wait until it's back.",
	"ftp":         "FTP transfers in progress are cut off.",
	"iscsitarget": "Machines using its iSCSI disks lose them, which can corrupt what they were writing.",
	"ups":         "The NAS stops watching the UPS, so it won't shut down cleanly if the power goes.",
	"nvmet":       "Machines using its NVMe-oF disks lose them, which can corrupt what they were writing.",
}

// serviceLabel is a service as the user knows it: "SMB" for cifs.
func serviceLabel(service string) string {
	if name, ok := serviceNames[service]; ok {
		return name
	}
	return strings.ToUpper(plain(service))
}

func serviceAction(tn *truenas.Client) Tool {
	type args struct {
		Service string `json:"service"`
		Action  string `json:"action"`
	}
	planned := func(ctx context.Context, raw json.RawMessage) (string, map[string]any, error) {
		var a args
		if err := decodeArgs(raw, &a); err != nil {
			return "", nil, err
		}
		action := strings.ToLower(strings.TrimSpace(a.Action))
		switch action {
		case "start", "stop", "restart":
		default:
			return "", nil, fmt.Errorf("action is start, stop or restart, not %q", a.Action)
		}
		name := strings.ToLower(strings.TrimSpace(a.Service))
		if name == "smb" {
			name = "cifs"
		}
		rows, err := query(ctx, tn, "service.query", []any{[]any{"service", "=", name}})
		if err != nil {
			return "", nil, errors.New(truenas.Reason(err))
		}
		if len(rows) == 0 {
			return "", nil, fmt.Errorf("there's no service named %q; list_services has their names", a.Service)
		}
		// The state can be UNKNOWN when TrueNAS couldn't tell in time.
		state := strings.ToUpper(str(rows[0], "state"))
		switch {
		case action == "start" && state == "RUNNING":
			return "", nil, fmt.Errorf("%s is already running", serviceLabel(name))
		case action == "stop" && state == "STOPPED":
			return "", nil, fmt.Errorf("%s isn't running", serviceLabel(name))
		}
		return action, rows[0], nil
	}

	return Tool{
		Def: Definition{
			Name: "service_action",
			Description: "Start, stop or restart a TrueNAS service such as SMB (cifs), NFS or SSH. " +
				"list_services has their names and states.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"service": {"type": "string", "description": "The service's name from list_services, such as cifs, nfs or ssh."},
					"action": {"type": "string", "enum": ["start", "stop", "restart"]}
				},
				"required": ["service", "action"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Working on the service…",
		Action: &Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (Card, error) {
				action, row, err := planned(ctx, raw)
				if err != nil {
					return Card{}, err
				}
				service := str(row, "service")
				label := serviceLabel(service)
				impact := serviceImpact[service]
				if impact == "" {
					impact = "Whatever uses it is cut off until it's back."
				}
				switch action {
				case "start":
					detail := label + " starts."
					if enabled, _ := row["enable"].(bool); !enabled {
						detail += " It isn't set to start at boot, so it stays off after the NAS restarts."
					}
					return Card{Title: "Start the " + label + " service", Detail: detail, Risk: RiskRoutine}, nil
				case "stop":
					detail := impact + " It stays off until it's started again"
					if enabled, _ := row["enable"].(bool); enabled {
						detail += ", or the NAS restarts: it's set to start at boot."
					} else {
						detail += "."
					}
					return Card{Title: "Stop the " + label + " service", Detail: detail, Risk: RiskDisruptive}, nil
				default:
					return Card{Title: "Restart the " + label + " service", Detail: "It stops for a few seconds. " + impact, Risk: RiskDisruptive}, nil
				}
			},
			Run: func(ctx context.Context, raw json.RawMessage) (Outcome, error) {
				action, row, err := planned(ctx, raw)
				if err != nil {
					return Outcome{}, err
				}
				service := str(row, "service")
				label := serviceLabel(service)
				jobID, err := controlService(ctx, tn, action, service)
				if err != nil {
					return Outcome{}, err
				}
				var ran []int64
				if jobID != 0 {
					ran = []int64{jobID}
				}
				switch action {
				case "start":
					return Outcome{Message: "Started the " + label + " service.", Ran: ran}, nil
				case "stop":
					return Outcome{Message: "Stopped the " + label + " service.", Ran: ran}, nil
				default:
					return Outcome{Message: "Restarted the " + label + " service.", Ran: ran}, nil
				}
			},
		},
	}
}

// serviceWait is how long a service gets to start or stop: TrueNAS's own
// limit is 120 seconds.
var serviceWait = 130 * time.Second

// controlService starts, stops or restarts a service, returning the job it
// ran: service.control, a job, on 25.10 and later; service.start and
// friends before.
func controlService(ctx context.Context, tn *truenas.Client, action, service string) (int64, error) {
	slow := fmt.Errorf("%s is taking a while; check it in Services", serviceLabel(service))
	raw, err := tn.Call(ctx, "service.control", strings.ToUpper(action), service, map[string]any{"silent": false})
	if err != nil {
		if !truenas.IsMethodNotFound(err) {
			return 0, errors.New(truenas.Reason(err))
		}
		raw, err = call(ctx, tn, "service."+action, service, map[string]any{"silent": false})
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, slow
		}
		if err != nil {
			return 0, err
		}
		return 0, serviceResult(raw, action, service)
	}
	jobID, err := truenas.JobID(raw)
	if err != nil {
		return 0, serviceResult(raw, action, service)
	}
	waitCtx, cancel := context.WithTimeout(ctx, serviceWait)
	defer cancel()
	job, err := tn.WaitJob(waitCtx, jobID, jobPoll)
	if err != nil || !job.Finished() {
		return jobID, slow
	}
	if !job.Succeeded() {
		return jobID, jobError{id: jobID, reason: job.Failure()}
	}
	return jobID, serviceResult(job.Result, action, service)
}

// serviceResult reads the true or false a service call returns: whether a
// start left the service running.
func serviceResult(raw json.RawMessage, action, service string) error {
	var ok bool
	if json.Unmarshal(raw, &ok) != nil || ok || action == "stop" {
		return nil
	}
	return fmt.Errorf("%s didn't come up; its logs may say why", serviceLabel(service))
}
