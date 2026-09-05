package state

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

// TestGalileoE5bINAVIsTrackedDeferral : E5b I/NAV pages (u-blox sigId 5/6)
// are carried raw and counted under their own label — never projected into SV
// state or a station capability, and never confused with the E1-B entry or the
// generic "unsupported" bucket. The same CRC-valid word set on sigId 0 still
// produces the E1-B entry, so the guard is keyed on the signal, not the payload.
func TestGalileoE5bINAVIsTrackedDeferral(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	deferred := metrics.DecodeErrorsTotal.WithLabelValues("2", "gal_e5b_deferred")
	unsupported := metrics.DecodeErrorsTotal.WithLabelValues("2", "unsupported")
	for _, sig := range []int{5, 6} {
		s := New(1)
		d0, u0 := testutil.ToFloat64(deferred), testutil.ToFloat64(unsupported)
		for wt := 1; wt <= 5; wt++ {
			s.Apply(&ingest.RawFrame{GnssID: gnss.Galileo, SvID: 12, SigID: sig, Source: "obs-e5b", Recv: now, RecvLocal: now, Words: inavWordN(wt, 40, nil)})
		}
		if got := testutil.ToFloat64(deferred) - d0; got != 5 {
			t.Fatalf("sigId %d: gal_e5b_deferred delta = %v, want 5", sig, got)
		}
		if got := testutil.ToFloat64(unsupported) - u0; got != 0 {
			t.Fatalf("sigId %d: unsupported delta = %v, want 0", sig, got)
		}
		svs := s.FeedSVs(now)
		for _, key := range []string{"E12@0", "E12@5", "E12@6"} {
			if _, has := svs[key]; has {
				t.Fatalf("sigId %d: %s served from a deferred E5b page", sig, key)
			}
		}
		caps := s.FeedStationCapabilities(now)["obs-e5b"]
		if hasCap(caps, 2, sig) || hasCap(caps, 2, 0) {
			t.Fatalf("sigId %d: capability recorded for a deferred signal: %+v", sig, caps)
		}
	}
	// Control: the identical pages on E1-B (sigId 0) still decode to the E1 entry.
	s := New(1)
	for wt := 1; wt <= 5; wt++ {
		s.Apply(&ingest.RawFrame{GnssID: gnss.Galileo, SvID: 12, SigID: 0, Source: "obs-e1", Recv: now, RecvLocal: now, Words: inavWordN(wt, 40, nil)})
	}
	if _, has := s.FeedSVs(now)["E12@0"]; !has {
		t.Fatal("control: E1-B pages no longer produce E12@0")
	}
}
