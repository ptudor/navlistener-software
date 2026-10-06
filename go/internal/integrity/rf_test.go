package integrity

import (
	"slices"
	"testing"
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

func TestReceiverSpoofing(t *testing.T) {
	for state, want := range map[int]State{SpoofUnknown: Unavailable, SpoofNone: Assured, SpoofIndicated: Unassured, SpoofMultiple: Unassured, 9: Unavailable} {
		if v := receiverSpoofing(state); v.State != want {
			t.Errorf("state %d: %+v, want %s", state, v, want)
		}
	}
}
