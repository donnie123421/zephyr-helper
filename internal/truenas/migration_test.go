package truenas

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestLoginResultOK pins the auth-result interpretation across the shapes
// TrueNAS has used. The bug this guards: a bool-only cast treated every
// 25.10.3+ object response (including AUTH_ERR) as success because
// unmarshalling an object into a bool fails silently.
func TestLoginResultOK(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"bare true", `true`, true},
		{"bare false", `false`, false},
		{"object success", `{"response_type":"SUCCESS"}`, true},
		{"object success lowercase", `{"response_type":"success"}`, true},
		{"object auth error", `{"response_type":"AUTH_ERR"}`, false},
		{"object otp required", `{"response_type":"OTP_REQUIRED"}`, false},
		{"object empty", `{}`, false},
		{"null", `null`, false},
		{"garbage", `"nope"`, false},
		{"number", `1`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := loginResultOK(json.RawMessage(tc.raw)); got != tc.want {
				t.Fatalf("loginResultOK(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestMapREST covers the translation rules the helper relies on, including
// the pool.snapshot.query move (zfs.snapshot.query was removed in 25.10+)
// and the nil-filters normalisation (options-without-filters must send `[]`,
// not `null`, as the first positional arg).
func TestMapREST(t *testing.T) {
	cases := []struct {
		name       string
		httpMethod string
		path       string
		body       any
		wantMethod string
		wantParams []any
	}{
		{
			name:       "snapshot ordered (no filters) normalises to empty list",
			httpMethod: "GET",
			path:       "/pool/snapshot?limit=50&order_by=-properties.creation.rawvalue",
			wantMethod: "pool.snapshot.query",
			wantParams: []any{
				[]any{},
				map[string]any{"limit": 50, "order_by": []any{"-properties.creation.rawvalue"}},
			},
		},
		{
			name:       "snapshot bare",
			httpMethod: "GET",
			path:       "/pool/snapshot",
			wantMethod: "pool.snapshot.query",
			wantParams: []any{},
		},
		{
			name:       "snapshot filtered by dataset",
			httpMethod: "GET",
			path:       `/pool/snapshot?limit=50&filters=[["dataset","=","tank"]]`,
			wantMethod: "pool.snapshot.query",
			wantParams: []any{
				[]any{[]any{"dataset", "=", "tank"}},
				map[string]any{"limit": 50},
			},
		},
		{
			name:       "alert.list is a bare method with no params",
			httpMethod: "GET",
			path:       "/alert/list",
			wantMethod: "alert.list",
			wantParams: nil,
		},
		{
			name:       "core.get_jobs sends both filter and options args",
			httpMethod: "GET",
			path:       "/core/get_jobs?limit=100",
			wantMethod: "core.get_jobs",
			wantParams: []any{[]any{}, map[string]any{"limit": 100}},
		},
		{
			name:       "single-record GET adds id filter and get:true",
			httpMethod: "GET",
			path:       "/pool/id/tank",
			wantMethod: "pool.query",
			wantParams: []any{[]any{[]any{"id", "=", "tank"}}, map[string]any{"get": true}},
		},
		{
			name:       "config singleton GET",
			httpMethod: "GET",
			path:       "/system/general",
			wantMethod: "system.general.config",
			wantParams: nil,
		},
		{
			name:       "audit.query forwards body as single positional dict",
			httpMethod: "POST",
			path:       "/audit/query",
			body:       map[string]any{"services": []any{"MIDDLEWARE"}},
			wantMethod: "audit.query",
			wantParams: []any{map[string]any{"services": []any{"MIDDLEWARE"}}},
		},
		{
			name:       "generic POST creates",
			httpMethod: "POST",
			path:       "/pool/dataset",
			body:       map[string]any{"name": "tank/x"},
			wantMethod: "pool.dataset.create",
			wantParams: []any{map[string]any{"name": "tank/x"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := mapREST(tc.httpMethod, tc.path, tc.body)
			if err != nil {
				t.Fatalf("mapREST(%s %s) error: %v", tc.httpMethod, tc.path, err)
			}
			if m.method != tc.wantMethod {
				t.Errorf("method = %q, want %q", m.method, tc.wantMethod)
			}
			if !reflect.DeepEqual(m.params, tc.wantParams) {
				t.Errorf("params = %#v, want %#v", m.params, tc.wantParams)
			}
		})
	}
}
