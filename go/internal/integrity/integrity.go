// Package integrity evaluates station-level PNT assurance checks
// (docs/proposals/STATION-ASSURANCE.md). Each check turns receiver measurements into
// a candidate assurance state with the values and thresholds behind it. A tracker
// filters the candidates (M of the last N) and applies one-sided hysteresis:
// degradation is served at once, recovery only after it has held. Fusion combines
// the served check states into a station state and applies the spoofing evidence
// rule.
//
// The package is pure: it holds no clocks, goroutines or I/O. The caller supplies
// every input with the collector-local instant it was received, so live operation
// and replay of stored inputs evaluate identically.
package integrity

import "time"

// EngineVersion identifies the filter, hysteresis and fusion rules. It changes
// whenever a change could alter a served state for the same inputs and profile.
const EngineVersion = 1

// State is an assurance level (the PNT Integrity library's four levels).
type State string

const (
	// Unavailable: too little input, or the check has not run.
	Unavailable State = "unavailable"
	// Unassured: the input is likely untrustworthy.
	Unassured State = "unassured"
	// Inconsistent: trust cannot be established reliably.
	Inconsistent State = "inconsistent"
	// Assured: the input is likely trustworthy.
	Assured State = "assured"
)

// rank orders states for hysteresis; lower is worse. Unavailable sits above the two
// degraded states and below assured: losing input lowers confidence at once, while
// a check that was unassured must hold its recovery before it may report that it
// merely lacks input.
func (s State) rank() int {
	switch s {
	case Unassured:
		return 0
	case Inconsistent:
		return 1
	case Unavailable:
		return 2
	case Assured:
		return 3
	}
	return 2
}

// level is the numeric level used by fusion: −1, 0 or +1. ok is false for
// Unavailable, which does not take part.
func (s State) level() (lvl float64, ok bool) {
	switch s {
	case Unassured:
		return -1, true
	case Inconsistent:
		return 0, true
	case Assured:
		return 1, true
	}
	return 0, false
}

// worse returns the worse of two states by rank.
func worse(a, b State) State {
	if b.rank() < a.rank() {
		return b
	}
	return a
}

// Domain groups checks that share their underlying measurements. The spoofing rule
// counts domains, not checks, so two views of one measurement cannot form a quorum.
type Domain string

const (
	// DomainSignalPower: per-satellite C/N₀ from NAV-SAT.
	DomainSignalPower Domain = "signal_power"
	// DomainPosition: the receiver's position and velocity solution.
	DomainPosition Domain = "position"
	// DomainReceiverClock: the receiver's clock bias and drift solution.
	DomainReceiverClock Domain = "receiver_clock"
	// DomainTimeReference: GNSS time against clocks independent of the receiver.
	DomainTimeReference Domain = "time_reference"
	// DomainRFEnvironment: front-end AGC, CW and antenna state. Interference
	// evidence, not spoofing evidence.
	DomainRFEnvironment Domain = "rf_environment"
	// DomainReceiverVerdict: the receiver's own spoofing indication.
	DomainReceiverVerdict Domain = "receiver_verdict"
)

// Physics reports whether the domain is an independent physical consistency test
// that counts toward the spoofing quorum.
func (d Domain) Physics() bool {
	switch d {
	case DomainSignalPower, DomainPosition, DomainReceiverClock, DomainTimeReference:
		return true
	}
	return false
}

// Info identifies a check.
type Info struct {
	Name    string
	Version int
	Domain  Domain
	// LowerOnly marks a check that may lower a station's state but never raise
	// it: its assured state is evidence of absence only. The PNT Integrity
	// library calls this disallowing positive weighting.
	LowerOnly bool
}

// Verdict is one raw evaluation of a check, before filtering and hysteresis.
// Metrics are the measured values and Thresholds the bands the evaluation applied,
// both keyed by name with units in the key suffix (_m, _ns, _mps, ...). Reasons are
// stable machine-readable codes.
type Verdict struct {
	State      State
	Metrics    map[string]float64
	Thresholds map[string]float64
	Reasons    []string
}

// Result is a check's served state with the evidence of its latest evaluation.
// Times are Unix seconds, like the rest of the observers feed.
type Result struct {
	Check   string `json:"check"`
	Version int    `json:"version"`
	Domain  Domain `json:"domain"`
	// State is the served state after filtering and hysteresis.
	State State `json:"state"`
	// Candidate is the filtered state the latest evaluations support. It differs
	// from State while a recovery is being held.
	Candidate State `json:"candidate"`
	// Since is when State was last changed.
	Since int64 `json:"since"`
	// RecoveringSince is when the current recovery hold began, if one is running.
	RecoveringSince *int64             `json:"recovering_since,omitempty"`
	EvaluatedAt     int64              `json:"evaluated_at"`
	Metrics         map[string]float64 `json:"metrics,omitempty"`
	Thresholds      map[string]float64 `json:"thresholds,omitempty"`
	Reasons         []string           `json:"reasons,omitempty"`
	lowerOnly       bool
}

// unix converts an instant to the feed's Unix seconds.
func unix(t time.Time) int64 { return t.Unix() }
