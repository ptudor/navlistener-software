package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
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
func TestApplyRejectsOutOfDomainGnssID(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	for _, bad := range []gnss.GNSSID{4, 8, 42, 255} {
		st.Apply(&ingest.RawFrame{Recv: now, Source: "test", GnssID: bad, SvID: 5, SigID: 0,
			Words: sf1Words(85)})
	}
	if svs := st.FeedSVs(now.Add(time.Second)); len(svs) != 0 {
		t.Fatalf("out-of-domain gnssId created state: %+v", svs)
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
