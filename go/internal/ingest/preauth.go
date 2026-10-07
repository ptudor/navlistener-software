package ingest

import (
	"net"
	"sync"
	"time"
	"unicode/utf8"
)

// Pre-authentication bounds. A connection that has not yet completed TLS, the
// GNF1 magic and the HELLO holds one of the pre-auth slots — half the fleet
// budget, at least minPreAuthSlots; a separate pool, so an unauthenticated
// connection never takes an authenticated feeder's MaxConns slot — for at most
// preAuthDeadline. One remote address may have at most maxPreAuthPerAddress of
// them in flight and may open new ones at preAuthRate per second with a burst
// of preAuthBurst; beyond that its connections are closed at accept, before
// any TLS work. The fleet slot is taken only once the HELLO has authenticated.
// mTLS does not help here — a certificate is examined only inside the
// handshake an idle attacker never starts — which is why the accept path, not
// the TLS layer, carries these bounds.
//
// The address limits leave room for legitimate bursts: a site NAT whose
// feeders all reconnect after a collector restart, or a C feeder replaying a
// run of recovered spool files on consecutive connections (one at a time, but
// quickly). The pre-auth deadline is generous for a ~150-byte HELLO plus an
// EVIDENCE frame even from an embedded feeder's TLS handshake.
const (
	preAuthDeadline      = 10 * time.Second
	minPreAuthSlots      = 64
	maxPreAuthPerAddress = 8
	preAuthBurst         = 64
	preAuthRate          = 32 // accepts per second per address, sustained
	preAuthLogLines      = 5  // pre-auth warnings per address per preAuthLogWindow
	preAuthLogWindow     = time.Minute
	loggedHelloFieldMax  = 64 // bytes of peer-chosen HELLO text one log line carries
	maxTrackedAddresses  = 4096
)

// preAuthSlots sizes the pre-auth pool for a fleet budget.
func preAuthSlots(maxConns int) int {
	return max(minPreAuthSlots, maxConns/2)
}

// addressGuard bounds what one remote address may do before authenticating:
// how many of its connections may be in the pre-auth phase at once, how fast
// it may open new ones, and how many pre-auth warnings it may put in the log.
type addressGuard struct {
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]*addressState
}

type addressState struct {
	inflight  int
	tokens    float64 // accept bucket, preAuthBurst at most
	logTokens float64 // log bucket, preAuthLogLines at most
	refilled  time.Time
}

func newAddressGuard(now func() time.Time) *addressGuard {
	return &addressGuard{now: now, entries: make(map[string]*addressState)}
}

// admit reserves a pre-auth place for one connection from address. It returns
// the function that gives the place back, or the refusal reason.
func (g *addressGuard) admit(address string) (release func(), reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state(address, g.now())
	switch {
	case s.inflight >= maxPreAuthPerAddress:
		return nil, "address_inflight"
	case s.tokens < 1:
		return nil, "address_rate"
	}
	s.tokens--
	s.inflight++
	return func() {
		g.mu.Lock()
		s.inflight--
		g.mu.Unlock()
	}, ""
}

// allowLog reports whether a pre-auth warning about address may be logged now.
func (g *addressGuard) allowLog(address string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state(address, g.now())
	if s.logTokens < 1 {
		return false
	}
	s.logTokens--
	return true
}

// state returns address's entry with its buckets refilled to now, creating it
// (after pruning idle entries when the table is full) if absent.
func (g *addressGuard) state(address string, now time.Time) *addressState {
	s := g.entries[address]
	if s == nil {
		if len(g.entries) >= maxTrackedAddresses {
			g.pruneLocked(now)
		}
		s = &addressState{tokens: preAuthBurst, logTokens: preAuthLogLines, refilled: now}
		g.entries[address] = s
		return s
	}
	if elapsed := now.Sub(s.refilled).Seconds(); elapsed > 0 {
		s.tokens = min(preAuthBurst, s.tokens+elapsed*preAuthRate)
		s.logTokens = min(preAuthLogLines, s.logTokens+elapsed*preAuthLogLines/preAuthLogWindow.Seconds())
		s.refilled = now
	}
	return s
}

// pruneLocked forgets addresses with nothing in flight: first those not seen
// for a log window, then — if the table is still full — every idle one. An
// address with a connection in flight is never forgotten, and there can be at
// most a pre-auth pool's worth of those.
func (g *addressGuard) pruneLocked(now time.Time) {
	for address, s := range g.entries {
		if s.inflight == 0 && now.Sub(s.refilled) >= preAuthLogWindow {
			delete(g.entries, address)
		}
	}
	if len(g.entries) < maxTrackedAddresses {
		return
	}
	for address, s := range g.entries {
		if s.inflight == 0 {
			delete(g.entries, address)
		}
	}
}

// preAuthWarn logs a warning about an unauthenticated connection, at most
// preAuthLogLines per address per preAuthLogWindow: every refused HELLO
// carries peer-chosen text, and an address that fails thousands of handshakes
// must not write thousands of lines. The metrics beside these lines count
// every event regardless.
func (p *PushServer) preAuthWarn(remote, msg string, attrs ...any) {
	if !p.addresses.allowLog(remoteHost(remote)) {
		return
	}
	p.log.Warn(msg, append([]any{"remote", remote}, attrs...)...)
}

// clipHelloField bounds peer-chosen HELLO text in a log line. Legitimate
// values are ValidObserverID-shaped and far shorter; anything longer is noise
// or an attempt to plant text in the operator's log.
func clipHelloField(s string) string {
	if len(s) <= loggedHelloFieldMax {
		return s
	}
	cut := loggedHelloFieldMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// remoteHost is the address part of a remote "host:port" (the whole string
// when it carries no port, as an in-memory pipe's does).
func remoteHost(remote string) string {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}
