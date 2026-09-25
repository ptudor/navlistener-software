package ingest

import (
	"context"
	"encoding/hex"
	"github.com/ptudor/navlistener/internal/wire"
	"os"
	"strings"
	"testing"
	"time"
)

func TestObserverDetailsPushTLS(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, out := startPushServer(t, ctx, tokenAuth("observer16", "test-token", "ubx"))
	conn := dialPush(t, addr)
	defer conn.Close()
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: "test-token", Station: "observer16", Feed: "ubx", Session: "boot-board"}); err != nil {
		t.Fatal(err)
	}
	if ft, payload, err := wire.ReadFrame(conn); err != nil || ft != wire.Welcome {
		t.Fatalf("welcome %v", err)
	} else if w, _ := parseWelcome(payload); !w.OK {
		t.Fatal("rejected")
	}
	rec := wire.RawRecord{FrameType: TelemObserverDetails, Raw: observerGolden(t)}
	if err := wire.WriteFrame(conn, wire.Data, wire.EncodeData(1, rec)); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-out:
		if f.Details == nil || f.Observer.ObserverID != "observer16" || f.Session != "boot-board" || f.Seq != 1 {
			t.Fatalf("trusted context: %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("board report not delivered")
	}
}

func TestObserverDetailsWaitsForHistorianDurability(t *testing.T) {
	tr := NewDurableTracker()
	cli, out, _ := streamOnPipe(t, "ubx", tr)
	rec := wire.RawRecord{FrameType: TelemObserverDetails, Raw: observerGolden(t)}
	if err := wire.WriteFrame(cli, wire.Data, wire.EncodeData(1, rec)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out:
	case <-time.After(time.Second):
		t.Fatal("no board sample")
	}
	if tr.Watermark("obs1", "boot-a") != 0 {
		t.Fatal("board sample acknowledged before persistence")
	}
	tr.Resolved("obs1", "boot-a", 1)
	if tr.Watermark("obs1", "boot-a") != 1 {
		t.Fatal("committed board sample did not advance watermark")
	}
}

func observerGolden(t *testing.T) []byte {
	t.Helper()
	text, err := os.ReadFile("../../../testdata/observer_details_v1.hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestObserverDetailsGoldenAndEnvelope(t *testing.T) {
	b := observerGolden(t)
	d, err := decodeObserverDetails(b)
	if err != nil {
		t.Fatal(err)
	}
	if *d.Environment.MCP9808C != 25.5 || *d.Environment.HDC2080C != 42.12 || *d.Environment.BMP3C != 25 || *d.Environment.HumidityPercent != 50 || *d.Environment.PressurePa != 100000 {
		t.Fatalf("units: %+v", d.Environment)
	}
	if d.EEPROM.BoardUIDKind != "serial128" || d.EEPROM.BoardUID != "02000000000000000000000000000001" || d.EEPROM.Action != "initialization_required" || d.ATECC.RNG != "repeating_output" || *d.RTC.Epoch != 1789433627 || d.Resources.Capacity != 4194304 || d.Receiver.Spoofing != 1 {
		t.Fatalf("diagnostics: %+v", d)
	}
	rec := wire.RawRecord{FrameType: TelemObserverDetails, Raw: b}
	f := recordToFrame(rec, "ubx", "authenticated-observer")
	if f == nil || f.Details == nil || f.Source != "authenticated-observer" || f.RF != nil || f.Words != nil || f.BoardSampleStamped {
		t.Fatalf("frame: %+v", f)
	}
	rec.RecvUnixNs = time.Now().UnixNano()
	f = recordToFrame(rec, "ubx", "authenticated-observer")
	if !f.BoardSampleStamped {
		t.Fatal("accepted clock lost")
	}
	rec.SvID = 1
	if recordToFrame(rec, "ubx", "authenticated-observer") != nil {
		t.Fatal("nonzero GNSS envelope")
	}
}
func TestObserverDetailsValidationAndExtensions(t *testing.T) {
	good := observerGolden(t)
	for n := 0; n < len(good); n++ {
		// A prefix ending exactly at a TLV boundary is a valid smaller snapshot.
		if n == 41 || n == 61 || n == 80 || n == 123 || n == 155 || n == 187 || n == 197 {
			continue
		}
		if _, err := decodeObserverDetails(good[:n]); err == nil {
			t.Fatalf("accepted truncation %d", n)
		}
	}
	for _, change := range []struct {
		offset int
		value  byte
	}{{0, 2}, {1, 0}, {22, 4}, {23, 16}, {25, 255}, {27, 8}, {28, 8}, {29, 127}, {35, 255}, {44, 255}, {72, 2},
		// Tag 4: the retired 64-bit kind, an 8-byte length, and capabilities without an action that allows them.
		{86, 1}, {87, 8}, {120, 1},
		{126, 2}, {168, 16}, {189, 40}} {
		b := append([]byte(nil), good...)
		b[change.offset] = change.value
		if _, err := decodeObserverDetails(b); err == nil {
			t.Errorf("accepted invalid offset %d", change.offset)
		}
	}
	duplicate := append(append([]byte(nil), good...), good[24:41]...)
	if _, err := decodeObserverDetails(duplicate); err == nil {
		t.Fatal("duplicate component")
	}
	extension := append(append([]byte(nil), good...), 240, 0, 2, 1, 2)
	if _, err := decodeObserverDetails(extension); err != nil {
		t.Fatal("bounded extension", err)
	}
	// An unavailable sensor loses its value, while the other two stay usable.
	b := append([]byte(nil), good...)
	b[27] = 5
	for _, i := range []int{31, 32, 35, 36} {
		b[i] = 0
	}
	d, err := decodeObserverDetails(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Environment.HDC2080C != nil || d.Environment.HumidityPercent != nil || d.Environment.MCP9808C == nil || d.Environment.BMP3C == nil {
		t.Fatal("invalid/stale measurements")
	}
}
func FuzzObserverDetails(f *testing.F) {
	text, err := os.ReadFile("../../../testdata/observer_details_v1.hex")
	if err != nil {
		f.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = decodeObserverDetails(b) })
}
