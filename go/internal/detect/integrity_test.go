package detect

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/integrity"
)

// TestWiredSpoofGatesMatchesIntegrity guards the published coverage numbers: the
// gauges must state what integrity.Fuse actually counts.
func TestWiredSpoofGatesMatchesIntegrity(t *testing.T) {
	if WiredSpoofGates != integrity.WiredPhysicsDomains() {
		t.Fatalf("WiredSpoofGates = %d, integrity wires %d physics domains", WiredSpoofGates, integrity.WiredPhysicsDomains())
	}
	if SpoofGateQuorum != integrity.SpoofingQuorum || SpoofGateQuorum < 2 {
		t.Fatalf("SpoofGateQuorum = %d, integrity uses %d; the quorum must stay at least 2", SpoofGateQuorum, integrity.SpoofingQuorum)
	}
	if WiredSpoofGates < SpoofGateQuorum {
		t.Fatal("spoofing_suspected is unreachable: fewer wired domains than the quorum")
	}
}

func check(name string, d integrity.Domain, s integrity.State) integrity.Result {
	return integrity.Result{Check: name, Version: 1, Domain: d, State: s, Candidate: s,
		Metrics: map[string]float64{"value": 1}, Thresholds: map[string]float64{"band": 2}}
}

// assessment fuses a set of checks the way the integrity package does, so the
// detector sees exactly what live state would give it.
func assessment(checks ...integrity.Result) integrity.Assessment {
	return integrity.Assessment{Fusion: integrity.Fuse(checks, nil), Engine: integrity.EngineVersion,
		ConfigHash: "sha256:test", Mode: integrity.ModeFixed, Surveyed: true, Checks: checks}
}

var (
	healthy = assessment(
		check(integrity.CheckStaticPosition, integrity.DomainPosition, integrity.Assured),
		check(integrity.CheckClockBiasDrift, integrity.DomainReceiverClock, integrity.Assured),
		check(integrity.CheckUTCOffset, integrity.DomainTimeReference, integrity.Assured))
	takeover = assessment(
		check(integrity.CheckStaticPosition, integrity.DomainPosition, integrity.Unassured),
		check(integrity.CheckClockBiasDrift, integrity.DomainReceiverClock, integrity.Unassured),
		check(integrity.CheckUTCOffset, integrity.DomainTimeReference, integrity.Assured))
	positionOnly = assessment(
		check(integrity.CheckStaticPosition, integrity.DomainPosition, integrity.Unassured),
		check(integrity.CheckClockBiasDrift, integrity.DomainReceiverClock, integrity.Assured),
		check(integrity.CheckUTCOffset, integrity.DomainTimeReference, integrity.Assured))
	noPhysics = assessment(
		check(integrity.CheckStaticPosition, integrity.DomainPosition, integrity.Unavailable),
		check(integrity.CheckAGC, integrity.DomainRFEnvironment, integrity.Inconsistent))
)

func one(id string, a integrity.Assessment) map[string]integrity.Assessment {
	return map[string]integrity.Assessment{id: a}
}

// TestSpoofingSuspectedReachable replaces the former dormancy test: with four
// physics domains wired, two agreeing domains confirm spoofing_suspected, and the
// event carries the evidence.
func TestSpoofingSuspectedReachable(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_800_000_000, 0)
	d.TickIntegrity(t0, one("s", healthy))
	d.TickIntegrity(t0.Add(15*time.Second), one("s", takeover))
	if evs := d.TickIntegrity(t0.Add(45*time.Second), one("s", takeover)); len(evs) != 0 {
		t.Fatalf("confirmed before the onset dwell: %+v", evs)
	}
	evs := d.TickIntegrity(t0.Add(15*time.Second+DebounceDuration), one("s", takeover))
	e, ok := find(evs, "spoofing_suspected")
	if !ok || e.OldValue != "ok" || e.NewValue != "suspected" || e.Severity != SevCritical {
		t.Fatalf("spoofing_suspected = %+v (ok=%v)", e, ok)
	}
	if got := e.Params["unassured_domains"]; !slices.Equal(got.([]any), []any{"position", "receiver_clock"}) {
		t.Fatalf("unassured_domains = %v", got)
	}
	if e.Params["config_hash"] != "sha256:test" || e.Params["engine_version"] != integrity.EngineVersion || e.Params["spoofing_indicated"] != true {
		t.Fatalf("provenance params = %+v", e.Params)
	}
	checks := e.Params["checks"].([]any)
	if len(checks) != 3 || checks[0].(map[string]any)["check"] != integrity.CheckStaticPosition ||
		checks[0].(map[string]any)["thresholds"].(map[string]any)["band"] != 2.0 {
		t.Fatalf("checks = %+v", checks)
	}
	if _, err := json.Marshal(e.Params); err != nil {
		t.Fatalf("params do not serialize: %v", err)
	}
	if a, ok := find(evs, "station_assurance"); !ok || a.NewValue != "unassured" || a.Severity != SevCritical {
		t.Fatalf("station_assurance = %+v (ok=%v)", a, ok)
	}

	// The checks already held their recovery, so the event clears after the
	// symmetric debounce.
	d.TickIntegrity(t0.Add(2*time.Minute), one("s", healthy))
	e, ok = find(d.TickIntegrity(t0.Add(2*time.Minute+DebounceDuration), one("s", healthy)), "spoofing_suspected")
	if !ok || e.NewValue != "ok" || e.Severity != SevInfo {
		t.Fatalf("clear = %+v (ok=%v)", e, ok)
	}
}

func TestSingleDomainIsNotSpoofing(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_800_000_000, 0)
	d.TickIntegrity(t0, one("s", healthy))
	d.TickIntegrity(t0.Add(15*time.Second), one("s", positionOnly))
	evs := d.TickIntegrity(t0.Add(15*time.Second+DebounceDuration), one("s", positionOnly))
	if _, ok := find(evs, "spoofing_suspected"); ok {
		t.Fatal("one physics domain raised spoofing_suspected")
	}
	if a, ok := find(evs, "station_assurance"); !ok || a.NewValue != "inconsistent" || a.Severity != SevWarning {
		t.Fatalf("station_assurance = %+v (ok=%v)", a, ok)
	}
}

// TestIntegrityColdStart: a station already spoofed when first assessed raises both
// events from unknown, like the station RF events.
func TestIntegrityColdStart(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_800_000_000, 0)
	if evs := d.TickIntegrity(t0, one("s", takeover)); len(evs) != 0 {
		t.Fatalf("first observation emitted: %+v", evs)
	}
	evs := d.TickIntegrity(t0.Add(DebounceDuration), one("s", takeover))
	for _, typ := range []string{"spoofing_suspected", "station_assurance"} {
		if e, ok := find(evs, typ); !ok || e.OldValue != stateUnknown {
			t.Fatalf("%s = %+v (ok=%v), want a transition from unknown", typ, e, ok)
		}
	}
}

// TestIntegrityHoldsWithoutInput: missing physics input is not a spoofing recovery,
// and an unavailable station state is not an assurance transition.
func TestIntegrityHoldsWithoutInput(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_800_000_000, 0)
	d.TickIntegrity(t0, one("s", healthy))
	d.TickIntegrity(t0.Add(15*time.Second), one("s", takeover))
	d.TickIntegrity(t0.Add(15*time.Second+DebounceDuration), one("s", takeover))
	for at := 2 * time.Minute; at <= 20*time.Minute; at += 15 * time.Second {
		if evs := d.TickIntegrity(t0.Add(at), one("s", noPhysics)); len(evs) != 0 {
			for _, e := range evs {
				if e.Type == "spoofing_suspected" {
					t.Fatalf("spoofing recovered without physics input: %+v", e)
				}
			}
		}
	}
	if band, _ := d.currentBand("s", "spoofing"); band != "suspected" {
		t.Fatalf("spoofing band = %q, want suspected held", band)
	}
	// A station that never had physics input or a fused state emits nothing.
	quiet := New(0)
	for at := time.Duration(0); at <= 5*time.Minute; at += 15 * time.Second {
		if evs := quiet.TickIntegrity(t0.Add(at), one("x", integrity.Assessment{Fusion: integrity.Fusion{State: integrity.Unavailable}})); len(evs) != 0 {
			t.Fatalf("station without input emitted: %+v", evs)
		}
	}
}
