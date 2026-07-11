package ingest

import (
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/ptudor/navlistener/internal/wire"
)

// TestJammingStatsRoundTrip encodes RF bands to the GNF1 JammingStats body and decodes them
// back, asserting every field survives the byte-swap to big-endian and home again.
func TestJammingStatsRoundTrip(t *testing.T) {
	bands := []RFBand{
		{Block: 0, AGC: 3000, NoiseLevel: 120, CWSuppress: 200, JamState: 2, AntStatus: 2},
		{Block: 1, AGC: 8191, NoiseLevel: 65535, CWSuppress: 0, JamState: 0, AntStatus: 4},
	}
	got, err := decodeJammingStats(EncodeJammingStats(bands))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, bands) {
		t.Errorf("round-trip mismatch:\n have %+v\n want %+v", got, bands)
	}
}

// TestReceptionDataRoundTrip round-trips per-SV C/N₀ + elevation, including a negative
// elevation (below the horizon) that must survive as a signed byte.
func TestReceptionDataRoundTrip(t *testing.T) {
	sats := []SatCN0{
		{GnssID: 0, SvID: 5, Cn0: 47, ElevDeg: 61, Used: true},
		{GnssID: 2, SvID: 14, Cn0: 33, ElevDeg: -8, Used: false},
		{GnssID: 6, SvID: 3, Cn0: 0, ElevDeg: 90, Used: false},
	}
	got, err := decodeReceptionData(EncodeReceptionData(sats))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, sats) {
		t.Errorf("round-trip mismatch:\n have %+v\n want %+v", got, sats)
	}
}

// TestReceptionDataPreservesElevationUnknownSentinel guards the push-path half
// of UBX-NAV-SAT's out-of-range "elevation unknown" sentinel (91) must
// survive the encode/decode round-trip unclamped, not collapse to a
// plausible-looking 90 that state.cn0ElevationResidual can no longer tell
// apart from a genuine zenith satellite.
func TestReceptionDataPreservesElevationUnknownSentinel(t *testing.T) {
	sats := []SatCN0{{GnssID: 0, SvID: 5, Cn0: 45, ElevDeg: 91}}
	got, err := decodeReceptionData(EncodeReceptionData(sats))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ElevDeg != 91 {
		t.Errorf("round-tripped ElevDeg = %+v, want 91 (unclamped)", got)
	}
}

// TestReceptionDataEmpty confirms a zero-sat body still round-trips (an empty sky sample).
func TestReceptionDataEmpty(t *testing.T) {
	got, err := decodeReceptionData(EncodeReceptionData(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected no sats, got %+v", got)
	}
}

// TestReceptionDataCap confirms an over-long sat list is capped to fit the feeder's record
// buffer rather than producing a body the wire can't carry.
func TestReceptionDataCap(t *testing.T) {
	sats := make([]SatCN0, maxTelemSats+50)
	for i := range sats {
		sats[i] = SatCN0{GnssID: 0, SvID: byteMod(i), Cn0: 40, ElevDeg: 30}
	}
	got, err := decodeReceptionData(EncodeReceptionData(sats))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxTelemSats {
		t.Errorf("cap: got %d sats, want %d", len(got), maxTelemSats)
	}
}

func byteMod(i int) int { return i % 200 }

// TestReceptionDataRejectsOverCapCount guards decodeReceptionData must
// reject n > maxTelemSats even when the body's length genuinely matches n (so
// the length-vs-body-size bounds check alone would accept it) -- the encoder
// never emits more than maxTelemSats, so a body claiming more is a
// misbehaving feeder pumping an oversized "sky sample" into the RF/spoofing
// detector, a shape the contract says cannot exist.
func TestReceptionDataRejectsOverCapCount(t *testing.T) {
	n := maxTelemSats + 1 // 201
	body := make([]byte, 3+n*receptionSatLen)
	body[0] = telemBodyVersion
	binary.BigEndian.PutUint16(body[1:], uint16(n))
	if _, err := decodeReceptionData(body); err != ErrBadTelemetry {
		t.Errorf("decodeReceptionData(n=%d, matching length) = %v, want ErrBadTelemetry", n, err)
	}
}

// TestTelemetryBadBody asserts truncated and mis-versioned bodies are rejected, never read
// past — the untrusted-input discipline the push path inherits from the dial parsers.
func TestTelemetryBadBody(t *testing.T) {
	if _, err := decodeJammingStats([]byte{1}); err == nil {
		t.Error("short JammingStats header not rejected")
	}
	if _, err := decodeJammingStats([]byte{1, 2, 0, 0}); err == nil {
		t.Error("JammingStats claiming 2 bands with none present not rejected")
	}
	if _, err := decodeJammingStats([]byte{9, 0}); err == nil {
		t.Error("unknown JammingStats version not rejected")
	}
	if _, err := decodeReceptionData([]byte{1, 0}); err == nil {
		t.Error("short ReceptionData header not rejected")
	}
	if _, err := decodeReceptionData([]byte{1, 0, 3}); err == nil {
		t.Error("ReceptionData claiming 3 sats with none present not rejected")
	}
	if _, err := decodeReceptionData([]byte{9, 0, 0}); err == nil {
		t.Error("unknown ReceptionData version not rejected")
	}
}

// TestIsTelemetryType checks the discriminator that separates §6.2 telemetry (< 0x10) from
// §6.1 raw-nav frame types (≥ 0x10); 0 is unmapped, not telemetry.
func TestIsTelemetryType(t *testing.T) {
	for _, tt := range []struct {
		t    int
		want bool
	}{
		{TelemReceptionData, true}, {TelemJammingStats, true},
		{0x00, false}, {0x10, false}, {0x50, false}, {0x70, false},
	} {
		if got := IsTelemetryType(tt.t); got != tt.want {
			t.Errorf("IsTelemetryType(%#x) = %v, want %v", tt.t, got, tt.want)
		}
	}
}

// TestRecordToFrameTelemetry drives the full push-side path: a GNF1 record carrying a
// telemetry frame_type decodes to a station-scoped RF frame (no nav words), tagged with the
// observer id the detector keys on. A malformed body yields nil (dropped by the caller).
func TestRecordToFrameTelemetry(t *testing.T) {
	jam := wire.RawRecord{FrameType: TelemJammingStats, Raw: EncodeJammingStats([]RFBand{{Block: 0, AGC: 2500, JamState: 1}})}
	f := recordToFrame(jam, "ubx", "obs7")
	if f == nil || f.RF == nil || f.Words != nil || f.Source != "obs7" {
		t.Fatalf("jamming frame = %+v", f)
	}
	if len(f.RF.Bands) != 1 || f.RF.Bands[0].AGC != 2500 {
		t.Errorf("jamming bands = %+v", f.RF.Bands)
	}

	rcv := wire.RawRecord{FrameType: TelemReceptionData, Raw: EncodeReceptionData([]SatCN0{{GnssID: 2, SvID: 11, Cn0: 44, ElevDeg: 55, Used: true}})}
	f = recordToFrame(rcv, "ubx", "obs7")
	if f == nil || f.RF == nil || len(f.RF.Sats) != 1 || !f.RF.Sats[0].Used {
		t.Fatalf("reception frame = %+v", f)
	}

	if recordToFrame(wire.RawRecord{FrameType: TelemJammingStats, Raw: []byte{0xff}}, "ubx", "obs7") != nil {
		t.Error("malformed telemetry body should yield nil")
	}
	if recordToFrame(wire.RawRecord{FrameType: 0x02, Raw: nil}, "ubx", "obs7") != nil {
		t.Error("un-transported telemetry type should yield nil")
	}
}

// FuzzDecodeTelemetry asserts the push-path body decoders never panic or read out of bounds
// on arbitrary bytes from a malformed or hostile feeder — the untrusted-input discipline
// (docs/INTEGRITY.md §9) applied to the fleet ingest path.
func FuzzDecodeTelemetry(f *testing.F) {
	f.Add(EncodeJammingStats([]RFBand{{Block: 0, AGC: 3000}}))
	f.Add(EncodeReceptionData([]SatCN0{{GnssID: 0, SvID: 1, Cn0: 40, ElevDeg: 30}}))
	f.Add([]byte{1, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = decodeJammingStats(b)
		_, _ = decodeReceptionData(b)
	})
}
