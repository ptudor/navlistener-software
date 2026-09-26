package ingest

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// The X20 fixture is the NEO golden report followed by tags 14-16, built from the documented
// layout (docs/OBSERVER-TELEMETRY.md) and shared with the C encoder test.
func observerX20Golden(t testing.TB) []byte {
	t.Helper()
	text, err := os.ReadFile("../../../testdata/observer_details_x20_v1.hex")
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
	x20HumidityBody = 258 + 3
	pressureBody    = x20HumidityBody + 2 + 3
	railsBody       = pressureBody + 2 + 3
)

func TestObserverDetailsX20Golden(t *testing.T) {
	b := observerX20Golden(t)
	if len(b) != railsBody+27 {
		t.Fatalf("fixture length %d", len(b))
	}
	d, err := decodeObserverDetails(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Environment == nil || *d.Environment.PressurePa != 100000 || d.HumiditySensor != "hdc2080" || d.PressureSensor != "bmp581" {
		t.Fatalf("earlier components: %+v", d)
	}
	r := d.Rails
	if r == nil || r.State != "ready" || len(r.Channels) != 3 {
		t.Fatalf("rails: %+v", r)
	}
	want := []struct {
		mohm         uint16
		volts, amps  float64
		shuntMicrovV int32
	}{{20, 5.008, 0.614, 12280}, {50, 3.296, 0.1, 5000}, {20, 3.304, -0.2, -4000}}
	for i, w := range want {
		c := r.Channels[i]
		if c.Channel != i+1 || c.ShuntMilliohms != w.mohm || c.BusVolts == nil || !near(*c.BusVolts, w.volts) ||
			*c.ShuntMicrovolts != w.shuntMicrovV || !near(*c.CurrentAmps, w.amps) {
			t.Fatalf("channel %d: %+v", i+1, c)
		}
	}
}

func TestObserverDetailsX20Validation(t *testing.T) {
	good := observerX20Golden(t)
	for _, change := range []struct {
		offset int
		value  byte
		why    string
	}{
		{pressureBody, 2, "pressure-sensor version"},
		{pressureBody + 1, 5, "an unknown pressure-sensor part"},
		{railsBody, 2, "rails version"},
		{railsBody + 1, 2, "an unknown rails state"},
		{railsBody + 1, 0, "readings from a monitor that is not responding"},
		{railsBody + 2, 8, "a fourth channel"},
		{railsBody + 3 + 1, 0x91, "a bus voltage off the 8 mV steps"},
		{railsBody + 3 + 5, 0xf9, "a shunt voltage off the 40 uV steps"},
		{railsBody + 3 + 2, 0x7f, "a shunt voltage past full scale"},
		{railsBody + 3 + 7, 0, "a reading through no shunt"},
	} {
		b := append([]byte(nil), good...)
		b[change.offset] = change.value
		if _, err := decodeObserverDetails(b); err == nil {
			t.Errorf("accepted %s", change.why)
		}
	}
	// Withheld values must be zero on the wire; a channel without a shunt is reported unused.
	b := append([]byte(nil), good...)
	b[railsBody+2] = 6
	if _, err := decodeObserverDetails(b); err == nil {
		t.Error("accepted a withheld channel with values")
	}
	copy(b[railsBody+3:], []byte{0, 0, 0, 0, 0, 0, 0, 0})
	d, err := decodeObserverDetails(b)
	if err != nil {
		t.Fatal(err)
	}
	if c := d.Rails.Channels[0]; c.BusVolts != nil || c.CurrentAmps != nil || c.ShuntMilliohms != 0 {
		t.Fatalf("unused channel: %+v", c)
	}
	for _, cut := range []int{pressureBody + 1, railsBody + 26} {
		if _, err := decodeObserverDetails(good[:cut]); err == nil {
			t.Errorf("accepted truncation at %d", cut)
		}
	}
}

func FuzzObserverDetailsX20(f *testing.F) {
	f.Add(observerX20Golden(f))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = decodeObserverDetails(b) })
}
