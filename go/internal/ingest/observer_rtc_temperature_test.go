package ingest

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// The RTC-temperature fixture is the X20 fixture followed by tag 26, built from the documented
// layout (docs/OBSERVER-TELEMETRY.md) and shared with the C encoder test.
func observerRTCTemperatureGolden(t testing.TB) []byte {
	t.Helper()
	text, err := os.ReadFile("../../../testdata/observer_details_rtc_temperature_v1.hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const rtcTemperatureBody = railsBody + 27 + 3

func TestObserverDetailsRTCTemperatureGolden(t *testing.T) {
	b := observerRTCTemperatureGolden(t)
	if len(b) != rtcTemperatureBody+5 {
		t.Fatalf("fixture length %d", len(b))
	}
	d, err := decodeObserverDetails(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Rails == nil || d.RTC == nil {
		t.Fatalf("earlier components: %+v", d)
	}
	k := d.RTCTemperature
	if k == nil || k.State != "answering" || k.TemperatureC == nil || !near(*k.TemperatureC, 24.25) {
		t.Fatalf("RTC temperature: %+v", k)
	}
	// Without tag 26, as from an older board or another RTC, the component is absent.
	if d, err := decodeObserverDetails(observerX20Golden(t)); err != nil || d.RTCTemperature != nil {
		t.Fatalf("X20 fixture: %v %+v", err, d)
	}
}

func TestObserverDetailsRTCTemperatureValues(t *testing.T) {
	good := observerRTCTemperatureGolden(t)
	for _, c := range []struct {
		state, valid, hi, lo byte
		want                 string
		celsius              float64
	}{
		{1, 1, 0xff, 0xe7, "answering", -0.25},  // -25
		{1, 1, 0xce, 0x00, "answering", -128},   // -12800, the register's floor
		{1, 1, 0x31, 0xe7, "answering", 127.75}, // 12775, its ceiling
		{1, 1, 0x00, 0x00, "answering", 0},      // a kept zero
		{1, 0, 0x00, 0x00, "answering", 0},      // withheld
		{0, 0, 0x00, 0x00, "not_answering", 0},  // lost
	} {
		b := append([]byte(nil), good...)
		b[rtcTemperatureBody+1], b[rtcTemperatureBody+2] = c.state, c.valid
		b[rtcTemperatureBody+3], b[rtcTemperatureBody+4] = c.hi, c.lo
		d, err := decodeObserverDetails(b)
		if err != nil {
			t.Fatalf("0x%02x%02x: %v", c.hi, c.lo, err)
		}
		k := d.RTCTemperature
		if k.State != c.want || (c.valid == 1) != (k.TemperatureC != nil) ||
			(k.TemperatureC != nil && !near(*k.TemperatureC, c.celsius)) {
			t.Fatalf("0x%02x%02x: %+v", c.hi, c.lo, k)
		}
	}
}

func TestObserverDetailsRTCTemperatureValidation(t *testing.T) {
	good := observerRTCTemperatureGolden(t)
	for _, change := range []struct {
		offset int
		value  byte
		why    string
	}{
		{rtcTemperatureBody, 2, "a version"},
		{rtcTemperatureBody + 1, 2, "a state"},
		{rtcTemperatureBody + 2, 2, "a validity"},
		{rtcTemperatureBody + 3, 0x7f, "a reading above the register's range"},
		{rtcTemperatureBody + 3, 0x80, "a reading below the register's range"},
		{rtcTemperatureBody + 4, 0x7a, "a reading off the 0.25 C steps"},
	} {
		b := append([]byte(nil), good...)
		b[change.offset] = change.value
		if _, err := decodeObserverDetails(b); err == nil {
			t.Fatalf("accepted %s", change.why)
		}
	}
	// A measurement needs an answering RTC, and a withheld one is zero on the wire.
	b := append([]byte(nil), good...)
	b[rtcTemperatureBody+1] = 0
	if _, err := decodeObserverDetails(b); err == nil {
		t.Fatal("accepted a measurement from an RTC that is not answering")
	}
	b = append([]byte(nil), good...)
	b[rtcTemperatureBody+2] = 0
	if _, err := decodeObserverDetails(b); err == nil {
		t.Fatal("accepted a withheld measurement that is not zero")
	}
	// The tag's length is fixed.
	short := append(append([]byte(nil), good[:rtcTemperatureBody-3]...), 26, 0, 4, 1, 1, 1, 0x09)
	if _, err := decodeObserverDetails(short); err == nil {
		t.Fatal("accepted a 4-byte tag 26")
	}
}
