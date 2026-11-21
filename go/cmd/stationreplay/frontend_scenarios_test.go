package main

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
	"github.com/ptudor/navlistener/internal/state"
)

// Septentrio scenarios: a station whose stored inputs are ReceiverStatus blocks, replayed
// from the rows the collector keeps them in. Like the scenarios above they show the
// classifiers separate the conditions they are meant to; they are synthetic, built on the
// gains a mosaic-G5 P3 reported on a bench, and say nothing about detection performance.

// The nine front ends that receiver reports, in its order, with their quiet gains in dB
// behind an L1/L5 antenna: the front ends that antenna does not pass sit highest.
var p3FrontEnds = []struct {
	id   uint8
	gain int
}{{5, 36}, {6, 46}, {3, 51}, {4, 50}, {12, 50}, {2, 51}, {0, 40}, {1, 44}, {11, 41}}

// testCRC16 is the CRC-16-CCITT of an SBF block (polynomial 0x1021, zero start), written
// out here so the fixture does not borrow the code it feeds.
func testCRC16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// p3Status is one ReceiverStatus block (4014, revision 1) at second i, with each gain
// moved by adjust.
func p3Status(i int, adjust func(id uint8, gain int) int) []byte {
	block := make([]byte, 8+24+4*len(p3FrontEnds))
	block[0], block[1] = '$', '@'
	binary.LittleEndian.PutUint16(block[4:], 1<<13|ingest.SBFReceiverStatusBlock)
	binary.LittleEndian.PutUint16(block[6:], uint16(len(block)))
	body := block[8:]
	binary.LittleEndian.PutUint32(body[0:], uint32(scenarioTOW0+i*1000))
	binary.LittleEndian.PutUint16(body[4:], 2445)
	body[6] = 5                                            // CPU load, %
	binary.LittleEndian.PutUint32(body[8:], uint32(600+i)) // uptime, s
	binary.LittleEndian.PutUint32(body[12:], 0x70)         // WNSET, TOWSET, FINETIME
	body[20], body[21], body[22], body[23] = uint8(len(p3FrontEnds)), 4, 29, 127
	for n, fe := range p3FrontEnds {
		// A quiet gain moves a decibel either way.
		gain := fe.gain + (i/7+n)%3 - 1
		if adjust != nil {
			gain = adjust(fe.id, gain)
		}
		sub := body[24+4*n:]
		sub[0], sub[1], sub[2], sub[3] = fe.id, uint8(int8(gain)), 100, 0
	}
	binary.LittleEndian.PutUint16(block[2:], testCRC16(block[4:]))
	return block
}

func TestFrontEndScenarios(t *testing.T) {
	const warm = 15 * 60 // the gain baselines are established
	shift := func(by int, ids ...uint8) func(uint8, int) int {
		return func(id uint8, gain int) int {
			for _, want := range ids {
				if id == want {
					return gain + by
				}
			}
			if len(ids) == 0 {
				return gain + by
			}
			return gain
		}
	}
	// The front ends an L1/L5 antenna passes: the ones that rise when it is unplugged.
	passed := []uint8{5, 0, 1, 11}
	scenarios := []struct {
		name    string
		seconds int
		// until is the second the change ends at; zero keeps it to the end.
		until           int
		adjust          func(id uint8, gain int) int
		want, forbidden []string
		// last is the state each named machine must end in, as "type=new_value".
		last []string
	}{
		{name: "quiet", seconds: warm + 900,
			forbidden: []string{"station_rf_degraded", "jamming_detected", "antenna_fault", "spoofing_suspected"}},
		{name: "noise on L1 alone", seconds: warm + 600, adjust: shift(-6, 0),
			// One front end 6 dB down with nothing corroborating it: a degradation.
			want:      []string{"station_rf_degraded=degraded"},
			forbidden: []string{"jamming_detected", "antenna_fault", "spoofing_suspected"}},
		{name: "broadband noise on every front end", seconds: warm + 600, adjust: shift(-12),
			// What a noisy bench did to this receiver, and more: a gain collapse.
			want:      []string{"jamming_detected=crit"},
			forbidden: []string{"station_rf_degraded", "antenna_fault", "spoofing_suspected"}},
		{name: "the antenna's amplifier stops", seconds: warm + 600, adjust: shift(12, passed...),
			// The front ends the antenna passes all rise: less power, not interference.
			want:      []string{"station_rf_degraded=degraded"},
			forbidden: []string{"jamming_detected", "antenna_fault", "spoofing_suspected"}},
		{name: "the antenna is unplugged for an hour", seconds: warm + 3600 + 900, until: warm + 3600, adjust: shift(12, passed...),
			// The rise is reported while it lasts. Nothing is learned from an hour of
			// it, so plugging the antenna back in is a return to the baseline: the
			// degradation clears, and the reconnection is never read as a gain collapse.
			want:      []string{"station_rf_degraded=degraded", "station_rf_degraded=ok"},
			forbidden: []string{"jamming_detected", "antenna_fault", "spoofing_suspected"},
			last:      []string{"station_rf_degraded=ok"}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			cfg, err := state.NewIntegrityConfig(integrity.DefaultProfile(), nil)
			if err != nil {
				t.Fatal(err)
			}
			r := newReplayer(cfg, 15*time.Second, func(record) {})
			for i := 0; i < sc.seconds; i++ {
				var adjust func(uint8, int) int
				if i >= warm && (sc.until == 0 || i < sc.until) {
					adjust = sc.adjust
				}
				local := scenarioT0.Add(time.Duration(i) * time.Second)
				stamp := local.Add(-150 * time.Millisecond)
				// As the stored row gives it back: the whole block and its clocks.
				f, err := ingest.StoredFrame(ingest.StoredInput{Origin: "rf", Kind: "receiver_status", Raw: p3Status(i, adjust),
					Source: "p3", Local: local, WallClock: &stamp, Session: "boot", Seq: uint64(i + 1), HasSeq: true})
				if err != nil {
					t.Fatalf("second %d: %v", i, err)
				}
				r.apply(f)
			}
			r.finish(scenarioT0.Add(time.Duration(sc.seconds) * time.Second))
			seen, final := map[string]bool{}, map[string]string{}
			onset := scenarioT0.Add(warm * time.Second)
			for _, e := range r.events {
				key := e.Type + "=" + e.NewValue
				for _, f := range sc.forbidden {
					if f == key || f == e.Type {
						t.Errorf("forbidden event %s at %s (%s)", key, e.Time.Sub(scenarioT0), e.Message)
					}
				}
				if !e.Time.Before(onset) {
					seen[key] = true
				}
				final[e.Type] = key
				if strings.Contains(e.Message, "relearned") {
					t.Errorf("%s at %s says a baseline was relearned: %s", key, e.Time.Sub(scenarioT0), e.Message)
				}
			}
			for _, w := range sc.want {
				if !seen[w] {
					t.Errorf("%s not confirmed after onset; events %v", w, eventList(r))
				}
			}
			for _, w := range sc.last {
				if typ, _, _ := strings.Cut(w, "="); final[typ] != w {
					t.Errorf("%s ended as %q, want %s; events %v", typ, final[typ], w, eventList(r))
				}
			}
		})
	}
}
