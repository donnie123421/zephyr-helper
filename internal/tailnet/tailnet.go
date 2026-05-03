// Package tailnet wraps tailscale.com/tsnet so the helper can join
// the user's tailnet without bundling tailscaled. When the operator
// supplies TS_AUTHKEY at install time the helper registers as a
// tailnet device named TS_HOSTNAME (default "zephyr-helper") and
// can read its own resolved MagicDNS hostname / IPs via the local
// API client — exactly the data iOS needs to populate the Add
// Tailscale endpoint sheet.
//
// When TS_AUTHKEY is absent the package is a no-op: Status() returns
// nil and the rest of the helper runs as it always has. This keeps
// existing installs from breaking on upgrade.
package tailnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"tailscale.com/client/tailscale"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
	"tailscale.com/types/key"
)

// Server wraps a *tsnet.Server with the small surface the rest of
// the helper actually needs. It also tolerates being constructed
// with no auth key — in that mode every method is a benign no-op,
// so callers don't have to branch on a nil receiver.
type Server struct {
	srv      *tsnet.Server
	local    *tailscale.LocalClient
	hostname string
	stateDir string
	authKey  string
	log      *slog.Logger

	// Captured the last time we successfully read Status(). Hosting
	// it on the struct lets the /remote-access handler answer fast
	// even if the LocalClient is briefly unavailable (e.g. during a
	// re-handshake).
	lastSelf SelfStatus
}

// SelfStatus is the slim subset of ipnstate.PeerStatus iOS needs.
// We mirror the names the iOS client decoder already expects.
type SelfStatus struct {
	HostName        string   `json:"hostname,omitempty"`
	DNSName         string   `json:"dnsName,omitempty"`         // "truenas-shed.tail-scale.ts.net."
	TailscaleIPs    []string `json:"tailscaleIPs,omitempty"`    // ["100.x.y.z"]
	MagicDNSSuffix  string   `json:"magicDNSSuffix,omitempty"`  // "tail-scale.ts.net"
	BackendState    string   `json:"backendState,omitempty"`    // "Running" / "NeedsLogin" / etc.
}

// PeerInfo carries just enough of an ipnstate.PeerStatus for the
// /remote-access handler to identify the NAS in the tailnet and
// hand iOS a reachable hostname + IP. We deliberately don't ship
// the full PeerStatus over the wire — most of it is debugging
// metadata iOS has no use for.
type PeerInfo struct {
	HostName     string   `json:"hostname,omitempty"`
	DNSName      string   `json:"dnsName,omitempty"`
	TailscaleIPs []string `json:"tailscaleIPs,omitempty"`
	Online       bool     `json:"online,omitempty"`
}

// New builds a Server. Callers must call Start() before Status().
// When authKey is empty the returned Server is inert — every
// method is a no-op and Available() returns false.
func New(authKey, hostname, stateDir string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		authKey:  strings.TrimSpace(authKey),
		hostname: hostname,
		stateDir: stateDir,
		log:      log,
	}
}

// Available reports whether the helper is configured to join the
// tailnet at all. Used by the /remote-access handler to decide
// whether to bother calling Status().
func (s *Server) Available() bool {
	return s != nil && s.authKey != ""
}

// Start brings up the embedded tsnet node. Blocks until the node
// is up or ctx is cancelled. Safe to call when no auth key is
// configured — returns nil immediately.
//
// The first registration call is the slow one (a few hundred ms
// of round-trips to the coordinator) so we run it under the
// supplied context and let the caller decide on a timeout.
func (s *Server) Start(ctx context.Context) error {
	if !s.Available() {
		return nil
	}

	s.srv = &tsnet.Server{
		Hostname: s.hostname,
		Dir:      s.stateDir,
		AuthKey:  s.authKey,
		// Mark the node ephemeral so Tailscale auto-removes it from
		// the admin console ~5 min after the container goes away.
		// The helper has no persistent state directory by default
		// (TS_STATE_DIR=/tmp/tsnet) so every container restart
		// registers a fresh node anyway — without ephemeral the
		// admin console fills up with zephyr-helper-1, zephyr-helper-2,
		// etc. as the user redeploys. Operators who *do* mount a
		// persistent volume keep getting the same identity back
		// because the node key on disk wins; ephemeral only applies
		// to truly-offline nodes.
		Ephemeral: true,
		Logf:      s.tsnetLogf,
		UserLogf:  s.tsnetLogf,
	}

	// Up returns once the node has registered. The local client we
	// stash for later only works after Up() has been called.
	if _, err := s.srv.Up(ctx); err != nil {
		return fmt.Errorf("tsnet up: %w", err)
	}
	lc, err := s.srv.LocalClient()
	if err != nil {
		return fmt.Errorf("tsnet local client: %w", err)
	}
	s.local = lc

	// Prime the cached Self so a /remote-access call right after
	// boot doesn't have to wait for a fresh round-trip.
	if status, err := s.local.Status(ctx); err == nil {
		s.lastSelf = selfFromStatus(status)
	}
	s.log.Info("tsnet: joined tailnet",
		"hostname", s.lastSelf.HostName,
		"dns_name", s.lastSelf.DNSName,
	)
	return nil
}

// Stop tears the embedded node down. Safe to call when never
// started.
func (s *Server) Stop() {
	if s == nil || s.srv == nil {
		return
	}
	_ = s.srv.Close()
}

// Status returns the latest known SelfStatus. Refreshes from the
// local client when ctx allows; falls back to the cached snapshot
// captured at Start() / the previous successful refresh.
//
// Returns (zero, false) when the helper isn't on the tailnet.
func (s *Server) Status(ctx context.Context) (SelfStatus, bool) {
	if !s.Available() || s.local == nil {
		return SelfStatus{}, false
	}
	if status, err := s.local.Status(ctx); err == nil {
		s.lastSelf = selfFromStatus(status)
	}
	return s.lastSelf, true
}

// FindPeerByHostname walks the tailnet peer list looking for the
// node that best matches `hostname`. Used by /remote-access to
// locate the user's NAS — the helper's own Self identity is
// unhelpful as an endpoint because the iPhone needs to reach
// TrueNAS at port 443, which lives on the NAS host, not on the
// helper container.
//
// Match priority (best first):
//  1. Exact case-insensitive match on HostName or DNS label
//  2. Token match — `truenas` finds `truenas-scale` because it
//     appears as a hyphen-separated token. Symmetric: a NAS named
//     `truenas-scale` also finds a peer just named `truenas`.
//
// Returns (zero, false) when no match is found, the helper isn't
// on the tailnet, or hostname is empty.
func (s *Server) FindPeerByHostname(ctx context.Context, hostname string) (PeerInfo, bool) {
	if !s.Available() || s.local == nil {
		return PeerInfo{}, false
	}
	target := strings.ToLower(strings.TrimSpace(hostname))
	if target == "" {
		return PeerInfo{}, false
	}
	status, err := s.local.Status(ctx)
	if err != nil || status == nil {
		return PeerInfo{}, false
	}
	// Two-pass scan so an exact match always wins over a token match
	// even when they appear later in the peer list.
	if peer := scanPeers(status.Peer, target, peerMatchesExact); peer != nil {
		return peerInfoFrom(peer), true
	}
	if peer := scanPeers(status.Peer, target, peerMatchesToken); peer != nil {
		return peerInfoFrom(peer), true
	}
	return PeerInfo{}, false
}

// ListOnlinePeers returns every reachable tailnet peer. Used by
// /remote-access to give iOS a picker when auto-match is wrong or
// ambiguous (multiple TrueNAS-like devices on the same tailnet).
// Excludes the helper's own Self entry — that's never a useful
// endpoint for the iPhone to talk to.
func (s *Server) ListOnlinePeers(ctx context.Context) []PeerInfo {
	if !s.Available() || s.local == nil {
		return nil
	}
	status, err := s.local.Status(ctx)
	if err != nil || status == nil {
		return nil
	}
	var out []PeerInfo
	for _, peer := range status.Peer {
		if peer == nil || !peer.Online {
			continue
		}
		out = append(out, peerInfoFrom(peer))
	}
	return out
}

func scanPeers(
	peers map[key.NodePublic]*ipnstate.PeerStatus,
	target string,
	matcher func(*ipnstate.PeerStatus, string) bool,
) *ipnstate.PeerStatus {
	for _, peer := range peers {
		if peer == nil {
			continue
		}
		if matcher(peer, target) {
			return peer
		}
	}
	return nil
}

func peerInfoFrom(peer *ipnstate.PeerStatus) PeerInfo {
	info := PeerInfo{
		HostName: peer.HostName,
		DNSName:  strings.TrimSuffix(peer.DNSName, "."),
		Online:   peer.Online,
	}
	for _, ip := range peer.TailscaleIPs {
		info.TailscaleIPs = append(info.TailscaleIPs, ip.String())
	}
	return info
}

// peerMatchesExact wins over peerMatchesToken — exact hostname
// equality is always more confident than a token-level match.
func peerMatchesExact(peer *ipnstate.PeerStatus, target string) bool {
	if peer == nil {
		return false
	}
	if strings.EqualFold(peer.HostName, target) {
		return true
	}
	return strings.EqualFold(dnsLabel(peer.DNSName), target)
}

// peerMatchesToken catches the common case where a Tailscale device
// is named differently from its OS hostname — e.g. OS reports
// `truenas` but the tailnet device is `truenas-scale` (because the
// user customised it or Tailscale auto-uniqued a duplicate).
//
// We tokenize both sides on hyphen / underscore / dot and check
// for any token-equal pair. Symmetry handles both directions:
// `truenas` matches `truenas-scale` AND a NAS reporting
// `truenas-scale` matches a peer simply named `truenas`.
func peerMatchesToken(peer *ipnstate.PeerStatus, target string) bool {
	if peer == nil {
		return false
	}
	targetTokens := tokenize(target)
	if len(targetTokens) == 0 {
		return false
	}
	candidates := []string{peer.HostName, dnsLabel(peer.DNSName)}
	for _, c := range candidates {
		ct := tokenize(strings.ToLower(c))
		for _, t := range targetTokens {
			for _, x := range ct {
				if t == x && t != "" {
					return true
				}
			}
		}
	}
	return false
}

func tokenize(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == ' '
	})
}

func dnsLabel(dns string) string {
	if dns == "" {
		return ""
	}
	label := dns
	if dot := strings.IndexByte(label, '.'); dot > 0 {
		label = label[:dot]
	}
	return label
}

func selfFromStatus(status *ipnstate.Status) SelfStatus {
	if status == nil || status.Self == nil {
		return SelfStatus{}
	}
	self := status.Self
	out := SelfStatus{
		HostName:       self.HostName,
		DNSName:        self.DNSName,
		MagicDNSSuffix: status.MagicDNSSuffix,
		BackendState:   status.BackendState,
	}
	for _, ip := range self.TailscaleIPs {
		out.TailscaleIPs = append(out.TailscaleIPs, ip.String())
	}
	return out
}

func (s *Server) tsnetLogf(format string, args ...any) {
	// tsnet emits a lot of debug chatter we don't want in our
	// JSON logs. Compress to a single Debug line so we can still
	// turn it on if something breaks.
	s.log.Debug("tsnet: "+strings.TrimSpace(fmt.Sprintf(format, args...)))
}

// Sentinel for callers that want to assert tsnet was unconfigured
// rather than failed.
var ErrUnconfigured = errors.New("tailnet: TS_AUTHKEY not set")

// _ keeps `time` imported for future use (rejoin backoff). Cheaper
// than importing/removing on every change.
var _ = time.Second
