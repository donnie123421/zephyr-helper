package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/tools"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// The tools here book things for later. Each is an action: the user
// approves a card saying what will run and when, written by the tool, and
// only then is it booked.

// Schedulable are the actions that can be booked, and whether they can
// repeat. Updates repeat through the update rule, with its safeguard.
var Schedulable = map[string]bool{
	"scrub_pool": true, "create_snapshot": true, "run_backup_task": true,
	"app_action": true, "service_action": true, "vm_action": true, "container_action": true,
	"update_app": false,
}

// whenSchema is the fields every booking tool takes for when.
const whenSchema = `
	"repeat": {"type": "string", "enum": ["once", "daily", "weekly", "monthly"]},
	"date": {"type": "string", "description": "YYYY-MM-DD, for once. Times are the NAS's local time."},
	"time": {"type": "string", "description": "HH:MM, 24-hour, the NAS's local time."},
	"weekday": {"type": "string", "enum": ["monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"], "description": "For weekly."},
	"day_of_month": {"type": "integer", "description": "1 to 28, for monthly."}`

// whenArgs are those fields.
type whenArgs struct {
	Repeat     string `json:"repeat"`
	Date       string `json:"date,omitempty"`
	Time       string `json:"time"`
	Weekday    string `json:"weekday,omitempty"`
	DayOfMonth int    `json:"day_of_month,omitempty"`
}

// Booking holds what the booking tools need.
type Booking struct {
	Store    *Store
	Registry *tools.Registry
	NAS      NAS
	// Model says which model writes check-ins: "Claude (claude-opus-5)",
	// and whether it's hosted, so uses an API key.
	Model func() (label string, hosted bool)
	Now   func() time.Time
}

// Tools are the booking tools and list_schedules.
func (b Booking) Tools() []tools.Tool {
	if b.Now == nil {
		b.Now = time.Now
	}
	return []tools.Tool{b.scheduleAction(), b.createUpdateRule(), b.createCheckIn(), b.listSchedules()}
}

// when reads the fields into a When in the NAS's time zone, and its first
// run, which must be within a year.
func (b Booking) when(ctx context.Context, a whenArgs) (When, time.Time, error) {
	w := When{Repeat: a.Repeat, Date: a.Date, Time: a.Time, Weekday: strings.ToLower(a.Weekday), Day: a.DayOfMonth, Zone: b.zone(ctx)}
	if err := w.Validate(); err != nil {
		return When{}, time.Time{}, err
	}
	first, err := within(w, b.Now())
	return w, first, err
}

// within is a booking's first run, which must come, and within a year.
func within(w When, now time.Time) (time.Time, error) {
	first, ok := w.Next(now)
	if !ok {
		return time.Time{}, errors.New("that time has already passed on the NAS's clock")
	}
	if first.Sub(now) > 366*24*time.Hour {
		return time.Time{}, errors.New("that's more than a year away")
	}
	return first, nil
}

// zone is the NAS's time zone, UTC when it can't be read.
func (b Booking) zone(ctx context.Context) string {
	raw, err := b.NAS.Call(ctx, "system.general.config")
	if err != nil {
		return "UTC"
	}
	var general struct {
		Timezone string `json:"timezone"`
	}
	if json.Unmarshal(raw, &general) != nil || general.Timezone == "" {
		return "UTC"
	}
	if _, err := time.LoadLocation(general.Timezone); err != nil {
		return "UTC"
	}
	return general.Timezone
}

// firstRun says when a booking first runs: "2:00 am on Sun 4 Oct".
func firstRun(t time.Time, w When) string {
	return t.In(locationOf(w)).Format("3:04 pm on Mon 2 Jan")
}

// ----- schedule_action -----------------------------------------------------------

type actionBooking struct {
	Action        string `json:"action"`
	ArgumentsJSON string `json:"arguments_json"`
	whenArgs
}

type settledAction struct {
	Action    string          `json:"action"`
	Arguments json.RawMessage `json:"arguments"`
	When      When            `json:"when"`
}

func (b Booking) scheduleAction() tools.Tool {
	// describe checks the action against the NAS as it is now and writes
	// the card for booking it.
	describe := func(ctx context.Context, action string, args json.RawMessage, w When, first time.Time) (tools.Card, error) {
		repeats, ok := Schedulable[action]
		switch {
		case !ok:
			return tools.Card{}, fmt.Errorf("%s can't be booked for later", action)
		case w.Repeat != "once" && !repeats:
			return tools.Card{}, errors.New("app updates repeat through create_update_rule, which rolls back an update that goes wrong")
		case action == "create_snapshot" && w.Repeat != "once" && strings.Contains(string(args), `"name"`):
			return tools.Card{}, errors.New("a repeating snapshot needs a new name each time: leave name out")
		}
		underlying := b.Registry.Action(action)
		if underlying == nil {
			return tools.Card{}, fmt.Errorf("%s isn't available", action)
		}
		card, err := underlying.Describe(ctx, args)
		if err != nil {
			return tools.Card{}, err
		}
		note := fmt.Sprintf("Booked for %s, NAS time. It's checked against the NAS again then, and you're notified if it fails.", firstRun(first, w))
		if w.Repeat != "once" {
			note = fmt.Sprintf("First runs at %s, NAS time. Each run is checked against the NAS first, and you're notified if one fails. Pause or remove it in Scheduled.", firstRun(first, w))
		}
		if action == "update_app" {
			note += " It updates to the newest version then."
		}
		settled, _ := json.Marshal(settledAction{Action: action, Arguments: args, When: w})
		return tools.Card{Title: card.Title + " " + w.Phrase(), Detail: card.Detail + "\n\n" + note, Risk: card.Risk, Args: settled}, nil
	}
	// read reads either the model's arguments or the card's settled ones.
	read := func(ctx context.Context, raw json.RawMessage) (string, json.RawMessage, When, time.Time, error) {
		var settled settledAction
		if json.Unmarshal(raw, &settled) == nil && settled.Action != "" && settled.When.Repeat != "" {
			if err := settled.When.Validate(); err != nil {
				return "", nil, When{}, time.Time{}, err
			}
			first, err := within(settled.When, b.Now())
			if err != nil {
				return "", nil, When{}, time.Time{}, err
			}
			return settled.Action, settled.Arguments, settled.When, first, nil
		}
		var a actionBooking
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", nil, When{}, time.Time{}, fmt.Errorf("bad arguments: %w", err)
		}
		args := json.RawMessage(strings.TrimSpace(a.ArgumentsJSON))
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		if !json.Valid(args) {
			return "", nil, When{}, time.Time{}, errors.New("arguments_json isn't JSON")
		}
		w, first, err := b.when(ctx, a.whenArgs)
		return a.Action, args, w, first, err
	}

	return tools.Tool{
		Def: tools.Definition{
			Name: "schedule_action",
			Description: "Book one of the actions for later: once, or repeating daily, weekly or monthly, " +
				"at a time of day in the NAS's local time. action is the action tool's name, and " +
				"arguments_json its arguments as a JSON string, as you'd pass the tool. Actions that can be " +
				"booked: scrub_pool, create_snapshot (repeating ones without a name), run_backup_task, " +
				"app_action, service_action, vm_action, container_action, and update_app (once; repeating " +
				"updates use create_update_rule).",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"action": {"type": "string", "description": "The action tool's name, such as scrub_pool."},
					"arguments_json": {"type": "string", "description": "The action's arguments as a JSON string, such as {\"pool\":\"tank\"}."},` + whenSchema + `
				},
				"required": ["action", "arguments_json", "repeat", "time"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Booking it…",
		Action: &tools.Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (tools.Card, error) {
				action, args, w, first, err := read(ctx, raw)
				if err != nil {
					return tools.Card{}, err
				}
				return describe(ctx, action, args, w, first)
			},
			Run: func(ctx context.Context, raw json.RawMessage) (tools.Outcome, error) {
				action, args, w, first, err := read(ctx, raw)
				if err != nil {
					return tools.Outcome{}, err
				}
				card, err := describe(ctx, action, args, w, first)
				if err != nil {
					return tools.Outcome{}, err
				}
				job, err := b.Store.Add(ctx, Job{Kind: KindAction, Tool: action, Arguments: args, Title: card.Title,
					Detail: card.Detail, Risk: string(card.Risk), When: w, NextRun: &first})
				if err != nil {
					return tools.Outcome{}, err
				}
				return tools.Outcome{Message: "Booked: " + job.Title + "."}, nil
			},
		},
	}
}

// ----- create_update_rule -------------------------------------------------------

type ruleBooking struct {
	Apps []string `json:"apps"`
	whenArgs
}

type settledRule struct {
	Apps []string `json:"apps"`
	When When     `json:"when"`
}

func (b Booking) createUpdateRule() tools.Tool {
	read := func(ctx context.Context, raw json.RawMessage) ([]string, When, time.Time, error) {
		var settled settledRule
		if json.Unmarshal(raw, &settled) == nil && settled.When.Repeat != "" {
			if err := settled.When.Validate(); err != nil {
				return nil, When{}, time.Time{}, err
			}
			first, err := within(settled.When, b.Now())
			return settled.Apps, settled.When, first, err
		}
		var a ruleBooking
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, When{}, time.Time{}, fmt.Errorf("bad arguments: %w", err)
		}
		if a.Repeat == "once" {
			return nil, When{}, time.Time{}, errors.New("a rule repeats: daily, weekly or monthly. To update once later, use schedule_action with update_app")
		}
		w, first, err := b.when(ctx, a.whenArgs)
		return a.Apps, w, first, err
	}
	describe := func(ctx context.Context, apps []string, w When, first time.Time) (tools.Card, error) {
		titles, err := b.appTitles(ctx, apps)
		if err != nil {
			return tools.Card{}, err
		}
		which, each := "apps", "every app that has a new version"
		if len(titles) > 0 {
			which = joinNames(titles)
			each = which + " when there's a new version"
		}
		detail := fmt.Sprintf("Each run updates %s, one at a time. One that isn't running again within %d minutes "+
			"is rolled back to the version it had, and you're notified. Nothing runs while a pool isn't healthy, and you "+
			"get a summary when something's updated. Custom apps, and updates that only bring newer images, are left "+
			"alone: they can't be rolled back. First runs at %s, NAS time; pause or remove it in Scheduled.",
			each, int(defaultRuleTiming.runningFor.Minutes()), firstRun(first, w))
		settled, _ := json.Marshal(settledRule{Apps: apps, When: w})
		return tools.Card{Title: fmt.Sprintf("Update %s automatically %s", which, w.Phrase()), Detail: detail,
			Risk: tools.RiskDisruptive, Args: settled}, nil
	}

	return tools.Tool{
		Def: tools.Definition{
			Name: "create_update_rule",
			Description: "Set up a standing rule that updates apps automatically, daily, weekly or monthly at a time " +
				"of day in the NAS's local time. apps limits it to those apps (names from list_apps); leave it " +
				"empty for every app. Each update is checked, and an app that doesn't come back up is rolled back.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"apps": {"type": "array", "items": {"type": "string"}, "description": "App names from list_apps. Empty for all apps."},` + whenSchema + `
				},
				"required": ["apps", "repeat", "time"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Setting up the rule…",
		Action: &tools.Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (tools.Card, error) {
				apps, w, first, err := read(ctx, raw)
				if err != nil {
					return tools.Card{}, err
				}
				return describe(ctx, apps, w, first)
			},
			Run: func(ctx context.Context, raw json.RawMessage) (tools.Outcome, error) {
				apps, w, first, err := read(ctx, raw)
				if err != nil {
					return tools.Outcome{}, err
				}
				card, err := describe(ctx, apps, w, first)
				if err != nil {
					return tools.Outcome{}, err
				}
				settings, _ := json.Marshal(UpdateRule{Apps: apps})
				job, err := b.Store.Add(ctx, Job{Kind: KindUpdateRule, Arguments: settings, Title: card.Title,
					Detail: card.Detail, Risk: string(card.Risk), When: w, NextRun: &first})
				if err != nil {
					return tools.Outcome{}, err
				}
				return tools.Outcome{Message: "Set up: " + job.Title + "."}, nil
			},
		},
	}
}

// appTitles checks the named apps exist and can be rolled back, and titles
// them as the catalog does.
func (b Booking) appTitles(ctx context.Context, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	raw, err := b.NAS.Call(ctx, "app.query")
	if err != nil {
		return nil, errors.New(truenas.Reason(err))
	}
	var apps []struct {
		Name      string `json:"name"`
		CustomApp bool   `json:"custom_app"`
		Metadata  struct {
			Title string `json:"title"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &apps); err != nil {
		return nil, err
	}
	var titles []string
	for _, name := range names {
		found := false
		for _, a := range apps {
			if a.Name != name {
				continue
			}
			found = true
			if a.CustomApp {
				return nil, fmt.Errorf("%s is a custom app, which the rule leaves alone: it can't be rolled back", name)
			}
			title := a.Metadata.Title
			if title == "" {
				title = a.Name
			}
			titles = append(titles, title)
		}
		if !found {
			return nil, fmt.Errorf("there's no app named %q; call list_apps for their names", name)
		}
	}
	return titles, nil
}

// ----- create_checkin ----------------------------------------------------------

// checkInTokens is roughly what one check-in uses: the briefing prompt and
// what the read-only tools return, then a short answer.
const checkInTokens = "20,000"

func (b Booking) createCheckIn() tools.Tool {
	read := func(ctx context.Context, raw json.RawMessage) (When, time.Time, error) {
		var settled struct {
			When When `json:"when"`
		}
		if json.Unmarshal(raw, &settled) == nil && settled.When.Repeat != "" {
			if err := settled.When.Validate(); err != nil {
				return When{}, time.Time{}, err
			}
			first, err := within(settled.When, b.Now())
			return settled.When, first, err
		}
		var a whenArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return When{}, time.Time{}, fmt.Errorf("bad arguments: %w", err)
		}
		if a.Repeat != "daily" && a.Repeat != "weekly" {
			return When{}, time.Time{}, errors.New("a check-in repeats daily or weekly")
		}
		return b.when(ctx, a)
	}
	describe := func(w When, first time.Time) tools.Card {
		label, hosted := "the model on your NAS", false
		if b.Model != nil {
			label, hosted = b.Model()
		}
		cost := "It runs on " + label + "."
		if hosted {
			cost = fmt.Sprintf("It uses %s with your own API key, roughly %s tokens each time.", label, checkInTokens)
		}
		detail := fmt.Sprintf("Zephyr looks over the NAS and writes you a briefing: what's fine, what needs attention, and "+
			"what to do about it. It's saved with your conversations, and you're notified when it's ready. %s It only "+
			"looks: any fix it suggests is yours to ask for. First at %s, NAS time.", cost, firstRun(first, w))
		settled, _ := json.Marshal(map[string]any{"when": w})
		return tools.Card{Title: "Check in on the NAS " + w.Phrase(), Detail: detail, Risk: tools.RiskRoutine, Args: settled}
	}

	return tools.Tool{
		Def: tools.Definition{
			Name: "create_checkin",
			Description: "Set up a regular check-in, daily or weekly at a time of day in the NAS's local time: " +
				"Zephyr looks the NAS over with the current model and writes a briefing, saved as a conversation, " +
				"with a notification.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {` + whenSchema + `
				},
				"required": ["repeat", "time"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Setting up the check-in…",
		Action: &tools.Action{
			Describe: func(ctx context.Context, raw json.RawMessage) (tools.Card, error) {
				w, first, err := read(ctx, raw)
				if err != nil {
					return tools.Card{}, err
				}
				return describe(w, first), nil
			},
			Run: func(ctx context.Context, raw json.RawMessage) (tools.Outcome, error) {
				w, first, err := read(ctx, raw)
				if err != nil {
					return tools.Outcome{}, err
				}
				card := describe(w, first)
				job, err := b.Store.Add(ctx, Job{Kind: KindCheckIn, Title: card.Title, Detail: card.Detail,
					Risk: string(card.Risk), When: w, NextRun: &first})
				if err != nil {
					return tools.Outcome{}, err
				}
				return tools.Outcome{Message: "Set up: " + job.Title + "."}, nil
			},
		},
	}
}

// ----- list_schedules -----------------------------------------------------------

func (b Booking) listSchedules() tools.Tool {
	return tools.Tool{
		Def: tools.Definition{
			Name:        "list_schedules",
			Description: "List what's booked: actions for later, standing rules and check-ins, with when each runs next and how its last run went.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		StatusLine: "Checking what's booked…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			jobs, err := b.Store.List(ctx)
			if err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(jobs))
			for _, job := range jobs {
				entry := map[string]any{"id": job.ID, "kind": job.Kind, "title": job.Title, "paused": job.Paused}
				if job.NextRun != nil {
					entry["next_run"] = job.NextRun.In(locationOf(job.When)).Format("Mon 2 Jan 3:04 pm")
				}
				if job.LastStatus != "" {
					entry["last_run"] = job.LastStatus + ": " + job.LastMessage
				}
				out = append(out, entry)
			}
			data, err := json.Marshal(map[string]any{"scheduled": out})
			return string(data), err
		},
	}
}
