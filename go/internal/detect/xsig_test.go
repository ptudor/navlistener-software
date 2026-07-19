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
// inputs.
func gal(svid, sigid, iod int, x, y, z float64, posAt int64) state.FeedSV {
	return state.FeedSV{
		Name: "E14", GnssID: 2, SvID: svid, SigID: sigid,
		HealthCode: 1, IOD: ptrI(iod), PosIOD: ptrI(iod),
		XM: &x, YM: &y, ZM: &z, PosAtUnixNs: posAt,
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
func TestXSigSkipsIncomparablePairs(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(13_000_000, 0)
	epoch := t0.UnixNano()

	// Different IODnav: one signal cut over first.
	skew := map[string]state.FeedSV{
		"E14@0": gal(14, 0, 78, 1.5e7, 2.0e7, 1.0e7, epoch),
		"E14@3": gal(14, 3, 77, 1.5e7+9000, 2.0e7, 1.0e7, epoch),
	}
	d.Tick(t0, skew, nil, 1)
	if evs := d.Tick(t0.Add(120*time.Second), skew, nil, 1); len(evs) != 0 {
		t.Fatalf("IODnav-skew pair emitted %+v, want none", evs)
	}

	// Same IODnav, different propagation epochs (one entry's tick failed).
	stale := map[string]state.FeedSV{
		"E14@0": gal(14, 0, 78, 1.5e7, 2.0e7, 1.0e7, epoch),
		"E14@3": gal(14, 3, 78, 1.5e7+2000, 2.0e7, 1.0e7, epoch+5e8),
	}
	d.Tick(t0.Add(200*time.Second), stale, nil, 1)
	if evs := d.Tick(t0.Add(320*time.Second), stale, nil, 1); len(evs) != 0 {
		t.Fatalf("epoch-skew pair emitted %+v, want none", evs)
	}
}

// TestXSigGalileoOnly: GPS LNAV-vs-CNAV entries are independent curve fits that
// legitimately differ by metres — the tight guard band must not classify them
// (see XSigDivergenceMeters; their comparison needs its own tolerance analysis).
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
	pair := map[string]state.FeedSV{"G05@0": mk(0, 1.5e7), "G05@6": mk(6, 1.5e7+3)}
	d.Tick(t0, pair, nil, 1)
	for _, e := range d.Tick(t0.Add(120*time.Second), pair, nil, 1) {
		if e.Type == "xsig_divergence" {
			t.Fatalf("GPS pair classified by the Galileo-only check: %+v", e)
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
