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
	// Failed-login spike rule: ≥N failures from one user|ip in window
	// rolls up into one row. Threshold/window kept conservative — too
	// noisy and the feature gets muted (see noisy-baseline tradeoff).
	securityFailSpikeWindow      = 10 * time.Minute
	securityFailSpikeThreshold   = 5
	securityFailSpikeMergeWindow = 30 * time.Minute
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
//   - seenAuditIDs: bounded to the most recent poll's payload so failed
//     logins aren't double-counted.
//   - failedLogins: rolling window per user|ip used for the spike rule.
//   - consecutiveFailures: gates the audit-down event behind the
//     auditDownFailureThreshold so transient errors stay silent.
type Security struct {
	tn       *truenas.Client
	store    *events.Store
	interval time.Duration

	seenAuditIDs        map[string]bool
	failedLogins        map[string][]time.Time
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
		failedLogins: make(map[string][]time.Time),
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

	// Process oldest-first so the spike-rule rolling window accumulates
	// in chronological order. /audit/query returns newest-first.
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
	s.pruneFailedLogins(time.Now().UTC())
}

// handleEntry dispatches one audit row to the rule(s) it matches.
// Entries can match more than one rule (e.g. a successful root login
// from a new public IP fires both rules); handlers are independent and
// safe to call together because the events store dedupes per-rule.
func (s *Security) handleEntry(ctx context.Context, entry map[string]any) {
	occurred := auditTimestamp(entry)
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}
	username := readUsername(entry)
	address := readAddress(entry)
	success := readSuccess(entry)
	event := strings.ToUpper(strings.TrimSpace(asString(entry["event"])))

	switch {
	case isAuthenticationEvent(event):
		s.handleAuthentication(ctx, entry, occurred, username, address, success)
	case isShareCreateEvent(event):
		s.emitShareCreated(ctx, entry, occurred, username)
	case isUserCreateEvent(event):
		s.emitUserCreated(ctx, entry, occurred, username)
	case isPrivilegeGrantEvent(event):
		s.emitPrivilegeGranted(ctx, entry, occurred, username)
	}
}

func (s *Security) handleAuthentication(
	ctx context.Context,
	entry map[string]any,
	occurred time.Time,
	username, address string,
	success bool,
) {
	if !success {
		s.recordFailedLogin(ctx, entry, occurred, username, address)
		return
	}

	// Successful login. Check rules in order; one entry can fire more
	// than one (e.g. root login from a new public IP).
	if address != "" && isPublicIP(address) {
		s.emitNewPublicIPLogin(ctx, entry, occurred, username, address)
	}
	if _, privileged := privilegedUsers[strings.ToLower(username)]; privileged {
		s.emitPrivilegedLogin(ctx, entry, occurred, username, address)
	}
}

// recordFailedLogin appends to the rolling window and emits a spike
// event when the threshold is crossed. After emission the bucket is
// reset so a sustained attack produces one event per merge window
// rather than one per failed attempt.
func (s *Security) recordFailedLogin(
	ctx context.Context,
	entry map[string]any,
	occurred time.Time,
	username, address string,
) {
	bucket := username + "|" + address
	if username == "" && address == "" {
		bucket = "unknown"
	}
	cutoff := occurred.Add(-securityFailSpikeWindow)
	window := s.failedLogins[bucket]
	// Drop expired timestamps before append.
	pruned := window[:0]
	for _, t := range window {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	pruned = append(pruned, occurred)
	s.failedLogins[bucket] = pruned

	if len(pruned) < securityFailSpikeThreshold {
		return
	}

	body, _ := json.Marshal(map[string]any{
		"username":   username,
		"address":    address,
		"count":      len(pruned),
		"window_min": int(securityFailSpikeWindow / time.Minute),
		"sample":     entry,
	})

	title := fmt.Sprintf("%d failed sign-ins from %s", len(pruned), authorOrUnknown(username, address))
	summary := fmt.Sprintf(
		"%d failed sign-in attempts from %s in the last %d minutes — possible password attack.",
		len(pruned), authorOrUnknown(username, address), int(securityFailSpikeWindow/time.Minute),
	)

	ev := events.Event{
		OccurredAt: occurred,
		Kind:       events.KindSecurity,
		Severity:   events.SeverityWarning,
		Title:      title,
		Summary:    summary,
		Body:       body,
		DedupeKey:  fmt.Sprintf("security|failed-login-spike|%s", bucket),
	}
	if _, err := s.store.Ingest(ctx, ev, securityFailSpikeMergeWindow); err != nil {
		slog.Warn("security: spike ingest", "err", err)
		return
	}
	// Reset bucket so the next emission requires a fresh threshold-worth
	// of failures rather than tripping on every subsequent attempt.
	s.failedLogins[bucket] = nil
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

func (s *Security) pruneFailedLogins(now time.Time) {
	cutoff := now.Add(-securityFailSpikeWindow)
	for k, ts := range s.failedLogins {
		pruned := ts[:0]
		for _, t := range ts {
			if t.After(cutoff) {
				pruned = append(pruned, t)
			}
		}
		if len(pruned) == 0 {
			delete(s.failedLogins, k)
		} else {
			s.failedLogins[k] = pruned
		}
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

func readSuccess(entry map[string]any) bool {
	if v, ok := entry["success"].(bool); ok {
		return v
	}
	if data, ok := entry["event_data"].(map[string]any); ok {
		if v, ok := data["success"].(bool); ok {
			return v
		}
		// AUTHENTICATION events sometimes encode failure as
		// event_data.error_msg or event_data.result == "FAIL".
		if asString(data["error_msg"]) != "" {
			return false
		}
		if strings.EqualFold(asString(data["result"]), "FAIL") {
			return false
		}
	}
	return true
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

// MARK: - Event-name predicates

func isAuthenticationEvent(event string) bool {
	return event == "AUTHENTICATION" || event == "LOGIN"
}

func isShareCreateEvent(event string) bool {
	switch event {
	case "SHARE_CREATE", "EXPORT_CREATE", "SMB_SHARE_CREATE", "NFS_SHARE_CREATE":
		return true
	}
	return false
}

func isUserCreateEvent(event string) bool {
	return event == "USER_CREATE" || event == "ACCOUNT_CREATE"
}

func isPrivilegeGrantEvent(event string) bool {
	switch event {
	case "PRIVILEGE_GRANT", "ROLE_GRANT", "GROUP_MEMBERSHIP_ADD":
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

func authorOrUnknown(user, addr string) string {
	switch {
	case user != "" && addr != "":
		return user + "@" + addr
	case user != "":
		return user
	case addr != "":
		return addr
	}
	return "an unknown source"
}
