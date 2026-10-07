package state

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
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
		Obs: &ingest.RawObs{RcvTow: 100000, Week: 2300, PrM: 2.2e7, CpCyc: 2.2e7 / 0.19, LockTimeMs: 1000, HalfCycleValid: true, CpValid: true},
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

// TestConfWeightedByStationTrust: the weighted corroboration counts a jammed front end
// at its RF trust, an inconsistent station at half and a station whose evidence
// indicates spoofing not at all, while conf still counts every fresh source.
func TestConfWeightedByStationTrust(t *testing.T) {
	const board = "board-0001-aa"
	st := New(4)
	cfg, err := NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{board: fixedSite()})
	if err != nil {
		t.Fatal(err)
	}
	st.SetIntegrity(cfg)
	at := clearSky(st, "stnA", 4000, integrityT0, 11*time.Minute) // stnA learns its AGC baseline
	for i := 0; i < 30; i++ {                                     // then its gain is cut
		st.Apply(rfSample("stnA", 0, 2500, 0, 2, at.Add(time.Duration(i)*time.Second)))
	}
	for i := 540; i < 680; i++ {
		st.Apply(solutionFrame(i, true, 1))
	}
	weighted := func(second int) (int, float64) {
		t.Helper()
		when := integrityT0.Add(time.Duration(second) * time.Second)
		// One source delivers a whole ephemeris set, so the satellite is served.
		for _, words := range [][]uint32{sf1Words(85), sf2Words(85, 205075516), sf3Words(85)} {
			st.Apply(&ingest.RawFrame{Recv: when.Add(-5 * time.Second), Source: "stnC", GnssID: gnss.GPS, SvID: 5, Words: words})
		}
		for _, src := range []string{"stnA", board, "stnC"} {
			st.Apply(&ingest.RawFrame{Recv: when.Add(-5 * time.Second), Source: src, GnssID: gnss.GPS, SvID: 5, Words: sf1Words(85)})
		}
		sv := st.FeedSVs(when)["G05@0"]
		return sv.Conf, sv.ConfWeighted
	}
	if conf, w := weighted(680); conf != 3 || w != 2.3 {
		t.Fatalf("jammed stnA = conf %d weighted %v, want 3 and 2.3", conf, w)
	}
	for i := 680; i < 690; i++ { // the receiver flags spoofing: one domain alone, inconsistent
		st.Apply(solutionFrame(i, true, 2))
	}
	if conf, w := weighted(690); conf != 3 || w != 1.8 {
		t.Fatalf("inconsistent board = conf %d weighted %v, want 3 and 1.8", conf, w)
	}
	for i := 690; i < 700; i++ { // and its position moves 300 m: spoofing indicated
		f := solutionFrame(i, true, 2)
		f.Solution.PVT.LatE7 += 27_000
		st.Apply(f)
	}
	if conf, w := weighted(700); conf != 3 || w != 1.3 {
		t.Fatalf("spoofed board = conf %d weighted %v, want 3 and 1.3", conf, w)
	}
}

func TestConfWeightedKeepsQuietAGCDrift(t *testing.T) {
	st := New(4)
	at := clearSky(st, "stnA", 4000, integrityT0, 11*time.Minute)
	st.Apply(rfSample("stnA", 0, 3700, 0, 0, at))
	if rf := st.FeedStationRF(at)["stnA"]; rf.RFTrust != 1 {
		t.Fatalf("300-count drift rf_trust = %g, want 1", rf.RFTrust)
	}
	for _, words := range [][]uint32{sf1Words(85), sf2Words(85, 205075516), sf3Words(85)} {
		st.Apply(&ingest.RawFrame{Recv: at, Source: "stnA", GnssID: gnss.GPS, SvID: 5, Words: words})
	}
	sv := st.FeedSVs(at)["G05@0"]
	if sv.Conf != 1 || sv.ConfWeighted != float64(sv.Conf) {
		t.Fatalf("quiet drift conf = %d, weighted = %g", sv.Conf, sv.ConfWeighted)
	}
}
