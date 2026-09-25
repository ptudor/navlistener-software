package ingest

import (
	"encoding/hex"
	"math"
	"os"
	"strings"
	"testing"
)

// The MAX fixture is the NEO golden report followed by tags 11-13, built from the
// documented layout (docs/OBSERVER-TELEMETRY.md) and shared with the C encoder test.
func observerMAXGolden(t testing.TB) []byte {
	t.Helper()
	text, err := os.ReadFile("../../../testdata/observer_details_max_v1.hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Offsets of the three bodies within the fixture.
const (
	barometerBody    = 258 + 3
	thermocoupleBody = barometerBody + 10 + 3
	motionBody       = thermocoupleBody + 12 + 3
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestObserverDetailsMAXGolden(t *testing.T) {
	b := observerMAXGolden(t)
	if len(b) != motionBody+69 {
		t.Fatalf("fixture length %d", len(b))
	}
	d, err := decodeObserverDetails(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Environment == nil || d.Heater == nil || d.Firmware != "test-v1" {
		t.Fatalf("earlier components: %+v", d)
	}
	baro := d.Barometer
	if baro == nil || baro.State != "ready" || *baro.TemperatureC != 22.15 || *baro.PressurePa != 101325 || baro.ExtendedRange {
		t.Fatalf("barometer: %+v", baro)
	}
	tc := d.Thermocouple
	if tc == nil || tc.State != "configured" || tc.Type != "K" || tc.NotchHz != 60 || !tc.NewConversion ||
		len(tc.Faults) != 0 || *tc.ThermocoupleC != 234.56 || *tc.ColdJunctionC != 24.37 {
		t.Fatalf("thermocouple: %+v", tc)
	}
	m := d.Motion
	if m == nil || m.IMUState != "ready" || m.MagnetometerState != "ready" || m.RateHz != 100 || m.AccelRangeG != 8 ||
		m.GyroRangeDPS != 1000 || m.Packets != 3000 || m.Overflows != 1 || m.Resyncs != 0 {
		t.Fatalf("motion: %+v", m)
	}
	s := m.Latest
	if s == nil || s.UptimeMS != 999900 || s.AccelCounts != [3]int16{10, -20, 4096} || !near(s.AccelG[2], 1) ||
		!near(s.GyroDPS[0], 3*1000.0/32768) || s.TemperatureC != 27.5 {
		t.Fatalf("latest IMU sample: %+v", s)
	}
	if w := m.Window; w == nil || w.AccelMinG != 0.98 || w.AccelMaxG != 1.53 || w.GyroMaxDPS != 12.5 {
		t.Fatalf("window: %+v", m.Window)
	}
	g := m.Magnetometer
	if g == nil || g.UptimeMS != 999800 || g.FieldCounts != [3]int16{512, -205, 922} || !near(g.FieldMicrotesla[0], 25) ||
		!near(g.FieldMicrotesla[1], -205*100.0/2048) || g.OffsetCounts != [3]uint16{32808, 32751, 32771} {
		t.Fatalf("magnetometer: %+v", g)
	}
}

func TestObserverDetailsMAXValidation(t *testing.T) {
	good := observerMAXGolden(t)
	for _, change := range []struct {
		offset int
		value  byte
		why    string
	}{
		{barometerBody, 2, "barometer version"},
		{barometerBody + 1, 3, "barometer state"},
		{barometerBody + 1, 0, "a valid reading from an absent barometer"},
		{barometerBody + 3, 1, "extended range at 1013 hPa"},
		{thermocoupleBody + 5, 0x02, "a J-type configuration"},
		{thermocoupleBody + 5, 0x23, "reserved configuration bits"},
		{thermocoupleBody + 4, 0x01, "a valid thermocouple with an open-circuit fault"},
		{thermocoupleBody + 3, 0, "a valid reading without a new conversion"},
		{thermocoupleBody + 1, 0, "a valid reading from an unconfigured converter"},
		{motionBody, 2, "motion version"},
		{motionBody + 3, 8, "motion validity"},
		{motionBody + 2, 0, "a magnetometer reading while it is not responding"},
		{motionBody + 6, 3, "an accelerometer range the part does not have"},
		{motionBody + 22, 0xc5, "a temperature off the 0.5 C FIFO steps"},
		{motionBody + 35, 0xff, "a minimum above the maximum"},
		{motionBody + 41, 0xff, "an IMU sample after the report"},
		{motionBody + 61, 0xff, "a magnetometer sample after the report"},
	} {
		b := append([]byte(nil), good...)
		b[change.offset] = change.value
		if _, err := decodeObserverDetails(b); err == nil {
			t.Errorf("accepted %s", change.why)
		}
	}
	// A fault reported without a new conversion, as when an input over/under voltage
	// suspends conversions: still decodable, both readings withheld.
	b := append([]byte(nil), good...)
	copy(b[thermocoupleBody:], []byte{1, 1, 0, 0, 0x02, 0x13, 0, 0, 0, 0, 0, 0})
	d, err := decodeObserverDetails(b)
	if err != nil {
		t.Fatal(err)
	}
	if tc := d.Thermocouple; tc.ThermocoupleC != nil || tc.ColdJunctionC != nil || tc.NotchHz != 50 ||
		len(tc.Faults) != 1 || tc.Faults[0] != "over_under_voltage" {
		t.Fatalf("faulted thermocouple: %+v", tc)
	}
	// Withheld values must be zero on the wire.
	b = append([]byte(nil), good...)
	b[barometerBody+2] = 0
	if _, err := decodeObserverDetails(b); err == nil {
		t.Error("accepted an invalid barometer reading with values")
	}
	b = append([]byte(nil), good...)
	b[motionBody+3] = 6 // latest sample withheld while its bytes remain
	if _, err := decodeObserverDetails(b); err == nil {
		t.Error("accepted a withheld IMU sample with values")
	}
	for _, cut := range []int{barometerBody + 9, thermocoupleBody + 11, motionBody + 68} {
		if _, err := decodeObserverDetails(good[:cut]); err == nil {
			t.Errorf("accepted truncation at %d", cut)
		}
	}
}

func FuzzObserverDetailsMAX(f *testing.F) {
	f.Add(observerMAXGolden(f))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = decodeObserverDetails(b) })
}
