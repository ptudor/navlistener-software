package state

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
)

func rfSample(source string, block, agc, cw, ant int, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{
		Source: source, Recv: recv,
		RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: block, AGC: agc, CWSuppress: cw, AntStatus: ant}}},
	}
}

// TestRFBaselineAndDeparture checks the AGC baseline learns the clear-sky level and that
// a jamming departure is measured without poisoning the baseline it is measured against
// (docs/DEFENSE-PNT.md §2), and that a real departure down-weights the station's trust.
func TestRFBaselineAndDeparture(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)

	// Clear sky: AGC hovers ~4000. Learn the baseline.
	for i := 0; i < 40; i++ {
		s.Apply(rfSample("stn1", 0, 4000+(i%5), 0, 2, now.Add(time.Duration(i)*time.Second)))
	}
	// Then a broadband jammer cuts the AGC hard (gain slammed down).
	jamAt := now.Add(60 * time.Second)
	for i := 0; i < 5; i++ {
		s.Apply(rfSample("stn1", 0, 2500, 0, 2, jamAt.Add(time.Duration(i)*time.Second)))
	}

	feed := s.FeedStationRF(jamAt.Add(10 * time.Second))
	st, ok := feed["stn1"]
	if !ok || len(st.Bands) != 1 {
		t.Fatalf("station rf = %+v", st)
	}
	b := st.Bands[0]
	if b.AGCDeparture == nil {
		t.Fatal("agc_departure not computed")
	}
	// baseline (~4000) − current (2500) ≈ 1500, well past the learn band.
	if *b.AGCDeparture < 1000 {
		t.Errorf("agc_departure = %.0f, want a large positive departure", *b.AGCDeparture)
	}
	if st.RFTrust >= 1.0 {
		t.Errorf("rf_trust = %.2f, want down-weighted under a jamming departure", st.RFTrust)
	}
}

// TestRFCn0SpoofGate checks the C/N₀-vs-elevation residual: a genuine sky (C/N₀ spread by
// elevation) has high residual variance, while a flat single-transmitter spoofer (every
// SV at the same high C/N₀ regardless of elevation) collapses it toward zero.
func TestRFCn0SpoofGate(t *testing.T) {
	genuine := []ingest.SatCN0{
		{ElevDeg: 10, Cn0: 33}, {ElevDeg: 25, Cn0: 39}, {ElevDeg: 40, Cn0: 44},
		{ElevDeg: 55, Cn0: 47}, {ElevDeg: 70, Cn0: 49}, {ElevDeg: 85, Cn0: 51},
	}
	_, residGenuine, n := cn0ElevationResidual(genuine)
	if n != 6 || residGenuine < 1 {
		t.Errorf("genuine sky residual variance = %.2f (n=%d), want a healthy spread", residGenuine, n)
	}

	spoof := []ingest.SatCN0{
		{ElevDeg: 10, Cn0: 50}, {ElevDeg: 25, Cn0: 50}, {ElevDeg: 40, Cn0: 50},
		{ElevDeg: 55, Cn0: 50}, {ElevDeg: 70, Cn0: 50}, {ElevDeg: 85, Cn0: 50},
	}
	meanSpoof, residSpoof, _ := cn0ElevationResidual(spoof)
	if residSpoof > 0.5 {
		t.Errorf("flat spoofer residual variance = %.3f, want ≈0", residSpoof)
	}
	if meanSpoof != 50 {
		t.Errorf("flat spoofer mean = %.1f, want 50", meanSpoof)
	}
	if residSpoof >= residGenuine {
		t.Errorf("spoofer residual %.3f should be far below genuine %.3f", residSpoof, residGenuine)
	}
}

// TestRFCn0ExcludesElevationUnknownSentinel guards UBX-NAV-SAT elevation
// is valid only in [0,90] -- 91 is the "elevation unknown" sentinel (typical
// for a freshly-acquired SV) and must be excluded, not fed into the regression
// as a real data point; a genuine elev=90 (zenith) is a legitimate value and
// must NOT be excluded.
func TestRFCn0ExcludesElevationUnknownSentinel(t *testing.T) {
	base := []ingest.SatCN0{
		{ElevDeg: 10, Cn0: 33}, {ElevDeg: 25, Cn0: 39}, {ElevDeg: 40, Cn0: 44},
		{ElevDeg: 55, Cn0: 47}, {ElevDeg: 70, Cn0: 49},
	}
	meanBase, residBase, nBase := cn0ElevationResidual(base)

	withSentinel := append(append([]ingest.SatCN0{}, base...), ingest.SatCN0{ElevDeg: 91, Cn0: 45})
	mean, resid, n := cn0ElevationResidual(withSentinel)
	if n != nBase {
		t.Errorf("n = %d, want %d (the elev=91 sentinel must be excluded)", n, nBase)
	}
	if mean != meanBase || resid != residBase {
		t.Errorf("mean/resid = %v/%v, want unchanged %v/%v -- the sentinel biased the fit", mean, resid, meanBase, residBase)
	}

	withZenith := append(append([]ingest.SatCN0{}, base...), ingest.SatCN0{ElevDeg: 90, Cn0: 45})
	_, _, n = cn0ElevationResidual(withZenith)
	if n != nBase+1 {
		t.Errorf("n = %d, want %d (elev=90 is a genuine zenith value, must not be excluded)", n, nBase+1)
	}
}

// TestRFStale drops a station whose RF telemetry has gone silent past the stale window.
func TestRFStale(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(rfSample("old", 0, 4000, 0, 2, now))
	if got := s.FeedStationRF(now.Add(rfStaleAfter + time.Minute)); len(got) != 0 {
		t.Errorf("stale station still present: %+v", got)
	}
	if got := s.FeedStationRF(now.Add(time.Minute)); len(got) != 1 {
		t.Errorf("fresh station missing: %+v", got)
	}
}

// TestRFCn0AgesIndependentlyOfMonRF guards MON-RF and NAV-SAT are
// independent frames that jointly keep the station-level lastSeen fresh. If
// NAV-SAT stops while MON-RF keeps arriving, the C/N₀-vs-elevation residual
// must age out and stop feeding the spoof gate -- otherwise a once-tripped
// spoof classification can never clear.
func TestRFCn0AgesIndependentlyOfMonRF(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)

	sats := []ingest.SatCN0{
		{ElevDeg: 10, Cn0: 33}, {ElevDeg: 25, Cn0: 39}, {ElevDeg: 40, Cn0: 44},
		{ElevDeg: 55, Cn0: 47}, {ElevDeg: 70, Cn0: 49},
	}
	// NAV-SAT arrives once at t0.
	s.Apply(&ingest.RawFrame{Source: "stn1", Recv: now, RF: &ingest.RawRF{Sats: sats}})

	// MON-RF keeps the station itself fresh well past rfStaleAfter, but NAV-SAT
	// never arrives again.
	monAt := now.Add(rfStaleAfter + time.Minute)
	s.Apply(rfSample("stn1", 0, 4000, 0, 2, monAt))

	feed := s.FeedStationRF(monAt)
	st, ok := feed["stn1"]
	if !ok {
		t.Fatal("station missing from feed (MON-RF alone should keep it fresh)")
	}
	if st.Cn0Mean != nil || st.Cn0Resid != nil {
		t.Errorf("cn0 mean/resid = %v/%v, want nil -- the NAV-SAT sample is stale", st.Cn0Mean, st.Cn0Resid)
	}
	if st.NumSats != 0 {
		t.Errorf("num_sats = %d, want 0 once the C/N0 sample is stale", st.NumSats)
	}
}

// TestRFStaleBandDroppedFromClassification guards other half: a band
// whose own telemetry has stopped must not keep contributing its last-ever
// agc/jamState to the jamming classification once another band (or NAV-SAT)
// keeps the station itself alive.
func TestRFStaleBandDroppedFromClassification(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)

	// Band 0 reports once, then goes silent.
	s.Apply(rfSample("stn1", 0, 4000, 0, 2, now))
	// Band 1 keeps the station alive well past rfStaleAfter.
	freshAt := now.Add(rfStaleAfter + time.Minute)
	s.Apply(rfSample("stn1", 1, 4000, 0, 2, freshAt))

	feed := s.FeedStationRF(freshAt)
	st, ok := feed["stn1"]
	if !ok {
		t.Fatal("station missing from feed")
	}
	if len(st.Bands) != 1 || st.Bands[0].Block != 1 {
		t.Errorf("bands = %+v, want only fresh band 1 (stale band 0 dropped)", st.Bands)
	}
}
