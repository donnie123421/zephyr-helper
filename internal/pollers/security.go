package pollers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/events"
	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// DefaultSecurityInterval is the cadence for /audit/query. 2 min keeps
// the iOS feed responsive while staying gentle on TrueNAS.
const DefaultSecurityInterval = 2 * time.Minute

const (
	// New public IPs only re-notify after a long quiet period — same IP
	// returning the next day is not news.
	securityIPMergeWindow = 30 * 24 * time.Hour
	// Repeated root logins, share creates, etc. collapse for an hour so
	// admin sessions don't spam the feed.
	securityRoutineMergeWindow = time.Hour
	// Audit-down events are urgent; merge tightly so a flapping audit
	// service still produces visible churn but doesn't quadruple-fire.
	securityAuditDownMergeWindow = 5 * time.Minute
)

// privilegedUsers are the canonical "everything-as-root" account names
// in TrueNAS Scale. Successful logins for these always emit info-level
// events because they're worth seeing in a feed even when expected.
var privilegedUsers = map[string]struct{}{
	"root":          {},
	"admin":         {},
	"truenas_admin": {},
}

// auditDownFailureThreshold is how many consecutive poll failures must
// occur before we emit a critical "audit unreachable" event. A single
// transient blip shouldn't paint the feed red. With a 2-min poll
// cadence this is roughly six minutes of sustained breakage.
const auditDownFailureThreshold = 3

// Security polls /audit/query every interval, applies deterministic
// rules, and ingests one events row per detection.
//
// State held between ticks:
//   - seenAuditIDs: bounded to the most recent poll's payload so the
//     same row isn't dispatched twice across overlapping polls.
//   - consecutiveFailures: gates the audit-down event behind the
//     auditDownFailureThreshold so transient errors stay silent.
type Security struct {
	tn       *truenas.Client
	store    *events.Store
	interval time.Duration

	seenAuditIDs        map[string]bool
	consecutiveFailures int
}

// NewSecurity builds a poller. Use DefaultSecurityInterval for the
// plan-recommended cadence.
func NewSecurity(tn *truenas.Client, store *events.Store, interval time.Duration) *Security {
	return &Security{
		tn:           tn,
		store:        store,
		interval:     interval,
		seenAuditIDs: make(map[string]bool),
	}
}

// Run blocks until ctx is cancelled. First poll fires immediately so
// the feed is populated when the iOS client connects.
func (s *Security) Run(ctx context.Context) {
	s.poll(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.poll(ctx)
		}
	}
}

func (s *Security) poll(ctx context.Context) {
	// /audit/query is a POST endpoint that takes a query-filter array
	// and an options map. limit=200 covers ~hours of activity on a
	// normal home NAS even when the audit log is chatty (smb mounts,
	// snapshot tasks, etc.).
	body := map[string]any{
		"query-filters": []any{},
		"query-options": map[string]any{
			"limit": 200,
			"order_by": []string{"-message_timestamp"},
		},
	}
	raw, err := s.tn.PostRaw(ctx, "/audit/query", body)
	if err != nil {
		// Any 4xx means our REQUEST is wrong — endpoint absent
		// (pre-25.04 servers, TrueNAS 26 with REST removed), method
		// not allowed, role denied, or audit not enabled. None of
		// those are "audit is broken"; fail silently and reset the
		// failure counter so we don't fire spurious recovery events.
		if isAuditUnavailable(err) {
			s.consecutiveFailures = 0
			return
		}
		// 5xx or network error: audit might really be down. Only
		// surface the critical event after sustained failure to ride
		// out transient hiccups.
		s.consecutiveFailures++
		slog.Warn("security: audit poll", "err", err, "consecutive_failures", s.consecutiveFailures)
		if s.consecutiveFailures >= auditDownFailureThreshold {
			s.emitAuditDown(ctx, err.Error())
		}
		return
	}
	s.consecutiveFailures = 0

	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		slog.Warn("security: decode", "err", err)
		return
	}

	// /audit/query returns newest-first; iterate in reverse so events
	// are dispatched in chronological order. Keeps room for any future
	// rule that depends on row ordering without restructuring the loop.
	current := make(map[string]bool, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		id, _ := entry["audit_id"].(string)
		if id != "" {
			current[id] = true
			if s.seenAuditIDs[id] {
				continue
			}
		}
		s.handleEntry(ctx, entry)
	}
	s.seenAuditIDs = current
}

// handleEntry dispatches one audit row to the rule(s) it matches.
// Entries can match more than one rule (e.g. a successful root login
// from a new public IP fires both rules); handlers are independent and
// safe to call together because the events store dedupes per-rule.
//
// Failed authentications aren't dispatched here: TrueNAS doesn't write
// failed sign-ins to /audit/query, so any spike rule would have no
// signal to read. If we ever pivot to syslog we can reintroduce that
// branch.
func (s *Security) handleEntry(ctx context.Context, entry map[string]any) {
	occurred := auditTimestamp(entry)
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}
	username := readUsername(entry)
	address := readAddress(entry)
	event := strings.ToUpper(strings.TrimSpace(asString(entry["event"])))

	switch event {
	case "AUTHENTICATION", "LOGIN":
		// Programmatic API key auths (this helper, scripts, other tools)
		// are not interactive sign-ins. Skip them so the helper's own
		// poll-driven authentications don't loop back into the feed.
		if isAPIKeyAuthentication(entry) {
			return
		}
		s.handleAuthentication(ctx, entry, occurred, username, address)
	case "METHOD_CALL":
		// Real schema: dispatch on event_data.method, not on the
		// top-level event field. The actual TrueNAS method names are
		// dotted (sharing.smb.create, user.create, privilege.create).
		method := readMethod(entry)
		switch {
		case isShareCreateMethod(method):
			s.emitShareCreated(ctx, entry, occurred, username)
		case isUserCreateMethod(method):
			s.emitUserCreated(ctx, entry, occurred, username)
		case isPrivilegeGrantMethod(method):
			s.emitPrivilegeGranted(ctx, entry, occurred, username)
		}
	}
}

func (s *Security) handleAuthentication(
	ctx context.Context,
	entry map[string]any,
	occurred time.Time,
	username, address string,
) {
	// One entry can fire more than one rule (e.g. root login from a new
	// public IP).
	if address != "" && isPublicIP(address) {
		s.emitNewPublicIPLogin(ctx, entry, occurred, username, address)
	}
	if _, privileged := privilegedUsers[strings.ToLower(username)]; privileged {
		s.emitPrivilegedLogin(ctx, entry, occurred, username, address)
	}
}

func (s *Security) emitNewPublicIPLogin(
	ctx context.Context,
	entry map[string]any,
	occurred time.Time,
	username, address string,
) {
	body, _ := json.Marshal(map[string]any{
		"username": username,
		"address":  address,
		"sample":   entry,
	})
	ev := events.Event{
		OccurredAt: occurred,
		Kind:       events.KindSecurity,
		Severity:   events.SeverityWarning,
		Title:      fmt.Sprintf("New sign-in from %s", address),
		Summary: fmt.Sprintf(
			"%s signed in from public IP %s — first time this IP has been seen recently.",
			displayUser(username), address,
		),
		Body:      body,
		DedupeKey: fmt.Sprintf("security|new-public-ip|%s", address),
	}
	// 30-day merge: the same IP returning next week is not news, but
	// after a month of silence it's worth re-surfacing.
	if _, err := s.store.Ingest(ctx, ev, securityIPMergeWindow); err != nil {
		slog.Warn("security: new-ip ingest", "err", err)
	}
}

func (s *Security) emitPrivilegedLogin(
	ctx context.Context,
	entry map[string]any,
	occurred time.Time,
	username, address string,
) {
	body, _ := json.Marshal(map[string]any{
		"username": username,
		"address":  address,
		"sample":   entry,
	})
	from := address
	if from == "" {
		from = "unknown source"
	}
	ev := events.Event{
		OccurredAt: occurred,
		Kind:       events.KindSecurity,
		Severity:   events.SeverityInfo,
		Title:      fmt.Sprintf("%s signed in", username),
		Summary:    fmt.Sprintf("Privileged account %q signed in from %s.", username, from),
		Body:       body,
		DedupeKey:  fmt.Sprintf("security|privileged-login|%s|%s", strings.ToLower(username), address),
	}
	if _, err := s.store.Ingest(ctx, ev, securityRoutineMergeWindow); err != nil {
		slog.Warn("security: privileged-login ingest", "err", err)
	}
}

func (s *Security) emitShareCreated(
	ctx context.Context,
	entry map[string]any,
	occurred time.Time,
	username string,
) {
	target := readShareName(entry)
	body, _ := json.Marshal(entry)
	ev := events.Event{
		OccurredAt: occurred,
		Kind:       events.KindSecurity,
		Severity:   events.SeverityInfo,
		Title:      fmt.Sprintf("Share created: %s", target),
		Summary:    fmt.Sprintf("%s created share %q.", displayUser(username), target),
		Body:       body,
		DedupeKey:  fmt.Sprintf("security|share-create|%s", target),
	}
	if _, err := s.store.Ingest(ctx, ev, securityRoutineMergeWindow); err != nil {
		slog.Warn("security: share-create ingest", "err", err)
	}
}

func (s *Security) emitUserCreated(
	ctx context.Context,
	entry map[string]any,
	occurred time.Time,
	actor string,
) {
	target := readTargetUsername(entry)
	body, _ := json.Marshal(entry)
	ev := events.Event{
		OccurredAt: occurred,
		Kind:       events.KindSecurity,
		Severity:   events.SeverityInfo,
		Title:      fmt.Sprintf("User created: %s", target),
		Summary:    fmt.Sprintf("%s created a new user %q.", displayUser(actor), target),
		Body:       body,
		DedupeKey:  fmt.Sprintf("security|user-create|%s", target),
	}
	if _, err := s.store.Ingest(ctx, ev, securityRoutineMergeWindow); err != nil {
		slog.Warn("security: user-create ingest", "err", err)
	}
}

func (s *Security) emitPrivilegeGranted(
	ctx context.Context,
	entry map[string]any,
	occurred time.Time,
	actor string,
) {
	target := readTargetUsername(entry)
	body, _ := json.Marshal(entry)
	ev := events.Event{
		OccurredAt: occurred,
		Kind:       events.KindSecurity,
		Severity:   events.SeverityWarning,
		Title:      fmt.Sprintf("Privileges granted to %s", target),
		Summary:    fmt.Sprintf("%s granted elevated privileges to %q.", displayUser(actor), target),
		Body:       body,
		DedupeKey:  fmt.Sprintf("security|privilege-grant|%s", target),
	}
	if _, err := s.store.Ingest(ctx, ev, securityRoutineMergeWindow); err != nil {
		slog.Warn("security: privilege-grant ingest", "err", err)
	}
}

func (s *Security) emitAuditDown(ctx context.Context, reason string) {
	body, _ := json.Marshal(map[string]any{"reason": reason})
	ev := events.Event{
		OccurredAt: time.Now().UTC(),
		Kind:       events.KindSecurity,
		Severity:   events.SeverityCritical,
		Title:      "Audit log unreachable",
		Summary:    "Zephyr can't reach TrueNAS's audit log — security monitoring is paused until it recovers.",
		Body:       body,
		DedupeKey:  "security|audit-down",
	}
	if _, err := s.store.Ingest(ctx, ev, securityAuditDownMergeWindow); err != nil {
		slog.Warn("security: audit-down ingest", "err", err)
	}
}

// MARK: - Audit field readers

// auditTimestamp pulls the time an audit row occurred, walking the
// shapes TrueNAS actually returns: `message_timestamp` (unix seconds),
// `time` ($date millis), or RFC3339 strings.
func auditTimestamp(entry map[string]any) time.Time {
	if t := parseTrueNASTime(entry["message_timestamp"]); !t.IsZero() {
		return t
	}
	if v, ok := entry["message_timestamp"].(float64); ok {
		return time.Unix(int64(v), 0).UTC()
	}
	if t := parseTrueNASTime(entry["time"]); !t.IsZero() {
		return t
	}
	if t := parseTrueNASTime(entry["@timestamp"]); !t.IsZero() {
		return t
	}
	return time.Time{}
}

func readUsername(entry map[string]any) string {
	if u := asString(entry["username"]); u != "" {
		return u
	}
	if data, ok := entry["event_data"].(map[string]any); ok {
		if u := asString(data["username"]); u != "" {
			return u
		}
		if creds, ok := data["credentials"].(map[string]any); ok {
			if cd, ok := creds["credentials_data"].(map[string]any); ok {
				if u := asString(cd["username"]); u != "" {
					return u
				}
			}
		}
	}
	return ""
}

func readAddress(entry map[string]any) string {
	for _, key := range []string{"address", "remote_address", "client_address"} {
		if v := asString(entry[key]); v != "" {
			return v
		}
	}
	if data, ok := entry["event_data"].(map[string]any); ok {
		for _, key := range []string{"address", "remote_address", "client_address"} {
			if v := asString(data[key]); v != "" {
				return v
			}
		}
	}
	return ""
}

func readShareName(entry map[string]any) string {
	if data, ok := entry["event_data"].(map[string]any); ok {
		if v := asString(data["name"]); v != "" {
			return v
		}
		if v := asString(data["share"]); v != "" {
			return v
		}
		if v := asString(data["path"]); v != "" {
			return v
		}
	}
	return "(unknown)"
}

func readTargetUsername(entry map[string]any) string {
	if data, ok := entry["event_data"].(map[string]any); ok {
		if v := asString(data["target_username"]); v != "" {
			return v
		}
		if v := asString(data["username"]); v != "" {
			return v
		}
	}
	return "(unknown)"
}

// MARK: - Method dispatch helpers

// readMethod returns the JSON-RPC method name carried by a METHOD_CALL
// audit row (e.g. "sharing.smb.create"). Empty when the entry isn't a
// METHOD_CALL or the field is missing.
func readMethod(entry map[string]any) string {
	if data, ok := entry["event_data"].(map[string]any); ok {
		return asString(data["method"])
	}
	return ""
}

// isAPIKeyAuthentication reports whether an AUTHENTICATION row was
// driven by an API key rather than an interactive sign-in. Skipping
// these prevents the helper's own /audit/query poll from generating an
// authentication event that the next poll then re-reads — and also
// keeps unrelated tool/script auths out of the feed, since "another
// program logged in" is not what users want to be notified about.
func isAPIKeyAuthentication(entry map[string]any) bool {
	data, ok := entry["event_data"].(map[string]any)
	if !ok {
		return false
	}
	creds, ok := data["credentials"].(map[string]any)
	if !ok {
		return false
	}
	if t := strings.ToUpper(asString(creds["credentials_type"])); t == "API_KEY" {
		return true
	}
	// Some payloads put the discriminator one level deeper inside
	// credentials_data, where the key shape is { "api_key": {...} }.
	if cd, ok := creds["credentials_data"].(map[string]any); ok {
		if _, hasKey := cd["api_key"]; hasKey {
			return true
		}
	}
	return false
}

// isShareCreateMethod matches the methods TrueNAS emits when an admin
// creates a share. iSCSI targets are functionally a share for our
// threat model — they expose data over the network — so they belong in
// the same rule.
func isShareCreateMethod(method string) bool {
	switch method {
	case "sharing.smb.create",
		"sharing.nfs.create",
		"sharing.iscsi.target.create":
		return true
	}
	return false
}

func isUserCreateMethod(method string) bool {
	return method == "user.create"
}

// isPrivilegeGrantMethod stays narrow on purpose: user.update /
// group.update fire on too many benign edits to use without inspecting
// params. If we miss a real escalation here we can broaden later.
func isPrivilegeGrantMethod(method string) bool {
	switch method {
	case "privilege.create", "privilege.update":
		return true
	}
	return false
}

// MARK: - IP classification

// isPublicIP reports whether `addr` should count as "outside the
// trusted perimeter." LAN clients (RFC1918), loopback, and Tailscale's
// CGNAT range (100.64/10) all return false — they're either inside the
// home or on a VPN bridged into it. Anything else is public.
func isPublicIP(addr string) bool {
	// Audit entries occasionally include the source port. SplitHostPort
	// is the canonical way to peel that off without eating the last
	// hextet of a bare IPv6 address.
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	} else {
		addr = strings.TrimPrefix(addr, "[")
		addr = strings.TrimSuffix(addr, "]")
	}

	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	// Tailscale CGNAT range — net.IP.IsPrivate doesn't cover this.
	if cgnat := mustCIDR("100.64.0.0/10"); cgnat.Contains(ip) {
		return false
	}
	return true
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("pollers: bad CIDR literal " + s + ": " + err.Error())
	}
	return n
}

// isAuditUnavailable returns true when the error from /audit/query
// indicates the endpoint isn't usable from our request shape, which is
// a "client/server mismatch" rather than "audit is broken." Covers:
//
//   - 404 / Not Found: pre-25.04 server with no audit endpoint, or
//     TrueNAS 26 where REST has been removed entirely
//   - 405 Method Not Allowed: server expects a different HTTP verb
//   - 401 / 403: API key lacks the audit role (silent rather than
//     painting the feed red on every poll)
//   - 422: request body shape doesn't match what the server expects
//
// 5xx errors and network failures are NOT swallowed here — those go
// through the consecutive-failure counter and surface as audit-down
// once they persist.
func isAuditUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pat := range []string{
		"truenas 401", "truenas 403", "truenas 404",
		"truenas 405", "truenas 422",
		"Not Found", "Method Not Allowed", "Unprocessable",
	} {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}

// MARK: - Misc helpers

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func displayUser(u string) string {
	if u == "" {
		return "Someone"
	}
	return u
}
