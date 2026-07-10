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
					RcvTow:     tow,
					PrM:        o.pr,
					CpCyc:      o.cpM / lam,
					LockTimeMs: lock,
					CpValid:    true,
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

// TestMeasuredIonoArcResetOnSlip confirms a lock-time regression (cycle slip)
// restarts the leveling arc rather than mixing ambiguities across the slip.
func TestMeasuredIonoArcResetOnSlip(t *testing.T) {
	s := New(1)
	apply := func(sig, lock int, tow, pr, cpCyc float64) {
		s.Apply(&ingest.RawFrame{
			Recv: time.Now(), Source: "bench", GnssID: gnss.GPS, SvID: 3, SigID: sig,
			Obs: &ingest.RawObs{RcvTow: tow, PrM: pr, CpCyc: cpCyc, LockTimeMs: lock, CpValid: true},
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
	n := tr.arc.Count()
	sh.mu.Unlock()
	if n != 5 {
		t.Fatalf("arc count = %d, want 5", n)
	}
	// Lock-time regression on the secondary: arc must reset.
	apply(6, 100, 1006, 2e7+3, (2e7-4)/0.25)
	sh.mu.Lock()
	n = tr.arc.Count()
	sh.mu.Unlock()
	if n >= 5 {
		t.Fatalf("arc count = %d after slip, want reset", n)
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
			Obs: &ingest.RawObs{RcvTow: tow, PrM: pr, CpCyc: cpCyc, LockTimeMs: lock, CpValid: true},
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
