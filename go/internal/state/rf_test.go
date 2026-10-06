package state

import (
	"math"
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

// clearSky feeds one band's quiet AGC near level at 1 Hz for the given duration and
// returns the instant after the last sample.
func clearSky(s *Store, source string, level int, from time.Time, d time.Duration) time.Time {
	at := from
	for ; at.Before(from.Add(d)); at = at.Add(time.Second) {
		s.Apply(rfSample(source, 0, level+int(at.Unix()%5), 0, 2, at))
	}
	return at
}

func departure(t *testing.T, s *Store, source string, at time.Time) *float64 {
	t.Helper()
	st, ok := s.FeedStationRF(at)[source]
	if !ok || len(st.Bands) != 1 {
		t.Fatalf("station rf = %+v", st)
	}
	return st.Bands[0].AGCDeparture
}

// TestRFBaselineAndDeparture checks the AGC baseline learns the clear-sky level and that
// a jamming departure is measured without poisoning the baseline it is measured against
// (docs/DEFENSE-PNT.md §2), and that a real departure down-weights the station's trust.
func TestRFBaselineAndDeparture(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	at := clearSky(s, "stn1", 4000, now, 11*time.Minute)
	// Then a broadband jammer cuts the AGC hard (gain slammed down).
	for i := 0; i < 120; i++ {
		s.Apply(rfSample("stn1", 0, 2500, 0, 2, at.Add(time.Duration(i)*time.Second)))
	}
	jammed := at.Add(2 * time.Minute)
	dep := departure(t, s, "stn1", jammed)
	// baseline (~4000) − current (2500) ≈ 1500, well past the learn band, and the
	// jammed samples did not move the baseline.
	if dep == nil || *dep < 1490 || *dep > 1510 {
		t.Fatalf("agc_departure = %v, want about 1500", dep)
	}
	if st := s.FeedStationRF(jammed)["stn1"]; st.RFTrust >= 1.0 {
		t.Errorf("rf_trust = %.2f, want down-weighted under a jamming departure", st.RFTrust)
	}
}

// TestRFBaselineWarmUp: no departure is served until ten quiet minutes exist, so a
// station is never measured against its first sample.
func TestRFBaselineWarmUp(t *testing.T) {
	s := New(1)
	now := time.Unix(1_700_000_000, 0)
	at := clearSky(s, "stn1", 4000, now, 9*time.Minute)
	if dep := departure(t, s, "stn1", at); dep != nil {
		t.Fatalf("departure %v served during warm-up", *dep)
	}
	at = clearSky(s, "stn1", 4000, at, 2*time.Minute)
	if dep := departure(t, s, "stn1", at); dep == nil || *dep < -10 || *dep > 10 {
		t.Fatalf("departure after warm-up = %v, want about 0", dep)
	}
}

// TestRFBaselineResistsSlowRamp: a jammer that lowers the AGC by 600 counts an hour
// never departs by more than the learn band from one sample to the next, but the
// baseline may follow at most 150 counts an hour, so the ramp shows as a departure.
func TestRFBaselineResistsSlowRamp(t *testing.T) {
	s := New(1)
	now := time.Unix(1_700_000_000, 0)
	at := clearSky(s, "stn1", 4000, now, 30*time.Minute)
	ramp := 2 * time.Hour
	for i := 0; i < int(ramp/time.Second); i++ {
		level := 4000 - int(600*float64(i)/3600)
		s.Apply(rfSample("stn1", 0, level, 0, 2, at.Add(time.Duration(i)*time.Second)))
	}
	dep := departure(t, s, "stn1", at.Add(ramp))
	if dep == nil || *dep < 800 {
		t.Fatalf("after a 1200-count ramp the departure is %v, want at least the 800-count jamming band", dep)
	}
}

// TestRFBaselineRelearnsAfterJammedStart: a station that starts inside interference
// learns a low baseline; once it is persistently quieter than that for 15 minutes it
// learns again and settles on the clear-sky level.
func TestRFBaselineRelearnsAfterJammedStart(t *testing.T) {
	s := New(1)
	now := time.Unix(1_700_000_000, 0)
	at := clearSky(s, "stn1", 2500, now, 11*time.Minute)
	at = clearSky(s, "stn1", 4000, at, 5*time.Minute)
	if dep := departure(t, s, "stn1", at); dep == nil || *dep > -1400 {
		t.Fatalf("clear sky against a jammed baseline: departure %v, want about -1500", dep)
	}
	at = clearSky(s, "stn1", 4000, at, 27*time.Minute)
	if dep := departure(t, s, "stn1", at); dep == nil || *dep < -10 || *dep > 10 {
		t.Fatalf("after relearning: departure %v, want about 0", dep)
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

func TestRFCn0ResidualGroupedByConstellation(t *testing.T) {
	var mixed []ingest.SatCN0
	for i, elev := range []int{10, 25, 40, 55, 70, 85} {
		mixed = append(mixed,
			ingest.SatCN0{GnssID: 0, ElevDeg: elev, Cn0: 50},
			ingest.SatCN0{GnssID: 2, ElevDeg: elev, Cn0: []int{32, 41, 43, 50, 46, 55}[i]},
		)
	}
	groups := cn0ElevationResidualByConstellation(mixed)
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want GPS and Galileo", groups)
	}
	if gps := groups[0]; gps.NumSats != 6 || gps.Mean != 50 || gps.Resid >= 2 {
		t.Fatalf("GPS group = %+v, want a flat high-C/N0 collapse", gps)
	}
	if gal := groups[2]; gal.NumSats != 6 || gal.Resid < 2 {
		t.Fatalf("Galileo group = %+v, want a genuine spread", gal)
	}
	// A lone SBAS GEO never becomes a gate group.
	mixed = append(mixed, ingest.SatCN0{GnssID: 1, ElevDeg: 30, Cn0: 50})
	if _, ok := cn0ElevationResidualByConstellation(mixed)[1]; ok {
		t.Fatal("SBAS must be excluded from constellation spoof fits")
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

// NAV-SAT may keep the station fresh after every MON-RF band ages out; the feed
// must expose that exact empty-band shape so the detector's regression fix hold is exercised.
func TestRFSatsKeepStationWithEmptyStaleBands(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(rfSample("stn1", 0, 4000, 0, 2, now))
	freshAt := now.Add(rfStaleAfter + time.Minute)
	sats := []ingest.SatCN0{
		{GnssID: 0, ElevDeg: 10, Cn0: 35}, {GnssID: 0, ElevDeg: 25, Cn0: 40},
		{GnssID: 0, ElevDeg: 40, Cn0: 44}, {GnssID: 0, ElevDeg: 55, Cn0: 47},
		{GnssID: 0, ElevDeg: 70, Cn0: 50},
	}
	s.Apply(&ingest.RawFrame{Source: "stn1", Recv: freshAt, RF: &ingest.RawRF{Sats: sats}})
	st, ok := s.FeedStationRF(freshAt)["stn1"]
	if !ok || len(st.Bands) != 0 || st.Cn0Mean == nil {
		t.Fatalf("NAV-SAT-only fresh station = %+v, want no bands plus current C/N0", st)
	}
}

// TestRFBaselineCheckpointRestore: a checkpointed baseline is installed when its band
// first reports, so a restart serves departures at once instead of warming up, and a
// restored station is not live until it is heard from.
func TestRFBaselineCheckpointRestore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := New(1)
	at := clearSky(s, "stn1", 4000, now, 11*time.Minute)
	saved := s.AGCBaselines()["stn1"]
	if len(saved) != 1 || saved[0].Block != 0 || saved[0].Baseline < 3990 || saved[0].Baseline > 4010 || len(saved[0].Minutes) < 10 {
		t.Fatalf("checkpoint = %+v", saved)
	}

	restarted := New(1)
	if n := restarted.RestoreAGCBaselines("stn1", saved, at); n != 1 {
		t.Fatalf("restored %d bands", n)
	}
	if got := restarted.FeedStationRF(at); len(got) != 0 {
		t.Fatalf("restored station live before reporting: %+v", got)
	}
	restarted.Apply(rfSample("stn1", 0, 2500, 0, 2, at))
	if dep := departure(t, restarted, "stn1", at); dep == nil || *dep < 1490 {
		t.Fatalf("departure after restore = %v, want about 1500 at once", dep)
	}
	if again := restarted.RestoreAGCBaselines("stn1", saved, at); again != 0 {
		t.Fatal("a band that already reported was overwritten by a restore")
	}
}

func TestRFBaselineRestoreRejects(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := New(1)
	bad := []AGCBaseline{
		{Block: 0, Baseline: 4000, BaselineAt: now.Add(-25 * time.Hour)},
		{Block: 1, Baseline: math.NaN(), BaselineAt: now},
		{Block: 2, Baseline: 70000, BaselineAt: now},
		{Block: -1, Baseline: 4000, BaselineAt: now},
		{Block: 3, Baseline: 4000, BaselineAt: now.Add(time.Hour)},
	}
	if n := s.RestoreAGCBaselines("stn1", bad, now); n != 0 {
		t.Fatalf("restored %d invalid baselines", n)
	}
	s.RestoreAGCBaselines("stn1", []AGCBaseline{{Block: 0, Baseline: 4000, BaselineAt: now}}, now)
	s.Reset()
	s.Apply(rfSample("stn1", 0, 4000, 0, 2, now))
	if dep := departure(t, s, "stn1", now); dep != nil {
		t.Fatal("a pending restore survived Reset")
	}
}
