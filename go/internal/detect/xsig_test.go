package detect

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/state"
)

func ptrI(v int) *int { return &v }

// gal builds one Galileo satellite×signal entry with a fresh position, its
// position-producing IODnav (PosIOD — stamped at propagation, not the served
// current-set iod), and propagation epoch — the cross-signal comparison's
// inputs. The served iod and PosIOD agree here, the steady state
// outside an ephemeris changeover; galSplitIOD builds the changeover window.
func gal(svid, sigid, iod int, x, y, z float64, posAt int64) state.FeedSV {
	return galSplitIOD(svid, sigid, iod, iod, x, y, z, posAt)
}

// galSplitIOD is gal with the two IODnav labels set independently: iod is the
// CURRENT data set the feed serves, posIOD is the set that actually produced the
// served position. They differ in the window between an ephemeris apply and that
// signal's next Propagate tick — the changeover the cross-signal check must not
// read as divergence.
func galSplitIOD(svid, sigid, iod, posIOD int, x, y, z float64, posAt int64) state.FeedSV {
	return state.FeedSV{
		Name: "E14", GnssID: 2, SvID: svid, SigID: sigid,
		HealthCode: 1, IOD: ptrI(iod), PosIOD: ptrI(posIOD),
		XM: &x, YM: &y, ZM: &z, PosAtUnixNs: posAt,
	}
}

// noXSig fails if any tick in the sequence confirms an xsig transition. Each
// entry is driven at its own instant so the sequence spans the debounce window.
func noXSig(t *testing.T, d *Detector, base time.Time, svs map[string]state.FeedSV, offsets ...time.Duration) {
	t.Helper()
	for _, off := range offsets {
		for _, e := range d.Tick(base.Add(off), svs, nil, 1) {
			if e.Type == "xsig_divergence" {
				t.Fatalf("xsig_divergence confirmed at +%s: %+v", off, e)
			}
		}
	}
}

// TestXSigAgreementAndDivergence guards two same-IODnav Galileo signals
// propagated at the identical epoch seed "ok" when identical (agreeing signals
// differ by exactly zero — same CED, same propagator), and confirm a warning
// xsig_divergence when their decoded positions disagree beyond the guard band.
func TestXSigAgreementAndDivergence(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(12_000_000, 0)
	epoch := t0.UnixNano()

	agree := map[string]state.FeedSV{
		"E14@0": gal(14, 0, 77, 1.5e7, 2.0e7, 1.0e7, epoch),
		"E14@3": gal(14, 3, 77, 1.5e7, 2.0e7, 1.0e7, epoch),
	}
	if evs := d.Tick(t0, agree, nil, 1); len(evs) != 0 {
		t.Fatalf("seed tick emitted %+v, want none", evs)
	}

	// F/NAV decodes different element bits: 5 m apart at the same epoch/IODnav.
	diverge := map[string]state.FeedSV{
		"E14@0": gal(14, 0, 77, 1.5e7, 2.0e7, 1.0e7, epoch),
		"E14@3": gal(14, 3, 77, 1.5e7+5, 2.0e7, 1.0e7, epoch),
	}
	d.Tick(t0.Add(10*time.Second), diverge, nil, 1)
	evs := d.Tick(t0.Add(80*time.Second), diverge, nil, 1)
	var found *Event
	for i := range evs {
		if evs[i].Type == "xsig_divergence" {
			found = &evs[i]
		}
	}
	if found == nil {
		t.Fatalf("no xsig_divergence in %+v", evs)
	}
	if found.NewValue != "divergent" || found.Severity != SevWarning {
		t.Errorf("xsig_divergence = %s/%d, want divergent/1", found.NewValue, found.Severity)
	}
	if found.SV != "E14" {
		t.Errorf("subject = %s, want the physical SV name E14 (no @sig)", found.SV)
	}
	if found.Params["sig_a"] != 0 || found.Params["sig_b"] != 3 {
		t.Errorf("params sigs = %v/%v, want 0/3", found.Params["sig_a"], found.Params["sig_b"])
	}

	// Back in agreement: the recovery confirms at info severity.
	d.Tick(t0.Add(200*time.Second), agree, nil, 1)
	evs = d.Tick(t0.Add(280*time.Second), agree, nil, 1)
	if len(evs) != 1 || evs[0].Type != "xsig_divergence" || evs[0].NewValue != "ok" || evs[0].Severity != SevInfo {
		t.Fatalf("got %+v, want the ok recovery at info", evs)
	}
}

// TestXSigSkipsIncomparablePairs: a changeover skew (different IODnav) and an
// epoch skew (different PosAtUnixNs) are designed behavior, not divergence —
// no classification happens (machines hold; no phantom events), even when the
// positions are kilometres apart.
//
// each scenario is presented to a machine ALREADY CONFIRMED "ok", not
// to a fresh detector. observe() seeds a first-ever subject silently, so against
// a fresh detector "no events were emitted" is satisfied even when the wrong
// classification is computed — it just becomes the silent seed, and this test
// passed with the skip preconditions deleted outright. From a confirmed "ok" a
// regression instead produces a confirmed ok→divergent transition and fails.
func TestXSigSkipsIncomparablePairs(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(13_000_000, 0)
	epoch := t0.UnixNano()

	// Seed "ok": one IODnav, one epoch, identical positions (agreeing signals
	// differ by exactly zero — same CED, same propagator).
	agree := map[string]state.FeedSV{
		"E14@0": gal(14, 0, 78, 1.5e7, 2.0e7, 1.0e7, epoch),
		"E14@3": gal(14, 3, 78, 1.5e7, 2.0e7, 1.0e7, epoch),
	}
	if evs := d.Tick(t0, agree, nil, 1); len(evs) != 0 {
		t.Fatalf("seed tick emitted %+v, want none", evs)
	}

	// Different IODnav: one signal cut over first. 9 km apart, but the pair is
	// not comparable — held across the debounce and well past it.
	skew := map[string]state.FeedSV{
		"E14@0": gal(14, 0, 78, 1.5e7, 2.0e7, 1.0e7, epoch),
		"E14@3": gal(14, 3, 77, 1.5e7+9000, 2.0e7, 1.0e7, epoch),
	}
	noXSig(t, d, t0, skew, 10*time.Second, 80*time.Second, 150*time.Second)

	// Same IODnav, different propagation epochs (one entry's tick failed).
	stale := map[string]state.FeedSV{
		"E14@0": gal(14, 0, 78, 1.5e7, 2.0e7, 1.0e7, epoch),
		"E14@3": gal(14, 3, 78, 1.5e7+2000, 2.0e7, 1.0e7, epoch+5e8),
	}
	noXSig(t, d, t0, stale, 200*time.Second, 270*time.Second, 340*time.Second)
}

// TestXSigKeysOnPosIOD guards the consuming half of the PosIOD stamp :
// the detector must pair positions by the IODnav that PRODUCED them (PosIOD),
// never by the served current-data-set label (IOD). In the window between an
// ephemeris apply and that signal's next Propagate tick a signal serves the NEW
// iod beside the OLD set's position, so keying on IOD pairs two positions from
// DIFFERENT data sets and reads the changeover delta as divergence — the exact
// false alarm the stamp exists to prevent. state's TestPosIODStampedAtPropagation
// pins only the producing half; every other xsig fixture sets IOD == PosIOD, so
// reverting detectXSig to sv.IOD left the whole suite green.
func TestXSigKeysOnPosIOD(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(13_500_000, 0)
	epoch := t0.UnixNano()

	agree := map[string]state.FeedSV{
		"E14@0": gal(14, 0, 85, 1.5e7, 2.0e7, 1.0e7, epoch),
		"E14@3": gal(14, 3, 85, 1.5e7, 2.0e7, 1.0e7, epoch),
	}
	if evs := d.Tick(t0, agree, nil, 1); len(evs) != 0 {
		t.Fatalf("seed tick emitted %+v, want none", evs)
	}

	// Both signals now serve data set 86, but only E14@0 has re-propagated under
	// it: E14@3's position is still the one set 85 produced, 3 km away across the
	// changeover. PosIOD keying sees 86 vs 85 and skips (nothing comparable);
	// IOD keying sees 86 vs 86, compares two different sets' positions, and
	// confirms a false divergence.
	changeover := map[string]state.FeedSV{
		"E14@0": galSplitIOD(14, 0, 86, 86, 1.5e7+3000, 2.0e7, 1.0e7, epoch),
		"E14@3": galSplitIOD(14, 3, 86, 85, 1.5e7, 2.0e7, 1.0e7, epoch),
	}
	noXSig(t, d, t0, changeover, 10*time.Second, 80*time.Second, 150*time.Second)
}

// TestXSigGalileoOnly: GPS LNAV-vs-CNAV entries are independent curve fits that
// legitimately differ by metres — the tight guard band must not classify them
// (see XSigDivergenceMeters; their comparison needs its own tolerance analysis).
//
// the GPS pair AGREES on the first tick, so a detector with the
// Galileo-only gate deleted seeds "ok" there and confirms ok→divergent on the
// metres-apart ticks that follow. Without that seeding tick the wrong
// classification is swallowed by observe()'s silent first sighting and the test
// passes with the gate gone.
func TestXSigGalileoOnly(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(14_000_000, 0)
	epoch := t0.UnixNano()
	mk := func(sigid int, x float64) state.FeedSV {
		sv := gps("G05", 5, 1)
		sv.SigID = sigid
		sv.IOD, sv.PosIOD = ptrI(40), ptrI(40)
		sv.XM, sv.YM, sv.ZM = &x, ptrF(2.0e7), ptrF(1.0e7)
		sv.PosAtUnixNs = epoch
		return sv
	}
	agree := map[string]state.FeedSV{"G05@0": mk(0, 1.5e7), "G05@6": mk(6, 1.5e7)}
	d.Tick(t0, agree, nil, 1)

	pair := map[string]state.FeedSV{"G05@0": mk(0, 1.5e7), "G05@6": mk(6, 1.5e7+3)}
	for _, off := range []time.Duration{10 * time.Second, 80 * time.Second, 150 * time.Second} {
		for _, e := range d.Tick(t0.Add(off), pair, nil, 1) {
			if e.Type == "xsig_divergence" {
				t.Fatalf("GPS pair classified by the Galileo-only check at +%s: %+v", off, e)
			}
		}
	}
}

// TestEventParamsCarrySigID guards dedup contract: every SV event's
// params carry sigid beside the physical-SV grouping key sv, so consumers can
// coalesce dual-signal double-emits without parsing the name@sigid subject.
func TestEventParamsCarrySigID(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(15_000_000, 0)
	sv := state.FeedSV{Name: "E14", GnssID: 2, SvID: 14, SigID: 3, HealthCode: 1}
	d.Tick(t0, map[string]state.FeedSV{"E14@3": sv}, nil, 1)
	bad := sv
	bad.HealthCode, bad.HealthIssueLevel = 3, 2
	m := map[string]state.FeedSV{"E14@3": bad}
	d.Tick(t0.Add(10*time.Second), m, nil, 1)
	evs := d.Tick(t0.Add(80*time.Second), m, nil, 1)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if got := evs[0].Params["sigid"]; got != 3 {
		t.Fatalf("params sigid = %v, want 3", got)
	}
	if got := evs[0].Params["sv"]; got != "E14" {
		t.Fatalf("params sv = %v, want the physical name E14", got)
	}
}
