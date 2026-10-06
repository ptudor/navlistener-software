package reception

import (
	"bytes"
	"testing"
)

func powerFixture() PowerExpectation {
	p := PowerExpectation{
		ExpectationID: 42,
		ModelID:       77,
		SiteID:        88,
		Issued:        1_800_000_000,
		MinDeviation:  6,
		MADMultiplier: 4,
		MinSupport:    3,
		Entries:       make([]PowerEntry, 4),
	}
	for i := range p.Entries {
		p.Entries[i].Valid = 0x1f
		for slot := 0; slot < Slots; slot++ {
			p.Entries[i].Expected[slot] = uint8(38 + i + slot)
			p.Entries[i].MAD[slot] = uint8(1 + i%2)
			p.Entries[i].Support[slot] = uint8(3 + slot)
		}
	}
	return p
}

func TestPowerExpectationRoundTrip(t *testing.T) {
	want := powerFixture()
	b, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != PowerHeaderSize+PowerEntrySize*len(want.Entries) || len(b) > PowerMaxSize {
		t.Fatalf("wire length %d", len(b))
	}
	got, err := DecodePowerExpectation(b)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := got.Encode()
	if !bytes.Equal(b, again) {
		t.Fatalf("round trip differs\n%x\n%x", b, again)
	}

	for name, mutate := range map[string]func([]byte){
		"version":         func(v []byte) { v[0]++ },
		"slot count":      func(v []byte) { v[1]-- },
		"reserved":        func(v []byte) { v[39] = 1 },
		"support maximum": func(v []byte) { v[38] = PowerMaxSupport + 1 },
		"valid tail":      func(v []byte) { v[PowerHeaderSize] = 0x80 },
		"false maturity":  func(v []byte) { v[PowerHeaderSize+11] = 2 },
		"bad cno":         func(v []byte) { v[PowerHeaderSize+1] = 100 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := append([]byte(nil), b...)
			mutate(bad)
			if _, err := DecodePowerExpectation(bad); err == nil {
				t.Fatal("malformed expectation accepted")
			}
		})
	}
	if _, err := DecodePowerExpectation(b[:len(b)-1]); err == nil {
		t.Fatal("truncated expectation accepted")
	}
}

func TestPowerSampleRoundTripAndComparison(t *testing.T) {
	p := powerFixture()
	s := PowerSample{
		Count:         uint8(len(p.Entries)),
		Flags:         PowerFlagLocal | PowerFlagRemote,
		ExpectationID: p.ExpectationID,
		RemoteModelID: p.ModelID,
		LocalModelID:  88,
		Unix:          p.Issued + 65,
		UptimeMS:      1234,
	}
	for i := range p.Entries {
		s.ObservedValid[i/8] |= 1 << uint(i%8)
		s.Observed[i] = p.Entries[i].Expected[1]
		s.LocalAssessment.Valid[i/8] |= 1 << uint(i%8)
	}
	s.Observed[1] += 8 // MAD 2 => threshold 8; the boundary is anomalous.
	s.Observed[2] += 5 // floor threshold is 6; remains normal.
	s.LocalValid = 1
	s.RemoteValid = 1
	b, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != PowerSampleSize {
		t.Fatalf("sample length %d", len(b))
	}
	got, err := DecodePowerSample(b)
	if err != nil {
		t.Fatal(err)
	}
	a := ComparePower(p, got)
	if a.Valid[0] != 0x0f || a.Bad[0] != 0x02 {
		t.Fatalf("comparison valid=%08b bad=%08b", a.Valid[0], a.Bad[0])
	}
	got.RemoteAssessment = a
	got.RemoteAlarm = 1
	if _, err := got.Encode(); err != nil {
		t.Fatal(err)
	}

	bad := append([]byte(nil), b...)
	bad[7] = 1
	if _, err := DecodePowerSample(bad); err == nil {
		t.Fatal("reserved byte accepted")
	}
	bad = append([]byte(nil), b...)
	bad[208] = 1 // local bad without local valid for entry zero is illegal here only after clearing validity.
	bad[192] = 0
	if _, err := DecodePowerSample(bad); err == nil {
		t.Fatal("bad bit outside valid coverage accepted")
	}
	bad = append([]byte(nil), b...)
	for i := 16; i < 24; i++ {
		bad[i] = 0
	}
	if _, err := DecodePowerSample(bad); err == nil {
		t.Fatal("remote capability accepted without a model identity")
	}
	bad = append([]byte(nil), b...)
	bad[3] = 1 << 4
	if _, err := DecodePowerSample(bad); err == nil {
		t.Fatal("reserved constellation accepted in aggregate mask")
	}
	bad = append([]byte(nil), b...)
	bad[48] = 0
	if _, err := DecodePowerSample(bad); err == nil {
		t.Fatal("model coverage accepted without an observation")
	}
}

func TestCountPowerUsesConstellationQuorum(t *testing.T) {
	base := Expectation{MinExpected: 4, MinMissing: 3, MissingPercent: 50}
	for i := 0; i < 5; i++ {
		base.Entries = append(base.Entries, Entry{GNSS: 0, SV: uint8(i + 1), Signal: Satellite, Slots: 31})
	}
	base.Entries = append(base.Entries, Entry{GNSS: 2, SV: 1, Signal: Satellite, Slots: 31})
	var a PowerAssessment
	a.Valid[0] = 0x3f
	a.Bad[0] = 0x07
	modeled, anomalous, valid, bad := CountPower(base, 6, a)
	if modeled[0] != 5 || anomalous[0] != 3 || valid != 1 || bad != 1 {
		t.Fatalf("counts modeled=%v anomalous=%v valid=%08b bad=%08b", modeled, anomalous, valid, bad)
	}
	if modeled[2] != 1 || anomalous[2] != 0 {
		t.Fatalf("unexpected secondary constellation counts: %v %v", modeled, anomalous)
	}
	if _, _, v, b := CountPower(base, 5, a); v != 0 || b != 0 {
		t.Fatal("accepted assessment bound to a different entry count")
	}
}

func TestPowerEventRoundTrip(t *testing.T) {
	want := PowerEvent{Flags: PowerFlagLocal | PowerFlagRemote, LocalValid: 1, LocalAlarm: 1,
		RemoteValid: 1, RemoteAlarm: 1, JointValid: 1, JointAlarm: 1, ExpectationID: 7,
		RemoteModelID: 8, LocalModelID: 9, Unix: 1_800_000_000, UptimeMS: 1234,
		Boot: 2, Event: 3, LocalAbnormal: 1, RemoteAbnormal: 1, JointAbnormal: 1}
	b, err := want.Encode()
	if err != nil || len(b) != PowerEventSize {
		t.Fatalf("encode: %d %v", len(b), err)
	}
	got, err := DecodePowerEvent(b)
	if err != nil || got != want {
		t.Fatalf("decode: %+v %v", got, err)
	}
	bad := append([]byte(nil), b...)
	bad[1] = 0
	if _, err := DecodePowerEvent(bad); err == nil {
		t.Fatal("reference fields accepted without capability flags")
	}
	invalid := want
	invalid.JointValid = 2
	if _, err := invalid.Encode(); err == nil {
		t.Fatal("joint coverage outside local and remote coverage accepted")
	}
	invalid = want
	invalid.ModelConflict = 2
	if _, err := invalid.Encode(); err == nil {
		t.Fatal("model conflict outside joint coverage accepted")
	}
}
