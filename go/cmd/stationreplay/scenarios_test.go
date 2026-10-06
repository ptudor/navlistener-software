package main

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
	"github.com/ptudor/navlistener/internal/state"
)

// Scenario fixtures (docs/proposals/STATION-ASSURANCE.md §7, item 2.5): synthetic,
// deterministic streams of everything a fixed ESP32 observer sends each second — the
// receiver solution, NAV-SAT, MON-RF and the board's PPS/RTC timing — run through the
// same state, checks and detectors as the collector. Each scenario changes the stream
// at onset and states which station events must and must not be confirmed. They show
// the checks behave as designed on the conditions they are meant to separate; they are
// not evidence of detection performance against real interference.

// epoch is one second of a station's inputs before encoding.
type epoch struct {
	east, north, up float64 // antenna offset from the survey, m
	velN            float64 // m/s
	fixOK           bool
	bias            float64 // ns
	sinceStart      uint32  // ms
	utcError        time.Duration
	spoof           uint8
	cn0             func(sv int, elevDeg int) int
	agc, cw, ant    int
	phaseStepNs     float64
	ppsValid        bool
	drop            bool
}

const (
	scenarioTOW0   = 345_600_000
	scenarioUptime = 3_600_000
	scenarioLat    = 37.4219
	scenarioLon    = -122.0841
	scenarioHeight = 12.5
)

var scenarioT0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func naturalCn0(sv, elevDeg int) int { return 30 + elevDeg/5 + (sv*7)%6 }

func baseEpoch(i int) epoch {
	return epoch{fixOK: true, bias: 1000 + 25*float64(i), sinceStart: uint32(5_000_000 + i*1000), spoof: 1,
		cn0: naturalCn0, agc: 4000 + i%5, ant: 2, ppsValid: true}
}

// frames encodes one second of inputs, with the board's sequence and uptime.
func (e epoch) frames(i int) []*ingest.RawFrame {
	if e.drop {
		return nil
	}
	local := scenarioT0.Add(time.Duration(i) * time.Second)
	stamp := local.Add(-150 * time.Millisecond)
	utc := local.Add(-400*time.Millisecond + e.utcError).UTC()
	tow := uint32(scenarioTOW0 + i*1000)
	// Offsets in metres to 1e-7 degree latitude/longitude, adequate at these sizes.
	latE7 := int32(math.Round((scenarioLat + e.north/111320) * 1e7))
	lonE7 := int32(math.Round((scenarioLon + e.east/(111320*math.Cos(scenarioLat*math.Pi/180))) * 1e7))
	flags, fixType := uint8(ingest.FixFlagOK), uint8(3)
	if !e.fixOK {
		flags, fixType = 0, 0
	}
	sol := &ingest.ReceiverSolution{
		PVT: &ingest.SolutionPVT{TOWMS: tow, Year: uint16(utc.Year()), Month: uint8(utc.Month()), Day: uint8(utc.Day()),
			Hour: uint8(utc.Hour()), Minute: uint8(utc.Minute()), Second: uint8(utc.Second()), NanoNS: int32(utc.Nanosecond()),
			UTCValid: 7, TAccNS: 25, FixType: fixType, FixFlags: flags, NumSV: 15, LatE7: latE7, LonE7: lonE7,
			HeightMM: int32(math.Round((scenarioHeight + e.up) * 1000)), HAccMM: 1500, VAccMM: 2500,
			VelNMMS: int32(e.velN * 1000), SAccMMS: 90},
		Clock:  &ingest.SolutionClock{TOWMS: tow, BiasNS: int32(math.Round(e.bias)), DriftNSS: 25, TAccNS: 25},
		Status: &ingest.SolutionStatus{TOWMS: tow, FixType: fixType, SpoofState: e.spoof, SinceStart: e.sinceStart},
	}
	push := func(f *ingest.RawFrame) *ingest.RawFrame {
		f.Source, f.Recv, f.RecvLocal, f.RecvStamped = "scenario", stamp, local, true
		return f
	}
	var sats []ingest.SatCN0
	for sv := 1; sv <= 16; sv++ {
		gnss, elev := 0, 8+(sv*37)%80
		if sv > 9 {
			gnss = 2
		}
		sats = append(sats, ingest.SatCN0{GnssID: gnss, SvID: sv, Cn0: e.cn0(sv, elev), ElevDeg: elev, Used: true})
	}
	// RTC 12 ppm fast against GNSS, wrapped to ±0.5 s, plus any injected step.
	var phase *float64
	if e.ppsValid {
		p := math.Remainder(0.21e9+12_000*float64(i)+e.phaseStepNs, 1e9)
		phase = &p
	}
	board := &ingest.ObserverDetails{Version: 1, Reason: 1, UptimeMS: uint64(scenarioUptime + i*1000),
		Timing: &ingest.BoardTiming{Clock: "esp_apb", RTCState: "enabled_1hz", PhaseNS: phase}}
	return []*ingest.RawFrame{
		push(&ingest.RawFrame{MsgType: ingest.TelemReceiverSolution, Solution: sol}),
		push(&ingest.RawFrame{MsgType: ingest.TelemReceptionData, RF: &ingest.RawRF{Sats: sats}}),
		push(&ingest.RawFrame{MsgType: ingest.TelemJammingStats, RF: &ingest.RawRF{Bands: []ingest.RFBand{
			{Block: 0, AGC: e.agc, CWSuppress: e.cw, JamState: 1, AntStatus: e.ant}}}}),
		push(&ingest.RawFrame{Details: board, Session: "boot", Seq: uint64(i), HasSeq: true}),
	}
}

type scenario struct {
	name    string
	seconds int
	onset   int
	change  func(i int, e *epoch)
	// want are events that must be confirmed after onset, as "type=new_value";
	// forbidden must not be confirmed at all.
	want, forbidden []string
}

func runScenario(t *testing.T, sc scenario) map[string]bool {
	t.Helper()
	cfg, err := state.NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{
		"scenario": {Mode: integrity.ModeFixed, Position: &integrity.Surveyed{LatDeg: scenarioLat, LonDeg: scenarioLon, HeightM: scenarioHeight}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newReplayer(cfg, 15*time.Second, func(record) {})
	for i := 0; i < sc.seconds; i++ {
		e := baseEpoch(i)
		if i >= sc.onset && sc.change != nil {
			sc.change(i, &e)
		}
		for _, f := range e.frames(i) {
			r.apply(f)
		}
	}
	r.finish(scenarioT0.Add(time.Duration(sc.seconds) * time.Second))
	seen := map[string]bool{}
	onset := scenarioT0.Add(time.Duration(sc.onset) * time.Second)
	for _, e := range r.events {
		key := e.Type + "=" + e.NewValue
		for _, f := range sc.forbidden {
			if f == key || f == e.Type {
				t.Errorf("%s: forbidden event %s at %s (%s)", sc.name, key, e.Time.Sub(scenarioT0), e.Message)
			}
		}
		if !e.Time.Before(onset) {
			seen[key] = true
		}
	}
	for _, w := range sc.want {
		if !seen[w] {
			t.Errorf("%s: %s not confirmed after onset; events %v", sc.name, w, eventList(r))
		}
	}
	return seen
}

func eventList(r *replayer) []string {
	var out []string
	for _, e := range r.events {
		out = append(out, fmt.Sprintf("%s %s %s->%s", e.Time.Sub(scenarioT0), e.Type, e.OldValue, e.NewValue))
	}
	return out
}

// spoofingOrUnassured are the attack and corroborated-distrust outcomes a benign
// scenario must never produce.
var spoofingOrUnassured = []string{"spoofing_suspected", "station_assurance=unassured", "jamming_detected"}

func TestScenarios(t *testing.T) {
	const warm = 15 * 60 // every check, the AGC baseline and the ten-minute mean are established
	scenarios := []scenario{
		{name: "normal open sky", seconds: warm + 600, onset: warm,
			forbidden: append([]string{"station_assurance=inconsistent", "station_rf_degraded", "antenna_fault"}, spoofingOrUnassured...)},
		{name: "receiver reset", seconds: warm + 600, onset: warm,
			change: func(i int, e *epoch) {
				// The receiver restarts: its uptime and clock start over.
				e.sinceStart = uint32(2000 + (i-warm)*1000)
				e.bias = 400 + 25*float64(i-warm)
			},
			forbidden: append([]string{"station_assurance=inconsistent"}, spoofingOrUnassured...)},
		{name: "millisecond clock adjustment", seconds: warm + 600, onset: warm,
			change:    func(i int, e *epoch) { e.bias -= 1e6 },
			forbidden: append([]string{"station_assurance=inconsistent"}, spoofingOrUnassured...)},
		{name: "loss and reacquisition", seconds: warm + 900, onset: warm,
			change: func(i int, e *epoch) {
				if i < warm+120 {
					e.fixOK, e.ppsValid = false, false
					e.phaseStepNs = 40_000 // a free-running pulse while the fix is lost
				}
			},
			forbidden: spoofingOrUnassured},
		{name: "slow position drift", seconds: warm + 1800, onset: warm,
			change:    func(i int, e *epoch) { e.east = 0.02 * float64(i-warm) }, // 36 m in 30 minutes
			want:      []string{"station_assurance=inconsistent"},
			forbidden: spoofingOrUnassured},
		{name: "position step", seconds: warm + 600, onset: warm,
			change:    func(i int, e *epoch) { e.north = 300 },
			want:      []string{"station_assurance=inconsistent"},
			forbidden: spoofingOrUnassured},
		{name: "time step", seconds: warm + 600, onset: warm,
			change: func(i int, e *epoch) {
				// Receiver UTC, receiver clock and the PPS all step by three seconds and
				// 250 µs: a time takeover.
				e.utcError = 3*time.Second + 250*time.Microsecond
				e.bias += 250_000
				e.phaseStepNs = 250_000
			},
			want:      []string{"spoofing_suspected=suspected", "station_assurance=unassured"},
			forbidden: []string{"jamming_detected"}},
		{name: "uniform C/N0", seconds: warm + 600, onset: warm,
			change:    func(i int, e *epoch) { e.cn0 = func(int, int) int { return 48 } },
			want:      []string{"station_assurance=inconsistent"},
			forbidden: spoofingOrUnassured},
		{name: "uniform C/N0 with the receiver's own spoofing flag", seconds: warm + 600, onset: warm,
			change: func(i int, e *epoch) {
				e.cn0 = func(int, int) int { return 48 }
				e.spoof = 2
			},
			want: []string{"spoofing_suspected=suspected"}},
		{name: "position takeover with time", seconds: warm + 600, onset: warm,
			change: func(i int, e *epoch) {
				e.north = 2000
				e.bias += 3000
				e.phaseStepNs = 3000
			},
			want: []string{"spoofing_suspected=suspected", "station_assurance=unassured"}},
		{name: "AGC compression", seconds: warm + 600, onset: warm,
			change:    func(i int, e *epoch) { e.agc = 1500 },
			want:      []string{"jamming_detected=crit"},
			forbidden: []string{"spoofing_suspected"}},
		{name: "narrowband interference", seconds: warm + 600, onset: warm,
			change: func(i int, e *epoch) {
				e.cw = 230
				e.agc = 3100
			},
			want:      []string{"jamming_detected=warn"},
			forbidden: []string{"spoofing_suspected"}},
		{name: "antenna disconnect", seconds: warm + 600, onset: warm,
			change: func(i int, e *epoch) {
				e.ant = 4
				e.cn0 = func(int, int) int { return 0 }
			},
			want:      []string{"antenna_fault=fault"},
			forbidden: []string{"spoofing_suspected"}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) { runScenario(t, sc) })
	}
}

// TestScenarioDuplicateAndOutOfOrderInputs: a spool replay delivers seconds again and
// out of order; the checks ignore what they have already seen, so a normal sky stays
// quiet.
func TestScenarioDuplicateAndOutOfOrderInputs(t *testing.T) {
	cfg, err := state.NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{
		"scenario": {Mode: integrity.ModeFixed, Position: &integrity.Surveyed{LatDeg: scenarioLat, LonDeg: scenarioLon, HeightM: scenarioHeight}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newReplayer(cfg, 15*time.Second, func(record) {})
	for i := 0; i < 1500; i++ {
		for _, f := range baseEpoch(i).frames(i) {
			r.apply(f)
		}
		if i > 30 && i%50 == 0 {
			// Ten seconds of earlier solutions arrive again, now.
			for j := i - 10; j < i; j++ {
				f := baseEpoch(j).frames(j)[0]
				f.RecvLocal = scenarioT0.Add(time.Duration(i) * time.Second)
				r.apply(f)
			}
		}
	}
	r.finish(scenarioT0.Add(1500 * time.Second))
	for _, e := range r.events {
		if e.Type != "station_assurance" || e.NewValue != "assured" {
			t.Errorf("replayed inputs produced %s %s->%s", e.Type, e.OldValue, e.NewValue)
		}
	}
}
