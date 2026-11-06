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

func environmentalAssessment(health, change integrity.Result) integrity.Assessment {
	a := healthy
	a.Checks = append(append([]integrity.Result(nil), healthy.Checks...), health, change)
	return a
}

func environmentResult(name string, state integrity.State, at int64, reasons ...string) integrity.Result {
	return integrity.Result{Check: name, Version: 1, Domain: integrity.DomainEnvironment, State: state, Candidate: state,
		EvaluatedAt: at, Metrics: map[string]float64{"value": 1}, Thresholds: map[string]float64{"band": 2}, Reasons: reasons}
}

func TestEnvironmentalEventsAreAdvisory(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_800_000_000, 0)
	normal := environmentalAssessment(
		environmentResult(integrity.CheckEnvironmentHealth, integrity.Assured, t0.Unix()),
		environmentResult(integrity.CheckEnvironmentChange, integrity.Assured, t0.Unix()))
	changed := environmentalAssessment(
		environmentResult(integrity.CheckEnvironmentHealth, integrity.Inconsistent, t0.Add(15*time.Second).Unix(), integrity.ReasonSensorDisagreement),
		environmentResult(integrity.CheckEnvironmentChange, integrity.Unassured, t0.Add(15*time.Second).Unix(), integrity.ReasonHumidityChange))
	d.TickIntegrity(t0, one("s", normal))
	d.TickIntegrity(t0.Add(15*time.Second), one("s", changed))
	events := d.TickIntegrity(t0.Add(15*time.Second+DebounceDuration), one("s", changed))
	health, ok := find(events, "environment_sensor_health")
	if !ok || health.OldValue != "ok" || health.NewValue != "degraded" || health.Severity != SevWarning {
		t.Fatalf("environment sensor event = %+v (ok=%v)", health, ok)
	}
	change, ok := find(events, "environmental_change")
	if !ok || change.OldValue != "normal" || change.NewValue != "severe" || change.Severity != SevWarning {
		t.Fatalf("environment change event = %+v (ok=%v)", change, ok)
	}
	if _, ok := find(events, "spoofing_suspected"); ok {
		t.Fatalf("environmental advisory raised spoofing: %+v", events)
	}
	if _, ok := find(events, "station_assurance"); ok {
		t.Fatalf("environmental advisory changed station assurance: %+v", events)
	}
}

func TestEnvironmentalStaleIsNotRecovery(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_800_000_000, 0)
	normal := environmentalAssessment(
		environmentResult(integrity.CheckEnvironmentHealth, integrity.Assured, t0.Unix()),
		environmentResult(integrity.CheckEnvironmentChange, integrity.Assured, t0.Unix()))
	stale := environmentalAssessment(
		environmentResult(integrity.CheckEnvironmentHealth, integrity.Unavailable, t0.Add(time.Minute).Unix(), integrity.ReasonStale),
		environmentResult(integrity.CheckEnvironmentChange, integrity.Unavailable, t0.Add(time.Minute).Unix(), integrity.ReasonStale))
	d.TickIntegrity(t0, one("s", normal))
	d.TickIntegrity(t0.Add(time.Minute), one("s", stale))
	events := d.TickIntegrity(t0.Add(time.Minute+DebounceDuration), one("s", stale))
	event, ok := find(events, "environment_sensor_health")
	if !ok || event.OldValue != "ok" || event.NewValue != "stale" || event.Severity != SevWarning {
		t.Fatalf("stale environment event = %+v (ok=%v)", event, ok)
	}
	change, ok := find(events, "environmental_change")
	if !ok || change.OldValue != "normal" || change.NewValue != "unknown" || change.Severity != SevWarning {
		t.Fatalf("unknown environment-change event = %+v (ok=%v)", change, ok)
	}
	checks, ok := change.Params["checks"].([]any)
	if !ok || !slices.ContainsFunc(checks, func(value any) bool {
		check, ok := value.(map[string]any)
		reasons, _ := check["reasons"].([]any)
		return ok && check["check"] == integrity.CheckEnvironmentChange && check["state"] == string(integrity.Unavailable) &&
			slices.Contains(reasons, any(integrity.ReasonStale))
	}) {
		t.Fatalf("unknown event lost its cause: %+v", change.Params)
	}
}

func pressureAssessment(regional, barometric integrity.Result) integrity.Assessment {
	a := healthy
	a.Checks = append(append([]integrity.Result(nil), healthy.Checks...), regional, barometric)
	return a
}

func TestPressureEventsDistinguishLocalAndGNSSDivergence(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_800_000_000, 0)
	result := func(name string, domain integrity.Domain, state integrity.State, at time.Time, reasons ...string) integrity.Result {
		return integrity.Result{Check: name, Version: 1, Domain: domain, State: state, Candidate: state,
			EvaluatedAt: at.Unix(), Metrics: map[string]float64{"pressure_neighbour_count": 2}, Reasons: reasons}
	}
	normal := pressureAssessment(
		result(integrity.CheckRegionalPressure, integrity.DomainEnvironment, integrity.Assured, t0),
		result(integrity.CheckBarometricHeight, integrity.DomainBarometricPosition, integrity.Assured, t0))
	local := pressureAssessment(
		result(integrity.CheckRegionalPressure, integrity.DomainEnvironment, integrity.Unassured, t0.Add(time.Minute), integrity.ReasonRegionalPressureDeparture),
		result(integrity.CheckBarometricHeight, integrity.DomainBarometricPosition, integrity.Unavailable, t0.Add(time.Minute), integrity.ReasonLocalPressureAmbiguous))
	d.TickIntegrity(t0, one("s", normal))
	d.TickIntegrity(t0.Add(time.Minute), one("s", local))
	events := d.TickIntegrity(t0.Add(time.Minute+DebounceDuration), one("s", local))
	regional, ok := find(events, "regional_pressure_anomaly")
	if !ok || regional.OldValue != "coherent" || regional.NewValue != "severe" || regional.Severity != SevWarning {
		t.Fatalf("regional pressure event = %+v (ok=%v)", regional, ok)
	}
	barometric, ok := find(events, "barometric_height_consistency")
	if !ok || barometric.OldValue != "consistent" || barometric.NewValue != "unknown" || barometric.Severity != SevWarning {
		t.Fatalf("ambiguous barometric event = %+v (ok=%v)", barometric, ok)
	}
	if _, ok := find(events, "spoofing_suspected"); ok {
		t.Fatalf("local pressure anomaly raised spoofing: %+v", events)
	}
	t.Logf("local events: regional=%s barometric=%s spoofing_event=false", regional.NewValue, barometric.NewValue)

	diverged := pressureAssessment(
		result(integrity.CheckRegionalPressure, integrity.DomainEnvironment, integrity.Assured, t0.Add(3*time.Minute)),
		result(integrity.CheckBarometricHeight, integrity.DomainBarometricPosition, integrity.Unassured, t0.Add(3*time.Minute), integrity.ReasonBarometricHeightDivergence))
	d.TickIntegrity(t0.Add(3*time.Minute), one("s", diverged))
	events = d.TickIntegrity(t0.Add(3*time.Minute+DebounceDuration), one("s", diverged))
	barometric, ok = find(events, "barometric_height_consistency")
	if !ok || barometric.NewValue != "diverged" || barometric.Severity != SevWarning {
		t.Fatalf("barometric divergence event = %+v (ok=%v)", barometric, ok)
	}
	t.Logf("GNSS divergence event: %s=%s", barometric.Type, barometric.NewValue)
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
