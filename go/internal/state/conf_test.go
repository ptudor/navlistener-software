package state

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

// TestConfCountsFreshNavSources guards conf is the number of distinct
// sources with a structurally-decoded nav frame for the satellite×signal inside
// the fresh-receiver window — the served floor of INTEGRITY §6's corroboration
// promise. Two sources delivering the same broadcast must read conf 2; a source
// whose last frame ages past the window must stop counting (and be pruned).
func TestConfCountsFreshNavSources(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	frame := func(src string, at time.Time, words []uint32) *ingest.RawFrame {
		return &ingest.RawFrame{Recv: at, Source: src, GnssID: gnss.GPS, SvID: 5, SigID: 0, Words: words}
	}

	// Source A delivers a full ephemeris set; source B independently re-delivers
	// subframe 1 of the same broadcast a few seconds later.
	st.Apply(frame("stnA", now, sf1Words(85)))
	st.Apply(frame("stnA", now, sf2Words(85, 205075516)))
	st.Apply(frame("stnA", now, sf3Words(85)))
	st.Apply(frame("stnB", now.Add(5*time.Second), sf1Words(85)))

	svs := st.FeedSVs(now.Add(10 * time.Second))
	if got := svs["G05@0"].Conf; got != 2 {
		t.Fatalf("conf = %d with two fresh sources, want 2", got)
	}

	// Only B keeps delivering: A's vote ages past freshReceiverWindow.
	st.Apply(frame("stnB", now.Add(65*time.Second), sf1Words(85)))
	svs = st.FeedSVs(now.Add(70 * time.Second))
	if got := svs["G05@0"].Conf; got != 1 {
		t.Fatalf("conf = %d after stnA aged out, want 1", got)
	}

	// Everyone quiet past the window: conf drops to the explicit 0.
	svs = st.FeedSVs(now.Add(200 * time.Second))
	if got := svs["G05@0"].Conf; got != 0 {
		t.Fatalf("conf = %d with no fresh source, want 0", got)
	}
}

// TestApplyRejectsOutOfDomainGnssID guards defense-in-depth gate: a
// frame whose gnssId is outside the documented 0..7-minus-IMES domain (already
// unreachable from the gated ingest boundaries, but a future ingest path might
// not gate) must create no state and must not consult the svId envelope table.
//
// the "no state was created" half is NOT a guard — it holds with the
// gate deleted, because svIDInRange defaults true for these ids and the dispatch
// switch's default arm drops them regardless. The gate's only observable effect
// is its METRIC label, so that is what this pins: the clamped
// {out_of_range, gnssid_range} series takes one increment per rejected frame,
// and no new series appears — a raw gnssId byte reaching a Prometheus label is
// exactly the unbounded-cardinality leak the gate exists to stop (the metrics.go
// contract). With the gate neutralized the frames fall through to the default
// arm, which labels with the raw byte: the clamped delta goes to zero and four
// {gnssid="4"|"8"|"42"|"255"} series appear.
func TestApplyRejectsOutOfDomainGnssID(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	bad := []gnss.GNSSID{4, 8, 42, 255}

	// Touch the clamped child first so it is part of the baseline series count;
	// any growth after Apply is then a NEW label value, not this one appearing.
	clamped := metrics.DecodeErrorsTotal.WithLabelValues("out_of_range", "gnssid_range")
	before := testutil.ToFloat64(clamped)
	seriesBefore := testutil.CollectAndCount(metrics.DecodeErrorsTotal)

	for _, g := range bad {
		st.Apply(&ingest.RawFrame{Recv: now, Source: "test", GnssID: g, SvID: 5, SigID: 0,
			Words: sf1Words(85)})
	}

	if svs := st.FeedSVs(now.Add(time.Second)); len(svs) != 0 {
		t.Fatalf("out-of-domain gnssId created state: %+v", svs)
	}
	if got := testutil.ToFloat64(clamped) - before; got != float64(len(bad)) {
		t.Errorf("decode_errors_total{out_of_range,gnssid_range} rose by %v, want %d (one per rejected frame)",
			got, len(bad))
	}
	if got := testutil.CollectAndCount(metrics.DecodeErrorsTotal); got != seriesBefore {
		t.Errorf("decode_errors_total series count %d → %d: a rejected frame's RAW gnssId byte reached a metric label",
			seriesBefore, got)
	}
}

// TestConfZeroForObservationOnlyEntry: RAWX observables carry no nav bits, so
// they corroborate no broadcast content — an iono-only entry serves conf 0
// (a known value, not an unknown), matching its health_code 0.
func TestConfZeroForObservationOnlyEntry(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	st.Apply(&ingest.RawFrame{
		Recv: now, Source: "stnA", GnssID: gnss.GPS, SvID: 4, SigID: 0,
		Obs: &ingest.RawObs{RcvTow: 100000, Week: 2300, PrM: 2.2e7, CpCyc: 2.2e7 / 0.19, LockTimeMs: 1000, CpValid: true},
	})
	svs := st.FeedSVs(now.Add(time.Second))
	e, ok := svs["G04@0"]
	if !ok {
		t.Fatal("observation-only entry missing from the feed")
	}
	if e.Conf != 0 {
		t.Errorf("conf = %d for an observation-only entry, want 0", e.Conf)
	}
}
