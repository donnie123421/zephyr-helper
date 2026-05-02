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
		Hostname:     s.hostname,
		Dir:          s.stateDir,
		AuthKey:      s.authKey,
		Ephemeral:    false,
		Logf:         s.tsnetLogf,
		UserLogf:     s.tsnetLogf,
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
