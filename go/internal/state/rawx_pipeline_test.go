package state

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss/iono"
	"github.com/ptudor/gnss/physconst"
	"github.com/ptudor/navlistener/internal/ingest"
)

// Exercise the real byte boundary: constructing RawObs alone cannot catch a
// tracking-status bit mapped to the wrong estimator input.
func TestRAWXPhaseContinuityPipeline(t *testing.T) {
	for _, change := range []string{"unresolved", "phase_invalid", "lock_reset", "clock_reset", "correction_change", "gap", "backward"} {
		t.Run(change, func(t *testing.T) {
			s := New(1)
			now := time.Now()
			emitEpoch := func(epoch int, flags, recStat byte, lock uint16, tow float64) {
				p := make([]byte, 16+64)
				binary.LittleEndian.PutUint64(p, math.Float64bits(tow))
				binary.LittleEndian.PutUint16(p[8:], 2300)
				p[11], p[12], p[13] = 2, recStat, 1
				for i, sig := range []byte{0, 6} {
					freq := 1575.42e6
					if sig == 6 {
						freq = 1176.45e6
					}
					delay := 4.2 * iono.Gamma(1575.42e6, freq)
					m := p[16+i*32:]
					binary.LittleEndian.PutUint64(m, math.Float64bits(2.2e7+delay))
					binary.LittleEndian.PutUint64(m[8:], math.Float64bits((2.2e7-delay)*freq/physconst.SpeedOfLight))
					binary.LittleEndian.PutUint32(m[16:], math.Float32bits(-100))
					m[21], m[22], m[26], m[30] = 7, sig, 42, flags
					binary.LittleEndian.PutUint16(m[24:], lock)
				}
				b := []byte{0xb5, 0x62, 0x02, 0x15, byte(len(p)), byte(len(p) >> 8)}
				b = append(b, p...)
				var a, c byte
				for _, v := range b[2:] {
					a += v
					c += a
				}
				b = append(b, a, c)
				count := 0
				err := ingest.ReplayUBX(bytes.NewReader(b), "rawx", func() time.Time { return now.Add(time.Duration(epoch) * time.Second) }, func(f *ingest.RawFrame) {
					count++
					if f.Obs.PrM <= 0 || f.Obs.DoHz != -100 || f.Obs.CycleSlip || f.Obs.HalfCycleValid != (flags&4 != 0) || f.Obs.HalfCycleSubtracted != (flags&8 != 0) || f.Obs.ClockReset != (recStat&2 != 0) {
						t.Fatalf("incorrect parsed status/code: %+v", f.Obs)
					}
					s.Apply(f)
				}, func(kind string) { t.Fatalf("parse error: %s", kind) })
				if err != nil || count != 2 {
					t.Fatalf("code observations lost: count=%d err=%v", count, err)
				}
			}
			mature := func(want bool) {
				t.Helper()
				sv := s.FeedSVs(now)["G07@0"]
				pr := sv.Perrecv["rawx"]
				if (pr != nil && pr.IonoDelayM != nil) != want {
					t.Fatalf("smoothed result: %+v want mature=%v", pr, want)
				}
				if want && math.Abs(*pr.IonoDelayM-4.2) > 1e-6 {
					t.Fatalf("delay=%v", *pr.IonoDelayM)
				}
			}
			for i := 0; i < iono.MinArc+2; i++ {
				emitEpoch(i, 15, 0, 64500, 100000+float64(i))
				if i == 0 {
					mature(false)
				}
			}
			mature(true) // bit 3 remains set; saturated lock is also continuous.
			epoch := iono.MinArc + 2
			flags, recStat, lock, tow := byte(15), byte(0), uint16(64500), 100000+float64(epoch)
			switch change {
			case "unresolved":
				flags &^= 4
			case "phase_invalid":
				flags &^= 2
			case "lock_reset":
				lock = 0
			case "clock_reset":
				recStat = 2
			case "correction_change":
				flags &^= 8
			case "gap":
				tow += 10
			case "backward":
				tow -= 2
			}
			emitEpoch(epoch, flags, recStat, lock, tow)
			mature(false)
			// Resolve status and accumulate a fresh arc after the discontinuity.
			for i := 1; i <= iono.MinArc+2; i++ {
				emitEpoch(epoch+i, 15, 0, uint16(i*1000), tow+float64(i))
			}
			mature(true)
		})
	}
}

func TestUnknownHalfCycleStatusCannotSmooth(t *testing.T) {
	if !phaseDiscontinuity(obsSample{haveSeen: true, haveCp: true}, obsSample{haveCp: false}) {
		t.Fatal("unknown phase must break arc")
	}
	s := New(1)
	for i := 0; i < iono.MinArc+1; i++ {
		for _, sig := range []int{0, 6} {
			s.Apply(&ingest.RawFrame{Recv: time.Now(), Source: "unknown", SvID: 7, SigID: sig, Obs: &ingest.RawObs{RcvTow: 100000 + float64(i), PrM: 2.2e7, CpCyc: 1e8, CpValid: true}})
		}
	}
	if pr := s.FeedSVs(time.Now())["G07@0"].Perrecv["unknown"]; pr != nil && pr.IonoDelayM != nil {
		t.Fatal("zero half-cycle status accepted")
	}
}
