package wire

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/ptudor/gnss"
)

func TestMagicRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMagic(&buf); err != nil {
		t.Fatal(err)
	}
	if err := ReadMagic(&buf); err != nil {
		t.Fatalf("ReadMagic = %v", err)
	}
}

func TestBadMagic(t *testing.T) {
	if err := ReadMagic(bytes.NewReader([]byte("XXXX"))); err != ErrBadMagic {
		t.Errorf("err = %v, want ErrBadMagic", err)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("hello payload")
	if err := WriteFrame(&buf, Hello, payload); err != nil {
		t.Fatal(err)
	}
	ft, got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if ft != Hello || !bytes.Equal(got, payload) {
		t.Errorf("round-trip = %d/%q", ft, got)
	}
}

func TestFrameTooLargeOnWrite(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Data, make([]byte, MaxFrameLen+1)); err != ErrFrameTooLarge {
		t.Errorf("write err = %v, want ErrFrameTooLarge", err)
	}
}

func TestFrameTooLargeOnRead(t *testing.T) {
	// Hand-craft a header claiming a payload larger than MaxFrameLen; ReadFrame
	// must reject it before allocating.
	var hdr [5]byte
	hdr[0] = byte(Data)
	binary.BigEndian.PutUint32(hdr[1:], MaxFrameLen+1)
	_, _, err := ReadFrame(bytes.NewReader(hdr[:]))
	if err != ErrFrameTooLarge {
		t.Errorf("read err = %v, want ErrFrameTooLarge", err)
	}
}

func TestDataRoundTrip(t *testing.T) {
	rec := RawRecord{
		RecvUnixNs: 1_700_000_000_000_000_000,
		GnssID:     gnss.GLONASS,
		SvID:       5,
		SigID:      0,
		FreqID:     11,
		FrameType:  0x40,
		Raw:        []byte{0xDE, 0xAD, 0xBE, 0xEF},
	}
	payload := EncodeData(42, rec)
	seq, got, err := DecodeData(payload)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 42 {
		t.Errorf("seq = %d, want 42", seq)
	}
	if got.RecvUnixNs != rec.RecvUnixNs || got.GnssID != rec.GnssID ||
		got.SvID != rec.SvID || got.SigID != rec.SigID ||
		got.FreqID != rec.FreqID || got.FrameType != rec.FrameType {
		t.Errorf("record header mismatch: %+v", got)
	}
	if !bytes.Equal(got.Raw, rec.Raw) {
		t.Errorf("raw = %x, want %x", got.Raw, rec.Raw)
	}
}

func TestDecodeDataShort(t *testing.T) {
	if _, _, err := DecodeData([]byte{1, 2, 3}); err != ErrShortRecord {
		t.Errorf("err = %v, want ErrShortRecord", err)
	}
}

func TestHelloWelcomeFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHello(&buf, HelloMsg{Token: "t", Station: "s", Feed: "ubx", Session: "boot-1"}); err != nil {
		t.Fatal(err)
	}
	ft, payload, err := ReadFrame(&buf)
	if err != nil || ft != Hello {
		t.Fatalf("hello frame: ft=%d err=%v", ft, err)
	}
	if !bytes.Contains(payload, []byte(`"feed":"ubx"`)) {
		t.Errorf("hello JSON missing feed: %s", payload)
	}
	if !bytes.Contains(payload, []byte(`"session":"boot-1"`)) {
		t.Errorf("hello JSON missing session : %s", payload)
	}
}

// TestValidSession pins the regression fix session-identity domain: 1..SessionMaxLen
// bytes of [A-Za-z0-9._-]. The charset must stay JSON/SQL-metacharacter-free —
// the session lands in logs and the historian ledger verbatim.
func TestValidSession(t *testing.T) {
	long := make([]byte, SessionMaxLen)
	for i := range long {
		long[i] = 'a'
	}
	for _, tc := range []struct {
		s  string
		ok bool
	}{
		{"", false},
		{"a", true},
		{"0123456789abcdef0123456789abcdef", true}, // the reference 32-hex shape
		{"boot-1.2_3", true},
		{string(long), true},
		{string(long) + "a", false},
		{`boot"1`, false},
		{"boot 1", false},
		{"boot\x00", false},
		{"séssion", false},
	} {
		if got := ValidSession(tc.s); got != tc.ok {
			t.Errorf("ValidSession(%q) = %v, want %v", tc.s, got, tc.ok)
		}
	}
}

// TestWelcomeCompactSpelling pins the regression fix wire requirement at the unit level: the
// WELCOME payload must carry the compact `"ok":true` / `"zstd":true` byte sequences the edge
// feeders match with strstr, because neither the OpenWrt binary nor the ESP32 firmware
// carries a JSON parser. Reformatting this payload (MarshalIndent, a hand-rolled encoder
// with a space after the colon, a re-serializing proxy) would leave the entire fleet in a
// permanent reconnect loop, never seeing an accepted handshake. The C-feeder e2e suite
// catches it too, but only where a C toolchain exists — this runs everywhere and names the
// contract.
func TestWelcomeCompactSpelling(t *testing.T) {
	b, err := MarshalWelcome(WelcomeMsg{OK: true, Zstd: true, AckIntervalMS: 250})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ok":true`, `"zstd":true`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("WELCOME payload %s lacks the normative byte sequence %s", b, want)
		}
	}
	// The negative form is what actually breaks the feeders, so assert it directly.
	for _, bad := range []string{`"ok": true`, `"zstd": true`, "\n", "\t"} {
		if bytes.Contains(b, []byte(bad)) {
			t.Errorf("WELCOME payload %s contains non-compact JSON %q", b, bad)
		}
	}
	// A rejection must be equally unambiguous: no `"ok":true` anywhere in it.
	rej, err := MarshalWelcome(WelcomeMsg{OK: false, Error: "unauthorized"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rej, []byte(`"ok":true`)) {
		t.Errorf("rejection payload %s would be read as an acceptance", rej)
	}
}

func TestAckRoundTrip(t *testing.T) {
	got, err := DecodeAck(EncodeAck(9001))
	if err != nil || got != 9001 {
		t.Errorf("ack round-trip = %d, %v", got, err)
	}
}
