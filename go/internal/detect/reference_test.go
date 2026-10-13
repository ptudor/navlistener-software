package detect

import (
	"testing"
	"time"
)

func referenceComparison(delta float64) map[string]OrbitReferenceComparison {
	return map[string]OrbitReferenceComparison{
		"G05@0": {
			Name: "G05", GNSSID: 0, SigID: 0, DeltaM: delta,
			Conf: 2, ConfWeighted: 1.5,
			PositionAt: time.Unix(1_000_000, 0), ReferenceEpoch: time.Unix(999_000, 0),
			ReferenceSource: "independent fixture",
		},
	}
}

func TestOrbitReferenceDivergenceDebounceAndRecovery(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(1_000_000, 0)

	// A degraded first sighting must earn its onset instead of being silently
	// seeded, and unavailable reference ticks hold rather than confirming it.
	if events := d.TickOrbitReference(t0, referenceComparison(2_000)); len(events) != 0 {
		t.Fatalf("first degraded tick emitted %+v", events)
	}
	d.TickOrbitReference(t0.Add(30*time.Second), nil)
	if events := d.TickOrbitReference(t0.Add(50*time.Second), referenceComparison(2_000)); len(events) != 0 {
		t.Fatalf("warning confirmed before dwell: %+v", events)
	}
	if events := d.TickOrbitReference(t0.Add(70*time.Second), referenceComparison(2_000)); len(events) != 1 {
		t.Fatalf("warning onset = %+v", events)
	} else {
		e := events[0]
		if e.Type != "reference_orbit_divergence" || e.OldValue != stateUnknown || e.NewValue != "warn" || e.Severity != SevWarning {
			t.Fatalf("warning event = %+v", e)
		}
		if e.Params["reference_orbit_delta_m"] != float64(2_000) || e.Params["conf"] != 2 || e.Params["sigid"] != 0 {
			t.Fatalf("warning evidence = %+v", e.Params)
		}
	}

	// A gross disagreement escalates, then a continuously healthy comparison
	// clears at info severity.
	d.TickOrbitReference(t0.Add(80*time.Second), referenceComparison(20_000))
	if events := d.TickOrbitReference(t0.Add(145*time.Second), referenceComparison(20_000)); len(events) != 1 || events[0].NewValue != "crit" || events[0].Severity != SevCritical {
		t.Fatalf("critical transition = %+v", events)
	}
	d.TickOrbitReference(t0.Add(150*time.Second), referenceComparison(10))
	if events := d.TickOrbitReference(t0.Add(215*time.Second), referenceComparison(10)); len(events) != 1 || events[0].NewValue != "ok" || events[0].Severity != SevInfo {
		t.Fatalf("recovery = %+v", events)
	}
}

func TestAlmanacReferenceUsesCoarseBandsAndPhysicalSubject(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(2_000_000, 0)
	comparison := OrbitReferenceComparison{
		Name: "C51", GNSSID: 3, DeltaM: 200_000,
		PositionAt: t0, ReferenceEpoch: t0.Add(-time.Hour),
		ReferenceSource: "independent fixture", PositionSource: "almanac",
	}
	input := map[string]OrbitReferenceComparison{"C51": comparison}
	if events := d.TickOrbitReference(t0, input); len(events) != 0 {
		t.Fatalf("first degraded almanac tick emitted %+v", events)
	}
	if events := d.TickOrbitReference(t0.Add(61*time.Second), input); len(events) != 1 {
		t.Fatalf("almanac warning onset = %+v", events)
	} else {
		e := events[0]
		if e.Type != "reference_almanac_divergence" || e.NewValue != "warn" || e.Severity != SevWarning {
			t.Fatalf("almanac warning = %+v", e)
		}
		if e.Params["position_source"] != "almanac" || e.Params["sv"] != "C51" {
			t.Fatalf("almanac evidence = %+v", e.Params)
		}
		if _, exists := e.Params["sigid"]; exists {
			t.Fatalf("signal id invented for an almanac: %+v", e.Params)
		}
	}
}
