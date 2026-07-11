package state_test

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/detect"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
)

// TestFeedSVEphemerisLessFieldsOmitted guards an SV present only via RAWX
// observables (no nav frame ever decoded, st.haveEph == false) must not report
// fabricated clock/toe fields -- af0/af1/af2 = 0 pointers, a Toe=0 eph_age_m
// (half-week-wrapped garbage up to +-5040 min), or a spurious tow/wn. detectSV's
// eph_aged classifier gates only on EphAgeM != nil, so a fabricated (even if
// technically non-nil) value can debounce-confirm a false event; the entry itself
// (and its per-receiver iono, the whole point of publishing an ephemeris-less SV)
// must still be served. This is an external test (package state_test) because it
// needs both state and detect, which would otherwise import-cycle (detect already
// imports state for the FeedSV type).
func TestFeedSVEphemerisLessFieldsOmitted(t *testing.T) {
	s := state.New(1)
	now := time.Now()
	s.Apply(&ingest.RawFrame{
		Recv: now, Source: "bench", GnssID: gnss.GPS, SvID: 9, SigID: 0,
		Obs: &ingest.RawObs{RcvTow: 100000, PrM: 2.2e7, CpCyc: 2.2e7 / 0.19, LockTimeMs: 1000, CpValid: true},
	})

	svs := s.FeedSVs(now)
	sv, ok := svs["G09@0"]
	if !ok {
		t.Fatal("G09@0 missing from the feed -- observation-only SVs must still be published")
	}
	if sv.Af0 != nil || sv.Af1 != nil || sv.Af2 != nil {
		t.Errorf("Af0/Af1/Af2 = %v/%v/%v, want all nil (no ephemeris)", sv.Af0, sv.Af1, sv.Af2)
	}
	if sv.Tow != nil {
		t.Errorf("Tow = %v, want nil (no ephemeris)", *sv.Tow)
	}
	if sv.Wn != nil {
		t.Errorf("Wn = %v, want nil (no ephemeris)", *sv.Wn)
	}
	if sv.EphAgeM != nil {
		t.Errorf("EphAgeM = %v, want nil (no ephemeris)", *sv.EphAgeM)
	}

	det := detect.New(0)
	events := det.Tick(now, svs, nil)
	for _, e := range events {
		if e.Type == "eph_aged" {
			t.Errorf("detector emitted eph_aged for an ephemeris-less SV: %+v", e)
		}
	}
}
