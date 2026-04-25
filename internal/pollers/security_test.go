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

func TestRecordFailedLoginSpike(t *testing.T) {
	store, err := events.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewSecurity(nil, store, DefaultSecurityInterval)
	ctx := context.Background()

	base := time.Now().UTC()
	// Below threshold — no event.
	for i := 0; i < securityFailSpikeThreshold-1; i++ {
		s.recordFailedLogin(ctx, map[string]any{}, base.Add(time.Duration(i)*time.Second), "alice", "8.8.8.8")
	}
	got, err := store.List(ctx, events.ListFilter{Kind: events.KindSecurity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d events below threshold, want 0", len(got))
	}

	// Threshold-th attempt — fires.
	s.recordFailedLogin(ctx, map[string]any{}, base.Add(5*time.Second), "alice", "8.8.8.8")
	got, err = store.List(ctx, events.ListFilter{Kind: events.KindSecurity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events at threshold, want 1", len(got))
	}
	if got[0].Severity != events.SeverityWarning {
		t.Errorf("severity = %s, want warning", got[0].Severity)
	}
	if !strings.Contains(got[0].DedupeKey, "alice|8.8.8.8") {
		t.Errorf("dedupe = %q, want bucket key", got[0].DedupeKey)
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
	s.handleAuthentication(ctx, map[string]any{}, now, "alice", "203.0.113.5", true)

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
	s.handleAuthentication(ctx, map[string]any{}, now, "alice", "192.168.1.50", true)

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
	s.handleAuthentication(ctx, map[string]any{}, now, "root", "192.168.1.50", true)

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
	s.handleAuthentication(ctx, map[string]any{}, now, "root", "8.8.8.8", true)

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
