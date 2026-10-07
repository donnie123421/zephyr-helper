package schedule

import (
	"bytes"
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
//
// Describe reads only what the model may send, and works out the rest,
// the NAS's time zone included. The card it writes carries what was
// approved, as it was shown; Run books exactly that. Only the helper sees
// a card's arguments, so the model can't book anything but a card.

// Schedulable are the actions that can be booked, and whether they can
// repeat. Updates repeat through the update rule, with its safeguard.
var Schedulable = map[string]bool{
	"scrub_pool": true, "create_snapshot": true, "run_backup_task": true,
	"app_action": true, "service_action": true, "vm_action": true, "container_action": true,
	"update_app": false,
}

// maxArguments bounds the arguments booked with an action.
const maxArguments = 4 << 10

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

// approved is what a booking card holds: what will be booked, as the card
// showed it.
type approved struct {
	Kind      Kind            `json:"kind"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	When      When            `json:"when"`
	Title     string          `json:"title"`
	Detail    string          `json:"detail"`
	Risk      tools.Risk      `json:"risk"`
}

// card shows a booking for approval, carrying it for Run.
func (a approved) card() tools.Card {
	args, _ := json.Marshal(a)
	return tools.Card{Title: a.Title, Detail: a.Detail, Risk: a.Risk, Args: args}
}

// book books an approved card of the kind.
func (b Booking) book(ctx context.Context, raw json.RawMessage, kind Kind) (tools.Outcome, error) {
	job, err := b.bookJob(ctx, raw, kind)
	if err != nil {
		return tools.Outcome{}, err
	}
	if kind == KindAction {
		return tools.Outcome{Message: "Booked: " + job.Title + "."}, nil
	}
	return tools.Outcome{Message: "Set up: " + job.Title + "."}, nil
}

// bookJob books an approved card of the kind, and returns it booked.
func (b Booking) bookJob(ctx context.Context, raw json.RawMessage, kind Kind) (Job, error) {
	job, err := b.jobFor(raw, kind)
	if err != nil {
		return Job{}, err
	}
	return b.Store.Add(ctx, job)
}

// jobFor is the job an approved card of the kind books, its first run
// worked out now.
func (b Booking) jobFor(raw json.RawMessage, kind Kind) (Job, error) {
	var a approved
	if err := strict(raw, &a); err != nil || a.Kind != kind || a.Title == "" {
		return Job{}, errors.New("that isn't a booking card")
	}
	if err := a.When.Validate(); err != nil {
		return Job{}, err
	}
	first, err := within(a.When, b.Now())
	if err != nil {
		return Job{}, err
	}
	return Job{Kind: a.Kind, Tool: a.Tool, Arguments: a.Arguments, Title: a.Title,
		Detail: a.Detail, Risk: string(a.Risk), When: a.When, NextRun: &first}, nil
}

// bookingKinds are the booking tools, by name, and the kind each books:
// what the app's own booking (Handler) can ask for.
var bookingKinds = map[string]Kind{
	"schedule_action":    KindAction,
	"create_update_rule": KindUpdateRule,
	"create_checkin":     KindCheckIn,
}

// describe writes the card a booking tool would, for its arguments.
func (b Booking) describe(ctx context.Context, name string, raw json.RawMessage) (tools.Card, Kind, error) {
	kind, ok := bookingKinds[name]
	if !ok {
		return tools.Card{}, "", fmt.Errorf("%s isn't a way to book something", name)
	}
	for _, tool := range b.Tools() {
		if tool.Def.Name == name && tool.Action != nil {
			card, err := tool.Action.Describe(ctx, raw)
			return card, kind, err
		}
	}
	return tools.Card{}, "", fmt.Errorf("%s isn't available", name)
}

// strict decodes JSON into v, refusing fields v doesn't have.
func strict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}

// when reads the model's fields into a When in the NAS's time zone, and
// its first run, which must be within a year.
func (b Booking) when(ctx context.Context, a whenArgs) (When, time.Time, error) {
	zone, err := b.zone(ctx)
	if err != nil {
		return When{}, time.Time{}, err
	}
	w := When{Repeat: a.Repeat, Date: a.Date, Time: a.Time, Weekday: strings.ToLower(a.Weekday), Day: a.DayOfMonth, Zone: zone}
	if err := w.Validate(); err != nil {
		return When{}, time.Time{}, err
	}
	w.Time = w.clockText()
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

// zone is the NAS's time zone. A booking can't do without it: its times
// are the NAS's, as the card says.
func (b Booking) zone(ctx context.Context) (string, error) {
	raw, err := b.NAS.Call(ctx, "system.general.config")
	if err != nil {
		return "", fmt.Errorf("couldn't read the NAS's time zone: %s", truenas.Reason(err))
	}
	var general struct {
		Timezone string `json:"timezone"`
	}
	if json.Unmarshal(raw, &general) != nil || general.Timezone == "" {
		return "", errors.New("couldn't read the NAS's time zone")
	}
	if _, err := time.LoadLocation(general.Timezone); err != nil {
		return "", fmt.Errorf("the NAS's time zone, %s, is unknown to the helper", general.Timezone)
	}
	return general.Timezone, nil
}

// firstRun says when a booking first runs: "2:00 am on Sun 4 Oct". When
// its time doesn't happen that night, as the clocks go forward through it,
// the run is at the first time after, and it says why.
func firstRun(t time.Time, w When) string {
	local := t.In(locationOf(w))
	text := local.Format("3:04 pm on Mon 2 Jan")
	if local.Format("15:04") != w.clockText() {
		text += " (the clocks go forward that night)"
	}
	return text
}

// ----- schedule_action -----------------------------------------------------------

type actionBooking struct {
	Action        string `json:"action"`
	ArgumentsJSON string `json:"arguments_json"`
	whenArgs
}

func (b Booking) scheduleAction() tools.Tool {
	describe := func(ctx context.Context, raw json.RawMessage) (tools.Card, error) {
		var a actionBooking
		if err := strict(raw, &a); err != nil {
			return tools.Card{}, err
		}
		args := json.RawMessage(strings.TrimSpace(a.ArgumentsJSON))
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		var fields map[string]any
		switch {
		case len(args) > maxArguments:
			return tools.Card{}, errors.New("arguments_json is too long")
		case json.Unmarshal(args, &fields) != nil:
			return tools.Card{}, errors.New("arguments_json isn't a JSON object")
		}
		repeats, ok := Schedulable[a.Action]
		switch {
		case !ok:
			return tools.Card{}, fmt.Errorf("%s can't be booked for later", a.Action)
		case a.Repeat != "once" && !repeats:
			return tools.Card{}, errors.New("app updates repeat through create_update_rule, which rolls back an update that goes wrong")
		}
		name, _ := fields["name"].(string)
		if a.Action == "create_snapshot" && a.Repeat != "once" && name != "" {
			return tools.Card{}, errors.New("a repeating snapshot needs a new name each time: leave name out")
		}
		underlying := b.Registry.Action(a.Action)
		if underlying == nil {
			return tools.Card{}, fmt.Errorf("%s isn't available", a.Action)
		}
		w, first, err := b.when(ctx, a.whenArgs)
		if err != nil {
			return tools.Card{}, err
		}
		card, err := underlying.Describe(tools.ForLater(ctx), args)
		if err != nil {
			return tools.Card{}, err
		}
		detail := card.Detail
		if a.Action == "create_snapshot" && name == "" && strings.HasPrefix(detail, "Named ") {
			// The name is made when it's taken, not now.
			if i := strings.Index(detail, ". "); i >= 0 {
				detail = "Named for when it's taken." + detail[i+1:]
			}
		}
		note := fmt.Sprintf("Booked for %s, NAS time. It's checked against the NAS again then, and you're notified if it fails.", firstRun(first, w))
		if w.Repeat != "once" {
			note = fmt.Sprintf("First runs at %s, NAS time. Each run is checked against the NAS first, and you're notified if one fails. Pause or remove it in Scheduled.", firstRun(first, w))
		}
		if a.Action == "update_app" {
			note += " It updates to the newest version then."
		}
		return approved{
			Kind: KindAction, Tool: a.Action, Arguments: args, When: w,
			Title: card.Title + " " + w.Phrase(), Detail: detail + "\n\n" + note, Risk: card.Risk,
		}.card(), nil
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
			Describe: describe,
			Run: func(ctx context.Context, raw json.RawMessage) (tools.Outcome, error) {
				return b.book(ctx, raw, KindAction)
			},
		},
	}
}

// ----- create_update_rule -------------------------------------------------------

type ruleBooking struct {
	Apps []string `json:"apps"`
	whenArgs
}

func (b Booking) createUpdateRule() tools.Tool {
	describe := func(ctx context.Context, raw json.RawMessage) (tools.Card, error) {
		var a ruleBooking
		if err := strict(raw, &a); err != nil {
			return tools.Card{}, err
		}
		if a.Repeat == "once" {
			return tools.Card{}, errors.New("a rule repeats: daily, weekly or monthly. To update once later, use schedule_action with update_app")
		}
		if len(a.Apps) > MaxJobs {
			return tools.Card{}, fmt.Errorf("a rule names at most %d apps; leave apps empty for all of them", MaxJobs)
		}
		w, first, err := b.when(ctx, a.whenArgs)
		if err != nil {
			return tools.Card{}, err
		}
		apps, titles, err := b.appTitles(ctx, a.Apps)
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
		rule, _ := json.Marshal(UpdateRule{Apps: apps})
		return approved{
			Kind: KindUpdateRule, Arguments: rule, When: w,
			Title: fmt.Sprintf("Update %s automatically %s", which, w.Phrase()), Detail: detail, Risk: tools.RiskDisruptive,
		}.card(), nil
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
			Describe: describe,
			Run: func(ctx context.Context, raw json.RawMessage) (tools.Outcome, error) {
				return b.book(ctx, raw, KindUpdateRule)
			},
		},
	}
}

// appTitles checks the named apps exist and can be rolled back, and gives
// their names, each once, and titles as the catalog has them.
func (b Booking) appTitles(ctx context.Context, names []string) ([]string, []string, error) {
	if len(names) == 0 {
		return nil, nil, nil
	}
	raw, err := b.NAS.Call(ctx, "app.query")
	if err != nil {
		return nil, nil, errors.New(truenas.Reason(err))
	}
	var apps []struct {
		Name      string `json:"name"`
		CustomApp bool   `json:"custom_app"`
		Metadata  struct {
			Title string `json:"title"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &apps); err != nil {
		return nil, nil, err
	}
	var kept, titles []string
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		found := false
		for _, a := range apps {
			if a.Name != name {
				continue
			}
			found = true
			if a.CustomApp {
				return nil, nil, fmt.Errorf("%s is a custom app, which the rule leaves alone: it can't be rolled back", name)
			}
			title := a.Metadata.Title
			if title == "" {
				title = a.Name
			}
			kept = append(kept, a.Name)
			titles = append(titles, title)
		}
		if !found {
			return nil, nil, fmt.Errorf("there's no app named %q; call list_apps for their names", name)
		}
	}
	return kept, titles, nil
}

// ----- create_checkin ----------------------------------------------------------

// checkInTokens is roughly what one check-in uses: the briefing prompt and
// what the read-only tools return, then a short answer.
const checkInTokens = "20,000"

func (b Booking) createCheckIn() tools.Tool {
	describe := func(ctx context.Context, raw json.RawMessage) (tools.Card, error) {
		var a whenArgs
		if err := strict(raw, &a); err != nil {
			return tools.Card{}, err
		}
		if a.Repeat != "daily" && a.Repeat != "weekly" {
			return tools.Card{}, errors.New("a check-in repeats daily or weekly")
		}
		w, first, err := b.when(ctx, a)
		if err != nil {
			return tools.Card{}, err
		}
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
		return approved{
			Kind: KindCheckIn, When: w, Title: "Check in on the NAS " + w.Phrase(), Detail: detail, Risk: tools.RiskRoutine,
		}.card(), nil
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
			Describe: describe,
			Run: func(ctx context.Context, raw json.RawMessage) (tools.Outcome, error) {
				return b.book(ctx, raw, KindCheckIn)
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
