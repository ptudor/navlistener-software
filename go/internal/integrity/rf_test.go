package integrity

import (
	"slices"
	"testing"
	"time"
)

func TestCn0Uniformity(t *testing.T) {
	prof := DefaultProfile().Cn0Uniformity
	sky := Cn0Fit{Mean: 41, ResidVar: 18, NumSats: 14, PerConstellation: map[int]Cn0Group{0: {Mean: 42, ResidVar: 15, NumSats: 8}, 2: {Mean: 40, ResidVar: 20, NumSats: 6}}}
	if v := cn0Uniformity(sky, prof); v.State != Assured {
		t.Fatalf("genuine sky: %+v", v)
	}
	flat := sky
	flat.PerConstellation = map[int]Cn0Group{6: {Mean: 50, ResidVar: 0.3, NumSats: 6}, 2: {Mean: 49, ResidVar: 0.2, NumSats: 6}, 0: {Mean: 42, ResidVar: 15, NumSats: 8}}
	for range 20 { // the reported constellation must not depend on map order
		v := cn0Uniformity(flat, prof)
		if v.State != Unassured || v.Metrics["tripped_gnss_id"] != 2 || !slices.Contains(v.Reasons, ReasonUniformCn0) {
			t.Fatalf("flat Galileo and GLONASS: %+v, want unassured naming Galileo", v)
		}
	}
	aggregate := Cn0Fit{Mean: 50, ResidVar: 0.5, NumSats: 9}
	if v := cn0Uniformity(aggregate, prof); v.State != Unassured {
		t.Fatalf("flat aggregate: %+v", v)
	}
	if v := cn0Uniformity(Cn0Fit{Mean: 50, ResidVar: 0.1, NumSats: 4}, prof); v.State != Unavailable || v.Reasons[0] != ReasonTooFewSatellites {
		t.Fatalf("four satellites: %+v", v)
	}
}

func TestAGC(t *testing.T) {
	prof := DefaultProfile().AGC
	dep := func(v float64) *float64 { return &v }
	cases := []struct {
		name  string
		bands []RFBand
		want  State
	}{
		{"no bands", nil, Unavailable},
		{"no baseline yet", []RFBand{{AntStatus: 2}}, Unavailable},
		{"quiet", []RFBand{{Departure: dep(30), AntStatus: 2}}, Assured},
		{"departure alone", []RFBand{{Departure: dep(900), AntStatus: 2}}, Inconsistent},
		{"CW tone alone, no baseline", []RFBand{{CWSuppress: 220, AntStatus: 2}}, Inconsistent},
		{"antenna open", []RFBand{{Departure: dep(0), AntStatus: 4}}, Inconsistent},
		{"corroborated departure", []RFBand{{Departure: dep(900), CWSuppress: 220, AntStatus: 2}}, Unassured},
		{"receiver flag corroborates", []RFBand{{Departure: dep(900), JamState: 2, AntStatus: 2}}, Unassured},
		{"gain collapse", []RFBand{{Departure: dep(10), AntStatus: 2}, {Block: 1, Departure: dep(2500), AntStatus: 2}}, Unassured},
	}
	for _, tc := range cases {
		if v := agc(RFSample{Bands: tc.bands}, prof); v.State != tc.want {
			t.Errorf("%s: %+v, want %s", tc.name, v, tc.want)
		}
	}
}

// TestAGCFrontEnds: a Septentrio receiver's front ends are judged on their own dB
// thresholds. A gain cut is a departure like a band's; a gain rise is the antenna path
// giving less, a sign of its own that is never an interference claim.
func TestAGCFrontEnds(t *testing.T) {
	prof := DefaultProfile().AGC
	dep := func(v float64) *float64 { return &v }
	ends := func(deps ...*float64) []RFFrontEnd {
		out := make([]RFFrontEnd, len(deps))
		for i, d := range deps {
			out[i] = RFFrontEnd{ID: i, Departure: d}
		}
		return out
	}
	cases := []struct {
		name    string
		sample  RFSample
		want    State
		reasons []string
	}{
		{"no baseline yet", RFSample{FrontEnds: ends(nil, nil)}, Unavailable, []string{ReasonNoBaseline}},
		{"quiet", RFSample{FrontEnds: ends(dep(0.5), dep(-1), nil)}, Assured, nil},
		{"just inside both thresholds", RFSample{FrontEnds: ends(dep(3.9), dep(-3.9))}, Assured, nil},
		{"cut alone", RFSample{FrontEnds: ends(dep(4), dep(0))}, Inconsistent, []string{ReasonAGCDeparture}},
		{"cut with a simultaneous drop", RFSample{FrontEnds: ends(dep(6)), Cn0Drop: true}, Unassured, []string{ReasonAGCDeparture, ReasonSimultaneousDrop}},
		{"cut with a neighbour", RFSample{FrontEnds: ends(dep(6)), Neighbours: 2}, Unassured, []string{ReasonAGCDeparture, ReasonNeighbourInterference}},
		{"collapse", RFSample{FrontEnds: ends(dep(1), dep(10))}, Unassured, []string{ReasonAGCCollapse}},
		{"rise alone", RFSample{FrontEnds: ends(dep(-4), dep(0))}, Inconsistent, []string{ReasonGainRise}},
		{"a rise is never a collapse", RFSample{FrontEnds: ends(dep(-30))}, Inconsistent, []string{ReasonGainRise}},
		{"a rise is not corroborated into interference", RFSample{FrontEnds: ends(dep(-12)), Cn0Drop: true, Neighbours: 3}, Inconsistent, []string{ReasonGainRise}},
		{"cut on one front end, rise on another", RFSample{FrontEnds: ends(dep(5), dep(-5))}, Inconsistent, []string{ReasonAGCDeparture, ReasonGainRise}},
		{"collapse beside a rise", RFSample{FrontEnds: ends(dep(11), dep(-5))}, Unassured, []string{ReasonAGCCollapse, ReasonGainRise}},
	}
	for _, tc := range cases {
		v := agc(tc.sample, prof)
		if v.State != tc.want || !slices.Equal(v.Reasons, tc.reasons) {
			t.Errorf("%s: %s %v, want %s %v (%+v)", tc.name, v.State, v.Reasons, tc.want, tc.reasons, v)
		}
	}

	v := agc(RFSample{FrontEnds: ends(dep(6), dep(-2), nil)}, prof)
	if v.Metrics["front_ends"] != 3 || v.Metrics["max_gain_cut_db"] != 6 || v.Metrics["max_gain_rise_db"] != 2 || v.Metrics["bands"] != 0 ||
		v.Thresholds["gain_departure_db"] != DefaultGainDepartureDB || v.Thresholds["gain_departure_severe_db"] != DefaultGainDepartureSevereDB {
		t.Fatalf("front-end verdict evidence: %+v", v)
	}
	if _, has := v.Metrics["max_departure"]; has {
		t.Fatalf("a front-end verdict reports an AGC count departure: %+v", v.Metrics)
	}
	// A u-blox verdict carries no front-end evidence or thresholds.
	u := agc(RFSample{Bands: []RFBand{{Departure: dep(900), AntStatus: 2}}}, prof)
	for _, key := range []string{"front_ends", "max_gain_cut_db", "max_gain_rise_db"} {
		if _, has := u.Metrics[key]; has {
			t.Fatalf("a band verdict reports %s: %+v", key, u.Metrics)
		}
	}
	if _, has := u.Thresholds["gain_departure_db"]; has {
		t.Fatalf("a band verdict reports a gain threshold: %+v", u.Thresholds)
	}
	// Each scale is judged on its own thresholds: 900 dB would be absurd, and 6 counts
	// is nothing.
	if w := agc(RFSample{Bands: []RFBand{{Departure: dep(6), AntStatus: 2}}}, prof); w.State != Assured {
		t.Fatalf("a 6-count AGC departure: %+v", w)
	}
}

// TestAGCGainProfileValidated: the gain thresholds are operating points like the rest.
func TestAGCGainProfileValidated(t *testing.T) {
	p := DefaultProfile()
	if p.AGC.GainDepartureDB != 4 || p.AGC.GainDepartureSevereDB != 10 {
		t.Fatalf("default gain thresholds = %g/%g dB", p.AGC.GainDepartureDB, p.AGC.GainDepartureSevereDB)
	}
	for name, edit := range map[string]func(*Profile){
		"zero departure":        func(p *Profile) { p.AGC.GainDepartureDB = 0 },
		"severe below ordinary": func(p *Profile) { p.AGC.GainDepartureSevereDB = 3 },
	} {
		bad := DefaultProfile()
		edit(&bad)
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: profile accepted", name)
		}
	}
}

// TestBaselineRelearnProfileValidated: the self-healing hold is a day by default; zero
// means a gain cut is never learned; anything shorter than the fifteen minutes a band
// above its baseline has always taken is refused, and so is a negative hold.
func TestBaselineRelearnProfileValidated(t *testing.T) {
	if got := DefaultProfile().AGC.BaselineRelearnAfter; got != 24*time.Hour {
		t.Fatalf("default hold = %s", got)
	}
	for hold, valid := range map[time.Duration]bool{
		0: true, MinBaselineRelearnAfter: true, time.Hour: true, 30 * 24 * time.Hour: true,
		MinBaselineRelearnAfter - time.Second: false, time.Nanosecond: false, -time.Hour: false,
	} {
		p := DefaultProfile()
		p.AGC.BaselineRelearnAfter = hold
		if err := p.Validate(); (err == nil) != valid {
			t.Errorf("hold %s: err = %v, want valid %v", hold, err, valid)
		}
	}
}

func TestReceiverSpoofing(t *testing.T) {
	for state, want := range map[int]State{SpoofUnknown: Unavailable, SpoofNone: Assured, SpoofIndicated: Unassured, SpoofMultiple: Unassured, 9: Unavailable} {
		if v := receiverSpoofing(state); v.State != want {
			t.Errorf("state %d: %+v, want %s", state, v, want)
		}
	}
}
