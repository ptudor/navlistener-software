package ingest

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// The regulator fixture is the X20 fixture followed by tag 23, built from the documented
// layout (docs/OBSERVER-TELEMETRY.md) and shared with the C encoder test.
func observerRegulatorGolden(t testing.TB) []byte {
	t.Helper()
	text, err := os.ReadFile("../../../testdata/observer_details_regulator_v1.hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const regulatorBody = railsBody + 27 + 3

func TestObserverDetailsRegulatorGolden(t *testing.T) {
	b := observerRegulatorGolden(t)
	if len(b) != regulatorBody+23 {
		t.Fatalf("fixture length %d", len(b))
	}
	d, err := decodeObserverDetails(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Rails == nil || d.Environment == nil || *d.Environment.MCP9808C != 25.5 {
		t.Fatalf("earlier components: %+v", d)
	}
	g := d.Regulator
	if g == nil || g.State != "ready" || g.SpreaderC == nil || !near(*g.SpreaderC, 97.12) || g.PeakC == nil ||
		!near(*g.PeakC, 98.5) || g.ThrottleLevel != 2 || g.WiFiTXCapDBm == nil || *g.WiFiTXCapDBm != 13 ||
		g.ThrottleChanges != 2 || g.ThrottledSeconds != 95 || g.ThrottleC != 95 || g.FullC != 105 || g.ReleaseC != 85 {
		t.Fatalf("regulator: %+v", g)
	}
}

func TestObserverDetailsRegulatorValidation(t *testing.T) {
	good := observerRegulatorGolden(t)
	for _, change := range []struct {
		offset int
		value  byte
		why    string
	}{
		{regulatorBody, 2, "regulator version"},
		{regulatorBody + 1, 2, "an unknown sensor state"},
		{regulatorBody + 1, 0, "a latest reading from a sensor that is not responding"},
		{regulatorBody + 2, 4, "an unknown validity bit"},
		{regulatorBody + 3, 5, "a fifth throttle level"},
		{regulatorBody + 4, 0x31, "a reading above the sensor's 125 C"},
		{regulatorBody + 6, 0xe0, "a peak below the sensor's -40 C"},
		{regulatorBody + 8, 0, "a throttle level without a transmit cap"},
		{regulatorBody + 8, 85, "a transmit cap above 21 dBm"},
		{regulatorBody + 8, 7, "a transmit cap below 2 dBm"},
		{regulatorBody + 21, 0x25, "a release point above the throttle point"},
		{regulatorBody + 19, 0x24, "a full point below the throttle point"},
	} {
		b := append([]byte(nil), good...)
		b[change.offset] = change.value
		if _, err := decodeObserverDetails(b); err == nil {
			t.Errorf("accepted %s", change.why)
		}
	}
	// Withheld readings are zero on the wire; the last peak may outlive the sensor.
	b := append([]byte(nil), good...)
	b[regulatorBody+1], b[regulatorBody+2] = 0, 2
	if _, err := decodeObserverDetails(b); err == nil {
		t.Error("accepted a withheld reading with a value")
	}
	b[regulatorBody+4], b[regulatorBody+5] = 0, 0
	d, err := decodeObserverDetails(b)
	if err != nil {
		t.Fatal(err)
	}
	if g := d.Regulator; g.State != "not_responding" || g.SpreaderC != nil || g.PeakC == nil || !near(*g.PeakC, 98.5) {
		t.Fatalf("lost sensor: %+v", g)
	}
	// Unthrottled: level 0 carries no cap.
	b = append([]byte(nil), good...)
	b[regulatorBody+3], b[regulatorBody+8] = 0, 0
	if d, err = decodeObserverDetails(b); err != nil || d.Regulator.ThrottleLevel != 0 || d.Regulator.WiFiTXCapDBm != nil {
		t.Fatalf("unthrottled: %v %+v", err, d)
	}
	b[regulatorBody+8] = 66
	if _, err := decodeObserverDetails(b); err == nil {
		t.Error("accepted a cap at level 0")
	}
	if _, err := decodeObserverDetails(good[:regulatorBody+22]); err == nil {
		t.Error("accepted a truncated regulator tag")
	}
}

func FuzzObserverDetailsRegulator(f *testing.F) {
	f.Add(observerRegulatorGolden(f))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = decodeObserverDetails(b) })
}
