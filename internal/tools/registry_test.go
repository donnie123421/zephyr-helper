package tools

import (
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
		for _, def := range r.Definitions() {
			out = append(out, def["function"].(Definition).Name)
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
