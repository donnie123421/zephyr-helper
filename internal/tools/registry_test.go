package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestDefinitionsAreSortedAndStable(t *testing.T) {
	r := &Registry{tools: map[string]Tool{}}
	for _, name := range []string{"system_info", "list_pools", "pool_status", "list_alerts", "list_apps"} {
		r.tools[name] = Tool{Def: Definition{Name: name, Parameters: json.RawMessage(`{}`)}}
	}

	names := func() []string {
		var out []string
		for _, def := range r.List(true) {
			out = append(out, def.Name)
		}
		return out
	}

	want := []string{"list_alerts", "list_apps", "list_pools", "pool_status", "system_info"}
	// Map iteration order varies run to run; repeat to catch any reliance on it.
	for i := 0; i < 20; i++ {
		if got := names(); !reflect.DeepEqual(got, want) {
			t.Fatalf("definitions order = %v, want %v", got, want)
		}
	}
}

func TestActionsAreListedOnlyOnRequestAndNeverDispatched(t *testing.T) {
	ran := false
	r := NewWith(
		Tool{Def: Definition{Name: "list_pools", Parameters: json.RawMessage(`{}`)}, Handler: func(context.Context, json.RawMessage) (string, error) {
			return `{}`, nil
		}},
		Tool{Def: Definition{Name: "scrub_pool", Parameters: json.RawMessage(`{}`)}, Action: &Action{
			Describe: func(context.Context, json.RawMessage) (Card, error) { return Card{Title: "Start a scrub"}, nil },
			Run: func(context.Context, json.RawMessage) (Outcome, error) {
				ran = true
				return Outcome{}, nil
			},
		}},
	)
	names := func(defs []Definition) (out []string) {
		for _, d := range defs {
			out = append(out, d.Name)
		}
		return out
	}
	if got := names(r.List(false)); !reflect.DeepEqual(got, []string{"list_pools"}) {
		t.Errorf("List(false) = %v", got)
	}
	if got := names(r.List(true)); !reflect.DeepEqual(got, []string{"list_pools", "scrub_pool"}) {
		t.Errorf("List(true) = %v", got)
	}
	if !r.HasActions() || r.Action("scrub_pool") == nil || r.Action("list_pools") != nil {
		t.Error("actions aren't told apart from reads")
	}
	// Only the approval flow runs an action.
	if result, isErr := r.Dispatch(context.Background(), "scrub_pool", nil); !isErr || ran {
		t.Errorf("Dispatch ran an action: %s", result)
	}
}
