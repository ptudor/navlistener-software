package state

import (
	"fmt"
	"math"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/iono"
	"github.com/ptudor/gnss/physconst"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

// Measured slant ionosphere from raw observables (docs/MATH.md §7.4): every
// dual-frequency observation pair of one SV from one receiver yields the actual
// first-order slant delay via the geometry-free combination, carrier-leveled per
// tracking arc. v1 publishes the measurement uncalibrated (receiver DCB not yet
// separated — iono_cal 0, docs/OUTPUT.md §1.1); the model comparison lands with
// the per-observer geometry pass.

// obsSample is the last observation of one signal used for pairing.
type obsSample struct {
	rcvTow   float64
	prM      float64
	cpM      float64 // carrier phase converted to metres (cycles × λ)
	lockMs   int
	haveCp   bool
	haveSamp bool
}

// ionoTrack pairs the primary and one secondary signal of an SV from one source
// and carries the leveling arc across epochs.
type ionoTrack struct {
	pri, sec obsSample
	pairSig  int
	arc      iono.Arc
	delayM   float64
	hasDelay bool
}

// pairEpsilonS is the maximum receiver-time difference for two signals to count
// as the same epoch (RAWX delivers all signals of an epoch in one message, so
// this is generous).
const pairEpsilonS = 0.05

// applyObservation folds one raw observable into the iono estimator. The state
// is keyed on the SV's primary signal; secondary signals contribute to the pair.
func (s *Store) applyObservation(f *ingest.RawFrame) {
	o := f.Obs
	priSig := primarySig(f.GnssID)
	f1 := signalFreqHz(f.GnssID, priSig, f.FreqID)
	f2 := signalFreqHz(f.GnssID, f.SigID, f.FreqID)
	if f1 == 0 || f2 == 0 {
		return // unmapped signal
	}
	if f.SigID != priSig && f2 == f1 {
		// signalFreqHz maps multiple sigIds to one carrier (e.g. Galileo
		// E1C/E1B both -> 1575.42 MHz), so a receiver reporting RAWX for both
		// components of the primary band forms a same-frequency "pair" — the
		// geometry-free Slant divides by Gamma(f1,f2)-1, which is exactly 0 for
		// f1==f2, yielding ±Inf that later fails json.Marshal and freezes the feed
		// (the regression fix mechanism). Not a genuine secondary signal; drop it. (The
		// primary signal's own observation, f.SigID == priSig, trivially has
		// f2 == f1 by construction and must not be rejected here.)
		return
	}
	if f.GnssID == gnss.GLONASS {
		return // FDMA inter-frequency biases need per-channel calibration; v2
	}

	key := Key{G: f.GnssID, Sv: f.SvID, Sig: primarySig(f.GnssID)}
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	st := sh.m[key]
	if st == nil {
		st = &svState{key: key}
		sh.m[key] = st
	}
	if st.ionoBySource == nil {
		st.ionoBySource = map[string]*ionoTrack{}
	}
	tr := st.ionoBySource[f.Source]
	if tr == nil {
		tr = &ionoTrack{}
		st.ionoBySource[f.Source] = tr
	}

	lambda := physconst.SpeedOfLight / f2
	sample := obsSample{
		rcvTow:   o.RcvTow,
		prM:      o.PrM,
		cpM:      o.CpCyc * lambda,
		lockMs:   o.LockTimeMs,
		haveCp:   o.CpValid,
		haveSamp: true,
	}

	if f.SigID == key.Sig {
		// A lock-time regression means a new tracking arc: the phase ambiguity
		// changed, so the leveling restarts (docs/MATH.md §7.4).
		if sample.lockMs < tr.pri.lockMs {
			tr.arc.Reset()
		}
		tr.pri = sample
	} else {
		if f.SigID != tr.pairSig || sample.lockMs < tr.sec.lockMs {
			tr.arc.Reset()
			tr.pairSig = f.SigID
		}
		tr.sec = sample
	}

	if !tr.pri.haveSamp || !tr.sec.haveSamp || !tr.pri.haveCp || !tr.sec.haveCp {
		return
	}
	if math.Abs(tr.pri.rcvTow-tr.sec.rcvTow) > pairEpsilonS {
		return // different epochs; wait for the pair to complete
	}

	f2sec := signalFreqHz(f.GnssID, tr.pairSig, f.FreqID)
	pGF := tr.sec.prM - tr.pri.prM
	phiGF := tr.pri.cpM - tr.sec.cpM
	tr.arc.Add(pGF, phiGF)
	if slant, ok := tr.arc.Slant(phiGF, f1, f2sec, 0); ok {
		tr.delayM = slant
		tr.hasDelay = true
		metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "iono_pair").Inc()
	}
	// Consume the epoch so the next Add pairs fresh samples.
	tr.pri.haveSamp = false
	tr.sec.haveSamp = false
}

// primarySig is the signal each constellation's iono measurement is referenced
// to (the u-blox sigId of the primary civil signal, docs/CONSTELLATIONS.md §2.1;
// Galileo's RAWX pilot is E1 C = 0).
func primarySig(g gnss.GNSSID) int { return 0 }

// signalFreqHz maps (constellation, u-blox sigId) to the carrier frequency
// (docs/CONSTELLATIONS.md §2.1, docs/MATH.md §2.2). GLONASS FDMA depends on the
// channel k = freqId − 7. Returns 0 for unmapped signals.
func signalFreqHz(g gnss.GNSSID, sigID, freqID int) float64 {
	const mhz = 1e6
	switch g {
	case gnss.GPS:
		switch sigID {
		case 0:
			return 1575.42 * mhz
		case 3, 4:
			return 1227.60 * mhz
		case 6, 7:
			return 1176.45 * mhz
		}
	case gnss.QZSS:
		switch sigID {
		case 0, 1:
			return 1575.42 * mhz
		case 4, 5:
			return 1227.60 * mhz
		case 8, 9:
			return 1176.45 * mhz
		}
	case gnss.Galileo:
		switch sigID {
		case 0, 1:
			return 1575.42 * mhz
		case 3, 4:
			return 1176.45 * mhz
		case 5, 6:
			return 1207.14 * mhz
		}
	case gnss.BeiDou:
		switch sigID {
		case 0, 1:
			return 1561.098 * mhz
		case 2, 3:
			return 1207.14 * mhz
		case 5, 6:
			return 1575.42 * mhz
		case 7, 8:
			return 1176.45 * mhz
		}
	case gnss.SBAS:
		if sigID == 0 {
			return 1575.42 * mhz
		}
	case gnss.GLONASS:
		k := float64(freqID - 7)
		switch sigID {
		case 0:
			return 1602*mhz + k*562500
		case 2:
			return 1246*mhz + k*437500
		}
	}
	return 0
}
