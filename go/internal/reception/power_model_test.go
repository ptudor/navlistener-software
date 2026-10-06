package reception

import (
	"bytes"
	"testing"
)

func modelSite() Site {
	return Site{Observer: "edge", Position: []float64{37, -122, 10}, Signals: []string{"0:0"},
		PowerModelEpoch: "antenna-1", PowerMinDeviation: 6, PowerMADMultiplier: 4, PowerMinSupport: 3}
}

func TestPowerModelNeedsDistinctPassesAndRejectsPoison(t *testing.T) {
	m := NewPowerModel(modelSite())
	baseTime := int64(1_800_000_000)
	_, bin := powerPhase(baseTime)
	phase := int64(bin)*PowerPhaseSeconds + PowerPhaseSeconds/2
	cycle := baseTime / SiderealSeconds
	at := func(day int) int64 { return (cycle+int64(day))*SiderealSeconds + phase }
	o := PowerObservation{GNSS: 0, SV: 12, Signal: Satellite, CN0: 40}
	for day, cn0 := range []uint8{40, 42, 41} {
		o.CN0 = cn0
		for sample := 0; sample < 10; sample++ {
			if !m.Observe(at(day)+int64(sample), o) {
				t.Fatalf("clean sample rejected on day %d", day)
			}
		}
	}
	base := Expectation{ID: 9, Issued: at(3) - SlotSeconds/2,
		Entries: []Entry{{GNSS: 0, SV: 12, Signal: Satellite, Slots: 1}}}
	if p := m.Forecast(base); p.Entries[0].Valid != 0 {
		t.Fatal("only two finalized passes became usable")
	}
	// The fourth cycle finalizes day 2 and makes three distinct passes.
	o.CN0 = 41
	if !m.Observe(at(3), o) {
		t.Fatal("clean fourth pass rejected")
	}
	p := m.Forecast(base)
	if !p.Valid() || p.Entries[0].Valid&1 == 0 || p.Entries[0].Expected[0] != 41 || p.Entries[0].Support[0] != 3 {
		t.Fatalf("mature forecast: %+v", p)
	}
	before := m.ModelID()
	o.CN0 = 60
	if m.Observe(at(3)+1, o) {
		t.Fatal("mature-model outlier was learned")
	}
	if m.ModelID() != before {
		t.Fatal("rejected sample changed model generation")
	}
	if m.Observe(at(2), PowerObservation{GNSS: 0, SV: 12, Signal: Satellite, CN0: 41}) {
		t.Fatal("out-of-order prior cycle was learned")
	}
}

func TestPowerModelPersistenceIsDeterministicAndBound(t *testing.T) {
	site := modelSite()
	m := NewPowerModel(site)
	for day := 0; day < 5; day++ {
		for sv := uint8(1); sv <= 3; sv++ {
			m.Observe(1_800_000_000+int64(day)*SiderealSeconds, PowerObservation{GNSS: 0, SV: sv, Signal: Satellite, CN0: 35 + sv})
		}
	}
	b, err := m.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	restored := NewPowerModel(site)
	if err := restored.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	again, _ := restored.MarshalBinary()
	if !bytes.Equal(b, again) || restored.ModelID() != m.ModelID() {
		t.Fatal("model did not round-trip byte-for-byte")
	}
	wrong := modelSite()
	wrong.PowerModelEpoch = "new-antenna"
	if NewPowerModel(wrong).UnmarshalBinary(b) == nil {
		t.Fatal("model crossed a configuration epoch")
	}
	bad := append([]byte(nil), b...)
	bad[4]++
	if NewPowerModel(site).UnmarshalBinary(bad) == nil {
		t.Fatal("unknown model version accepted")
	}
}

func TestPowerModelConfigValidation(t *testing.T) {
	c := Config{Stations: []Site{{Observer: "edge", Position: []float64{0, 0, 0}, Signals: []string{"0:0"}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s := c.Stations[0]
	if s.PowerModelEpoch != "1" || s.PowerMinDeviation != 6 || s.PowerMADMultiplier != 4 || s.PowerMinSupport != 3 {
		t.Fatalf("power defaults: %+v", s)
	}
	c.Stations[0].PowerModelEpoch = "bad epoch"
	if err := c.Validate(); err == nil {
		t.Fatal("unsafe model epoch accepted")
	}
}
