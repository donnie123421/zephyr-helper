package pollers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
)

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		// Private / internal — never alert.
		{"192.168.1.10", false},
		{"10.0.0.5", false},
		{"172.20.30.40", false},
		{"127.0.0.1", false},
		{"::1", false},
		{"fe80::1", false},
		{"100.100.5.1", false}, // Tailscale CGNAT
		// Public — alert candidates.
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"203.0.113.5", true},
		{"2001:4860:4860::8888", true},
		// Edge cases.
		{"203.0.113.5:54321", true},   // host:port form
		{"[2001:db8::1]:443", true},    // bracketed IPv6:port
		{"", false},
		{"not-an-ip", false},
		{"100.63.255.255", true},       // just below CGNAT range
		{"100.128.0.0", true},          // just above CGNAT range
	}
	for _, c := range cases {
		got := isPublicIP(c.addr)
		if got != c.want {
			t.Errorf("isPublicIP(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestIsAuditUnavailable(t *testing.T) {
	// 4xx variants must all be silent — they indicate our request is
	// wrong (endpoint absent, role denied, body shape mismatch) rather
	// than audit being broken.
	silent := []string{
		"truenas 404 Not Found: ...",
		"truenas 405 Method Not Allowed: ...",
		"truenas 401 Unauthorized: ...",
		"truenas 403 Forbidden: ...",
		"truenas 422 Unprocessable Entity: ...",
	}
	for _, msg := range silent {
		if !isAuditUnavailable(errors.New(msg)) {
			t.Errorf("expected %q to be treated as unavailable", msg)
		}
	}
	// 5xx + network failures must surface so consecutive-failure
	// counting can fire the audit-down event.
	loud := []string{
		"truenas 500 Internal Server Error",
		"truenas 503 Service Unavailable",
		"dial tcp: connection refused",
		"context deadline exceeded",
	}
	for _, msg := range loud {
		if isAuditUnavailable(errors.New(msg)) {
			t.Errorf("expected %q to surface, not be swallowed", msg)
		}
	}
}

func TestNewPublicIPLoginEmitsWarning(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewSecurity(nil, store, DefaultSecurityInterval)
	ctx := context.Background()

	now := time.Now().UTC()
	s.handleAuthentication(ctx, map[string]any{}, now, "alice", "203.0.113.5")

	got, err := store.List(ctx, events.ListFilter{Kind: events.KindSecurity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Severity != events.SeverityWarning {
		t.Errorf("severity = %s, want warning", got[0].Severity)
	}
	if got[0].DedupeKey != "security|new-public-ip|203.0.113.5" {
		t.Errorf("dedupe = %q", got[0].DedupeKey)
	}
}

func TestPrivateIPLoginIgnored(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewSecurity(nil, store, DefaultSecurityInterval)
	ctx := context.Background()

	now := time.Now().UTC()
	s.handleAuthentication(ctx, map[string]any{}, now, "alice", "192.168.1.50")

	got, err := store.List(ctx, events.ListFilter{Kind: events.KindSecurity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("LAN sign-in should not emit any event; got %d", len(got))
	}
}

func TestPrivilegedLoginEmitsInfo(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewSecurity(nil, store, DefaultSecurityInterval)
	ctx := context.Background()

	now := time.Now().UTC()
	// Internal LAN root login — only the privileged-login rule should
	// fire, not the new-public-ip rule.
	s.handleAuthentication(ctx, map[string]any{}, now, "root", "192.168.1.50")

	got, err := store.List(ctx, events.ListFilter{Kind: events.KindSecurity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Severity != events.SeverityInfo {
		t.Errorf("severity = %s, want info", got[0].Severity)
	}
	if !strings.Contains(got[0].DedupeKey, "privileged-login|root") {
		t.Errorf("dedupe = %q", got[0].DedupeKey)
	}
}

func TestRootLoginFromPublicIPFiresBothRules(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewSecurity(nil, store, DefaultSecurityInterval)
	ctx := context.Background()

	now := time.Now().UTC()
	s.handleAuthentication(ctx, map[string]any{}, now, "root", "8.8.8.8")

	got, err := store.List(ctx, events.ListFilter{Kind: events.KindSecurity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (new-public-ip + privileged-login)", len(got))
	}
	// Second rule should land at warning — the more severe of the two.
	var sawWarn bool
	for _, e := range got {
		if e.Severity == events.SeverityWarning {
			sawWarn = true
		}
	}
	if !sawWarn {
		t.Error("expected at least one warning-level event for root login from public IP")
	}
}

// methodCallEntry returns an audit row shaped like the real ones from
// /audit/query (event=METHOD_CALL with event_data.method).
func methodCallEntry(method, actor string, params map[string]any) map[string]any {
	return map[string]any{
		"audit_id":          "test-" + method,
		"event":             "METHOD_CALL",
		"message_timestamp": float64(time.Now().Unix()),
		"username":          actor,
		"event_data": map[string]any{
			"method":   method,
			"username": actor,
			"params":   []any{params},
			"name":     params["name"],
		},
	}
}

func TestHandleEntryShareCreate(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewSecurity(nil, store, DefaultSecurityInterval)
	ctx := context.Background()

	for _, method := range []string{
		"sharing.smb.create",
		"sharing.nfs.create",
		"sharing.iscsi.target.create",
	} {
		entry := methodCallEntry(method, "root", map[string]any{
			"name": "share-" + method,
			"path": "/mnt/pool/share",
		})
		s.handleEntry(ctx, entry)
	}

	got, err := store.List(ctx, events.ListFilter{Kind: events.KindSecurity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (one per share method)", len(got))
	}
	for _, e := range got {
		if !strings.HasPrefix(e.DedupeKey, "security|share-create|") {
			t.Errorf("unexpected dedupe key %q", e.DedupeKey)
		}
	}
}

func TestHandleEntryUserCreate(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewSecurity(nil, store, DefaultSecurityInterval)
	ctx := context.Background()

	entry := methodCallEntry("user.create", "root", map[string]any{
		"username":  "alice",
		"full_name": "alice",
	})
	// readTargetUsername reads event_data.username, which methodCallEntry
	// already populates from the actor name. Override so target != actor.
	entry["event_data"].(map[string]any)["username"] = "alice"
	entry["username"] = "root"
	s.handleEntry(ctx, entry)

	got, err := store.List(ctx, events.ListFilter{Kind: events.KindSecurity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if !strings.Contains(got[0].DedupeKey, "user-create|alice") {
		t.Errorf("dedupe = %q", got[0].DedupeKey)
	}
}

func TestHandleEntryPrivilegeGrant(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewSecurity(nil, store, DefaultSecurityInterval)
	ctx := context.Background()

	for _, method := range []string{"privilege.create", "privilege.update"} {
		entry := methodCallEntry(method, "root", map[string]any{
			"target_username": "bob",
		})
		entry["event_data"].(map[string]any)["target_username"] = "bob"
		s.handleEntry(ctx, entry)
	}

	got, err := store.List(ctx, events.ListFilter{Kind: events.KindSecurity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (merge window collapses both)", len(got))
	}
	if got[0].Severity != events.SeverityWarning {
		t.Errorf("severity = %s, want warning", got[0].Severity)
	}
}

func TestHandleEntryIgnoresUnrelatedMethod(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewSecurity(nil, store, DefaultSecurityInterval)
	ctx := context.Background()

	// Helper-traffic chatter that should never produce events.
	for _, method := range []string{
		"audit.query",
		"app.query",
		"pool.query",
		"auth.generate_token",
		"user.update",
	} {
		s.handleEntry(ctx, methodCallEntry(method, "root", map[string]any{}))
	}

	got, err := store.List(ctx, events.ListFilter{Kind: events.KindSecurity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d events for ignored methods, want 0", len(got))
	}
}

func TestHandleEntrySkipsAPIKeyAuthentication(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewSecurity(nil, store, DefaultSecurityInterval)
	ctx := context.Background()

	// Simulates an audit row from the helper polling itself: AUTHENTICATION
	// event, root user (would otherwise fire privileged-login), but auth
	// driven by an API key — must be filtered out.
	entry := map[string]any{
		"audit_id":          "test-helper-poll",
		"event":             "AUTHENTICATION",
		"message_timestamp": float64(time.Now().Unix()),
		"username":          "root",
		"event_data": map[string]any{
			"username": "root",
			"credentials": map[string]any{
				"credentials_type": "API_KEY",
				"credentials_data": map[string]any{
					"api_key": map[string]any{
						"id":   1,
						"name": "Zephyr Helper",
					},
				},
			},
		},
	}
	s.handleEntry(ctx, entry)

	got, err := store.List(ctx, events.ListFilter{Kind: events.KindSecurity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("API key auth should not emit any event; got %d", len(got))
	}
}

func TestIsAPIKeyAuthentication(t *testing.T) {
	cases := []struct {
		name  string
		entry map[string]any
		want  bool
	}{
		{
			name: "explicit API_KEY type",
			entry: map[string]any{
				"event_data": map[string]any{
					"credentials": map[string]any{
						"credentials_type": "API_KEY",
					},
				},
			},
			want: true,
		},
		{
			name: "credentials_data carries api_key block",
			entry: map[string]any{
				"event_data": map[string]any{
					"credentials": map[string]any{
						"credentials_data": map[string]any{
							"api_key": map[string]any{"id": 1, "name": "x"},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "interactive password sign-in",
			entry: map[string]any{
				"event_data": map[string]any{
					"credentials": map[string]any{
						"credentials_type": "LOGIN_PASSWORD",
					},
				},
			},
			want: false,
		},
		{
			name:  "no event_data at all",
			entry: map[string]any{},
			want:  false,
		},
	}
	for _, c := range cases {
		got := isAPIKeyAuthentication(c.entry)
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
