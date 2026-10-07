// Package controlauth observes refused credentials on the control endpoints
// the read listener also serves: station snapshots and update requests. Both
// are reachable wherever the read API is and are protected by a bearer digest
// alone, so every refusal must leave a trace an operator can alert on, without
// a line per attempt letting a guessing client flood the log.
package controlauth

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// FailuresTotal counts control requests refused for a missing, malformed or
// wrong credential, by endpoint. It moves for every refusal; the log does not.
var FailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "navlistener_control_auth_failures_total",
	Help: "Control requests refused for a missing, malformed or wrong credential, by endpoint (station-snapshot, updates).",
}, []string{"endpoint"})

const (
	// WarnEvery bounds the warning log to one line per remote address and
	// endpoint in each interval.
	WarnEvery = time.Minute
	// maxRemotes bounds the addresses remembered between sweeps. A flood from
	// more addresses than this logs more often, never less: the bound protects
	// memory, not the attacker.
	maxRemotes = 1024
)

// Limiter rate-limits the refusal warning per remote address and endpoint.
// The zero value is ready to use and logs through slog.Default.
type Limiter struct {
	mu   sync.Mutex
	last map[string]time.Time
	log  *slog.Logger
	now  func() time.Time
}

// New returns a Limiter that logs through log; nil selects slog.Default at
// each call, so a default installed later is honoured.
func New(log *slog.Logger) *Limiter { return &Limiter{log: log} }

// Failed records one refused credential on endpoint from the peer of r. The
// counter moves every time; the warning names the remote address at most once
// per WarnEvery for that address and endpoint.
func (l *Limiter) Failed(endpoint string, r *http.Request) {
	FailuresTotal.WithLabelValues(endpoint).Inc()
	remote := remoteHost(r.RemoteAddr)
	if !l.allow(endpoint, remote) {
		return
	}
	log := l.log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("control credential refused", "endpoint", endpoint, "remote", remote, "method", r.Method)
}

func (l *Limiter) allow(endpoint, remote string) bool {
	now := time.Now
	if l.now != nil {
		now = l.now
	}
	at := now()
	key := endpoint + "\x00" + remote
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.last[key]; ok && at.Sub(last) < WarnEvery {
		return false
	}
	if l.last == nil {
		l.last = make(map[string]time.Time)
	}
	if len(l.last) >= maxRemotes {
		for k, t := range l.last {
			if at.Sub(t) >= WarnEvery {
				delete(l.last, k)
			}
		}
		// Still full after the sweep: every remembered address was refused in
		// the last interval. Forget one so this address is logged; one extra
		// line for a flooding address is the right direction to fail in.
		for k := range l.last {
			if len(l.last) < maxRemotes {
				break
			}
			delete(l.last, k)
		}
	}
	l.last[key] = at
	return true
}

// remoteHost strips the port from a net/http RemoteAddr so one client is one
// key however many connections it opens. An address without a port, as a test
// or a Unix socket supplies, is used as it is.
func remoteHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
