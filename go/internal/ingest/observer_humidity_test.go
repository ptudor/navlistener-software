package ingest

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// The humidity fixture is the regulator fixture followed by tags 24 (SHT3x) and 25 (HIH8121),
// built from the documented layout (docs/OBSERVER-TELEMETRY.md) and shared with the C encoder test.
func observerHumidityGolden(t testing.TB) []byte {
	t.Helper()
	text, err := os.ReadFile("../../../testdata/observer_details_humidity_v1.hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const (
	sht3xBody   = regulatorBody + 23 + 3
	hih8121Body = sht3xBody + 7 + 3
)

func TestObserverDetailsExtraHumidityGolden(t *testing.T) {
	b := observerHumidityGolden(t)
	if len(b) != hih8121Body+8 {
		t.Fatalf("fixture length %d", len(b))
	}
	d, err := decodeObserverDetails(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Regulator == nil || d.Rails == nil {
		t.Fatalf("earlier components: %+v", d)
	}
	s, h := d.SHT3x, d.HIH8121
	if s == nil || s.State != "ready" || s.Status != "" || s.TemperatureC == nil || !near(*s.TemperatureC, 23.45) ||
		s.HumidityPercent == nil || !near(*s.HumidityPercent, 41.2) {
		t.Fatalf("SHT3x: %+v", s)
	}
	if h == nil || h.State != "answering" || h.Status != "fresh" || h.TemperatureC == nil || !near(*h.TemperatureC, 23.1) ||
		h.HumidityPercent == nil || !near(*h.HumidityPercent, 43.75) {
		t.Fatalf("HIH8121: %+v", h)
	}
}

func TestObserverDetailsExtraHumidityValidation(t *testing.T) {
	good := observerHumidityGolden(t)
	for _, change := range []struct {
		offset int
		value  byte
		why    string
	}{
		{sht3xBody, 2, "an SHT3x version"},
		{sht3xBody + 1, 2, "an unknown SHT3x state"},
		{sht3xBody + 1, 0, "an SHT3x measurement without the part"},
		{sht3xBody + 2, 2, "an unknown SHT3x validity bit"},
		{sht3xBody + 3, 0x31, "an SHT3x temperature above 125 C"},
		{sht3xBody + 3, 0xf0, "an SHT3x temperature below -40 C"},
		{sht3xBody + 5, 0x27, "an SHT3x humidity above 100 %RH"},
		{hih8121Body, 2, "an HIH8121 version"},
		{hih8121Body + 1, 2, "an unknown HIH8121 state"},
		{hih8121Body + 1, 0, "an HIH8121 measurement from a part that did not answer"},
		{hih8121Body + 3, 1, "a measurement from a stale fetch"},
		{hih8121Body + 3, 4, "an unknown HIH8121 status"},
		{hih8121Body + 4, 0x31, "an HIH8121 temperature above 125 C"},
		{hih8121Body + 6, 0x28, "an HIH8121 humidity above 100 %RH"},
	} {
		b := append([]byte(nil), good...)
		b[change.offset] = change.value
		if _, err := decodeObserverDetails(b); err == nil {
			t.Errorf("accepted %s", change.why)
		}
	}
	// A withheld measurement is zero on the wire.
	b := append([]byte(nil), good...)
	b[sht3xBody+2] = 0
	if _, err := decodeObserverDetails(b); err == nil {
		t.Error("accepted a withheld SHT3x measurement with values")
	}
	copy(b[sht3xBody+3:], []byte{0, 0, 0, 0})
	d, err := decodeObserverDetails(b)
	if err != nil || d.SHT3x.State != "ready" || d.SHT3x.TemperatureC != nil || d.SHT3x.HumidityPercent != nil {
		t.Fatalf("withheld SHT3x: %v %+v", err, d)
	}
	// A stale HIH8121 fetch is reported with its status and no measurement.
	b = append([]byte(nil), good...)
	b[hih8121Body+2], b[hih8121Body+3] = 0, 1
	copy(b[hih8121Body+4:], []byte{0, 0, 0, 0})
	if d, err = decodeObserverDetails(b); err != nil || d.HIH8121.State != "answering" || d.HIH8121.Status != "stale" ||
		d.HIH8121.TemperatureC != nil {
		t.Fatalf("stale HIH8121: %v %+v", err, d)
	}
	// A part that does not answer has no status.
	b[hih8121Body+1] = 0
	if _, err = decodeObserverDetails(b); err == nil {
		t.Error("accepted a status from a part that did not answer")
	}
	b[hih8121Body+3] = 0
	if d, err = decodeObserverDetails(b); err != nil || d.HIH8121.State != "not_answering" || d.HIH8121.Status != "" {
		t.Fatalf("silent HIH8121: %v %+v", err, d)
	}
	if _, err := decodeObserverDetails(good[:hih8121Body+7]); err == nil {
		t.Error("accepted a truncated HIH8121 tag")
	}
}

func FuzzObserverDetailsExtraHumidity(f *testing.F) {
	f.Add(observerHumidityGolden(f))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = decodeObserverDetails(b) })
}
