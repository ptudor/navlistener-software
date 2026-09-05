package state

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/iono"
	"github.com/ptudor/gnss/physconst"
	"github.com/ptudor/navlistener/internal/ingest"
)

// TestMeasuredIonoPipeline drives synthetic dual-frequency observations (GPS
// L1 C/A + L5, a known 4.2 m slant delay at L1, arbitrary phase ambiguities)
// through Store.Apply and asserts the carrier-leveled measurement surfaces in the
// v2 feed's perrecv once the arc matures. The RAWX *parser* has its own synthetic
// frame test in the ingest package; real-capture validation waits on a
// RAWX-enabled receiver (the current fleet emits SFRBX only).
func TestMeasuredIonoPipeline(t *testing.T) {
	const (
		f1     = 1575.42e6
		f2     = 1176.45e6 // GPS L5, sigId 6
		i1     = 4.2       // slant iono at L1, metres
		rho    = 2.2e7
		ambPhi = 12.7 // metres, arbitrary per-arc phase ambiguity difference
	)
	gamma := iono.Gamma(f1, f2)
	lambda1 := physconst.SpeedOfLight / f1
	lambda2 := physconst.SpeedOfLight / f2

	s := New(4)
	now := time.Now()
	for epoch := 0; epoch < iono.MinArc+5; epoch++ {
		tow := 100000.0 + float64(epoch)
		lock := 1000 + epoch*1000
		phi1 := rho - i1
		phi2 := rho - gamma*i1 + ambPhi
		obs := []struct {
			sig int
			pr  float64
			cpM float64
		}{
			{0, rho + i1, phi1},
			{6, rho + gamma*i1, phi2},
		}
		for _, o := range obs {
			lam := lambda1
			if o.sig == 6 {
				lam = lambda2
			}
			s.Apply(&ingest.RawFrame{
				Recv:   now,
				Source: "bench",
				GnssID: gnss.GPS,
				SvID:   7,
				SigID:  o.sig,
				Obs: &ingest.RawObs{
					RcvTow:         tow,
					PrM:            o.pr,
					CpCyc:          o.cpM / lam,
					LockTimeMs:     lock,
					HalfCycleValid: true, CpValid: true,
				},
			})
		}
	}

	svs := s.FeedSVs(time.Now())
	sv, ok := svs["G07@0"]
	if !ok {
		t.Fatal("G07@0 missing from the feed")
	}
	pr := sv.Perrecv["bench"]
	if pr == nil || pr.IonoDelayM == nil {
		t.Fatal("perrecv iono measurement missing")
	}
	if got := *pr.IonoDelayM; math.Abs(got-i1) > 1e-6 {
		t.Errorf("iono_delay_m = %v, want %v (noiseless synthetic)", got, i1)
	}
	if pr.IonoPairSigID == nil || *pr.IonoPairSigID != 6 {
		t.Errorf("iono_pair_sigid = %v, want 6", pr.IonoPairSigID)
	}
	if pr.IonoCal != 0 {
		t.Errorf("iono_cal = %d, want 0 (uncalibrated)", pr.IonoCal)
	}
}

// TestMeasuredIonoTriFrequencyDoesNotThrash guards a receiver
// alternating between two secondary signals per epoch (e.g. GPS L2C=sig3,
// L5=sig6, as an F9T delivering both) previously reset one shared secondary
// slot on every alternation, so the arc never reached iono.MinArc and no delay
// was ever published. Each secondary must now mature independently despite
// the alternation, and the served iono_pair_sigid must be deterministic (not
// dependent on Go's randomized map iteration order) across repeated feed
// builds from the same underlying state.
func TestMeasuredIonoTriFrequencyDoesNotThrash(t *testing.T) {
	const (
		f1  = 1575.42e6
		f2a = 1227.60e6 // GPS L2C, sigId 3
		f2b = 1176.45e6 // GPS L5, sigId 6
		i1  = 4.2
		rho = 2.2e7
	)
	gammaA := iono.Gamma(f1, f2a)
	gammaB := iono.Gamma(f1, f2b)
	lambda1 := physconst.SpeedOfLight / f1
	lambdaA := physconst.SpeedOfLight / f2a
	lambdaB := physconst.SpeedOfLight / f2b

	s := New(1)
	now := time.Now()
	epochs := 2 * (iono.MinArc + 5)
	for epoch := 0; epoch < epochs; epoch++ {
		tow := 300000.0 + float64(epoch)
		lock := 1000 + epoch*1000
		phi1 := rho - i1
		s.Apply(&ingest.RawFrame{
			Recv: now, Source: "bench", GnssID: gnss.GPS, SvID: 9, SigID: 0,
			Obs: &ingest.RawObs{RcvTow: tow, PrM: rho + i1, CpCyc: phi1 / lambda1, LockTimeMs: lock, HalfCycleValid: true, CpValid: true},
		})
		sig, gamma, lambda := 3, gammaA, lambdaA
		if epoch%2 != 0 {
			sig, gamma, lambda = 6, gammaB, lambdaB
		}
		phi2 := rho - gamma*i1
		s.Apply(&ingest.RawFrame{
			Recv: now, Source: "bench", GnssID: gnss.GPS, SvID: 9, SigID: sig,
			Obs: &ingest.RawObs{RcvTow: tow, PrM: rho + gamma*i1, CpCyc: phi2 / lambda, LockTimeMs: lock, HalfCycleValid: true, CpValid: true},
		})
	}

	svs1 := s.FeedSVs(now)
	sv1, ok := svs1["G09@0"]
	if !ok {
		t.Fatal("G09@0 missing from the feed")
	}
	pr1 := sv1.Perrecv["bench"]
	if pr1 == nil || pr1.IonoDelayM == nil || pr1.IonoPairSigID == nil {
		t.Fatal("no iono measurement published despite alternating secondaries maturing -- the regression fix thrash")
	}

	// Repeat with no new data: the reported sigId must be deterministic, not
	// dependent on Go's randomized map iteration order.
	pr2 := s.FeedSVs(now)["G09@0"].Perrecv["bench"]
	if pr2 == nil || pr2.IonoPairSigID == nil || *pr2.IonoPairSigID != *pr1.IonoPairSigID {
		t.Errorf("iono_pair_sigid not stable across repeated feed builds: %v then %v", pr1.IonoPairSigID, pr2.IonoPairSigID)
	}
}

// TestMeasuredIonoArcResetOnSlip confirms a lock-time regression (cycle slip)
// restarts the leveling arc rather than mixing ambiguities across the slip.
func TestMeasuredIonoArcResetOnSlip(t *testing.T) {
	s := New(1)
	apply := func(sig, lock int, tow, pr, cpCyc float64) {
		s.Apply(&ingest.RawFrame{
			Recv: time.Now(), Source: "bench", GnssID: gnss.GPS, SvID: 3, SigID: sig,
			Obs: &ingest.RawObs{RcvTow: tow, PrM: pr, CpCyc: cpCyc, LockTimeMs: lock, HalfCycleValid: true, CpValid: true},
		})
	}
	for epoch := 0; epoch < 5; epoch++ {
		tow := 1000.0 + float64(epoch)
		apply(0, 5000+epoch, tow, 2e7, 2e7/0.19)
		apply(6, 5000+epoch, tow, 2e7+3, (2e7-4)/0.25)
	}
	key := Key{G: gnss.GPS, Sv: 3, Sig: 0}
	sh := s.shardFor(key)
	sh.mu.Lock()
	tr := sh.m[key].ionoBySource["bench"]
	n := tr.secs[6].arc.Count()
	sh.mu.Unlock()
	if n != 5 {
		t.Fatalf("arc count = %d, want 5", n)
	}
	// Lock-time regression on the secondary: arc must reset.
	apply(6, 100, 1006, 2e7+3, (2e7-4)/0.25)
	sh.mu.Lock()
	n = tr.secs[6].arc.Count()
	sh.mu.Unlock()
	if n >= 5 {
		t.Fatalf("arc count = %d after slip, want reset", n)
	}
}

func TestMeasuredIonoCarrierInvalidBreaksArc(t *testing.T) {
	s := New(1)
	base := time.Unix(1_700_000_000, 0)
	apply := func(sig, epoch int, valid bool) {
		s.Apply(&ingest.RawFrame{Recv: base.Add(time.Duration(epoch) * time.Second), Source: "bench",
			GnssID: gnss.GPS, SvID: 4, SigID: sig, Obs: &ingest.RawObs{
				Week: 2200, RcvTow: 200000 + float64(epoch), PrM: 2.2e7 + float64(sig),
				CpCyc: 1.1e8, DoHz: -100, LockTimeMs: 1000 + epoch*1000, HalfCycleValid: true, CpValid: valid,
			}})
	}
	for epoch := 0; epoch < iono.MinArc+2; epoch++ {
		apply(0, epoch, true)
		apply(6, epoch, true)
	}
	if got := s.FeedSVs(base.Add((iono.MinArc + 2) * time.Second))["G04@0"].Perrecv["bench"]; got == nil {
		t.Fatal("mature ionosphere arc missing")
	}
	epoch := iono.MinArc + 2
	apply(0, epoch, true)
	apply(6, epoch, false)
	if got := s.FeedSVs(base.Add(time.Duration(epoch) * time.Second))["G04@0"].Perrecv["bench"]; got != nil {
		t.Fatal("carrier-invalid secondary left old leveled output visible")
	}
	for i := 1; i < iono.MinArc; i++ {
		apply(0, epoch+i, true)
		apply(6, epoch+i, true)
	}
	if got := s.FeedSVs(base.Add(time.Duration(epoch+iono.MinArc-1) * time.Second))["G04@0"].Perrecv["bench"]; got != nil {
		t.Fatal("arc rematured before MinArc fresh continuous samples")
	}
	apply(0, epoch+iono.MinArc, true)
	apply(6, epoch+iono.MinArc, true)
	if got := s.FeedSVs(base.Add(time.Duration(epoch+iono.MinArc) * time.Second))["G04@0"].Perrecv["bench"]; got == nil {
		t.Fatal("arc did not return after MinArc fresh samples")
	}
}

func TestMeasuredIonoRecencyUsesAbsoluteTimeAndExpires(t *testing.T) {
	s := New(1)
	key := Key{G: gnss.GPS, Sv: 8, Sig: 0}
	sh := s.shardFor(key)
	now := time.Unix(1_700_000_000, 0)
	sh.mu.Lock()
	sh.m[key] = &svState{key: key, lastSeen: now, ionoBySource: map[string]*ionoTrack{
		"bench": {secs: map[int]*secTrack{
			3: {delayM: 3, hasDelay: true, lastEpochS: float64(2200*weekSeconds + 604799), delayAt: now.Add(-time.Second)},
			6: {delayM: 6, hasDelay: true, lastEpochS: float64(2201 * weekSeconds), delayAt: now},
		}},
	}}
	sh.mu.Unlock()
	pr := s.FeedSVs(now)["G08@0"].Perrecv["bench"]
	if pr == nil || pr.IonoPairSigID == nil || *pr.IonoPairSigID != 6 {
		t.Fatalf("week-rollover recency selected %+v, want sig 6", pr)
	}
	if pr := s.FeedSVs(now.Add(ionoDelayTTL + time.Second))["G08@0"].Perrecv["bench"]; pr != nil {
		t.Fatalf("stale ionosphere output did not expire: %+v", pr)
	}
}

// TestMeasuredIonoSameFrequencyPairRejected guards Galileo sigId 0 (E1C)
// and sigId 1 (E1B) both map to 1575.42 MHz (signalFreqHz), so a receiver
// reporting RAWX for both components of the primary band must not form a
// geometry-free "pair" between them — Gamma(f1,f2)-1 is exactly 0 for equal
// frequencies, which previously produced a +-Inf iono_delay_m that failed
// json.Marshal and froze the feed (the regression fix mechanism). Feeding both signals
// must leave Perrecv empty and FeedSVs must still marshal cleanly.
func TestMeasuredIonoSameFrequencyPairRejected(t *testing.T) {
	s := New(1)
	apply := func(sig, lock int, tow, pr, cpCyc float64) {
		s.Apply(&ingest.RawFrame{
			Recv: time.Now(), Source: "bench", GnssID: gnss.Galileo, SvID: 11, SigID: sig,
			Obs: &ingest.RawObs{RcvTow: tow, PrM: pr, CpCyc: cpCyc, LockTimeMs: lock, HalfCycleValid: true, CpValid: true},
		})
	}
	for epoch := 0; epoch < iono.MinArc+5; epoch++ {
		tow := 200000.0 + float64(epoch)
		lock := 1000 + epoch*1000
		apply(0, lock, tow, 2.2e7, 2.2e7/0.19) // E1C, primary
		apply(1, lock, tow, 2.2e7, 2.2e7/0.19) // E1B, same carrier as E1C
	}

	svs := s.FeedSVs(time.Now())
	sv, ok := svs["E11@0"]
	if !ok {
		t.Fatal("E11@0 missing from the feed")
	}
	if pr, ok := sv.Perrecv["bench"]; ok {
		t.Errorf("perrecv iono measurement present for a same-frequency pair: %+v", pr)
	}
	if _, err := json.Marshal(svs); err != nil {
		t.Fatalf("json.Marshal(FeedSVs) failed: %v", err)
	}
}

// TestApplyObservationSetsLastSeen guards applyObservation previously never
// touched st.lastSeen, so an SV seen only via RAWX (iono-only) stayed at the zero
// Time -- Expire's `now.Sub(zeroTime) > ttl` is always true, deleting the entry (and
// destroying its leveling arc before it can mature) on every sweep, and a feed
// build that caught it first would serve an absurd last_seen_s (~1.7e9).
func TestApplyObservationSetsLastSeen(t *testing.T) {
	s := New(1)
	now := time.Now()
	s.Apply(&ingest.RawFrame{
		Recv: now, Source: "bench", GnssID: gnss.GPS, SvID: 5, SigID: 0,
		Obs: &ingest.RawObs{RcvTow: 100000, PrM: 2.2e7, CpCyc: 2.2e7 / 0.19, LockTimeMs: 1000, HalfCycleValid: true, CpValid: true},
	})

	// Expire runs a minute later with a generous TTL: the entry must survive (a
	// zero lastSeen would compute an elapsed time in the tens of years, far past
	// any TTL).
	s.Expire(now.Add(time.Minute), 2*time.Hour)

	key := Key{G: gnss.GPS, Sv: 5, Sig: 0}
	sh := s.shardFor(key)
	sh.mu.Lock()
	_, stillPresent := sh.m[key]
	sh.mu.Unlock()
	if !stillPresent {
		t.Fatal("iono-only SV was expired despite a fresh observation -- lastSeen not set")
	}

	sv, ok := s.FeedSVs(now.Add(2 * time.Second))["G05@0"]
	if !ok {
		t.Fatal("G05@0 missing from the feed")
	}
	if sv.LastSeenS < 0 || sv.LastSeenS > 10 {
		t.Errorf("last_seen_s = %d, want a small elapsed time (~2s), not a zero-Time artifact", sv.LastSeenS)
	}
}
