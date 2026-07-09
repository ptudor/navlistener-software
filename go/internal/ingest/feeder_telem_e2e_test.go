package ingest

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

// TestNavfeederTelemetryEndToEnd runs the real C navfeeder against a real PushServer over a
// synthetic UBX stream carrying MON-RF, MON-HW, and NAV-SAT, and asserts the RF telemetry
// the collector reconstructs matches, field-for-field, what the Go dial-mode scanner lifts
// off the same bytes. This is the cross-oracle for the fleet-push RF path (docs/DEFENSE-PNT.md
// §1): the C edge parses the UBX + encodes the GNF1 telemetry body, the Go collector decodes
// it, and both must agree with Go's own MON-RF/NAV-SAT parsers — or a fleet station's
// jamming/spoofing telemetry would arrive corrupted. Skips when the binary isn't built.
func TestNavfeederTelemetryEndToEnd(t *testing.T) {
	bin := feederBinary(t)
	capBytes := syntheticRFCapture()

	// Ground truth: the RF frames the Go scanner lifts off the synthetic stream.
	expected := scanRF(t, capBytes)
	if len(expected) != 3 {
		t.Fatalf("expected 3 RF frames from the synthetic capture, got %d", len(expected))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan *RawFrame, 64)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("rf-e2e", "s3cret", "ubx"),
		25*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pushLn, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	defer pushLn.Close()
	go func() { _ = srv.serve(ctx, pushLn) }()

	// A fake receiver: stream the capture once, then hold the connection open.
	srcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srcLn.Close()
	go func() {
		for {
			c, err := srcLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write(capBytes)
				<-ctx.Done()
			}(c)
		}
	}()

	var ferr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin,
		"--server", pushLn.Addr().String(),
		"--source", srcLn.Addr().String(),
		"--station", "rf-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "10000")
	cmd.Stderr = &ferr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if t.Failed() && ferr.Len() > 0 {
			t.Logf("navfeeder stderr:\n%s", ferr.String())
		}
	})

	got := make([]*RawFrame, 0, len(expected))
	deadline := time.After(20 * time.Second)
	for len(got) < len(expected) {
		select {
		case f := <-out:
			got = append(got, f)
		case <-deadline:
			t.Fatalf("only %d/%d telemetry frames reached the collector", len(got), len(expected))
		}
	}

	for i := range expected {
		e, g := expected[i], got[i]
		if g.RF == nil {
			t.Fatalf("frame %d: collector produced no RF sample", i)
		}
		if !reflect.DeepEqual(e.RF.Bands, g.RF.Bands) {
			t.Errorf("frame %d bands: scanner %+v vs feeder %+v", i, e.RF.Bands, g.RF.Bands)
		}
		if !reflect.DeepEqual(e.RF.Sats, g.RF.Sats) {
			t.Errorf("frame %d sats: scanner %+v vs feeder %+v", i, e.RF.Sats, g.RF.Sats)
		}
	}
	t.Logf("cross-checked %d RF telemetry frames C-feeder↔Go-collector", len(got))
}

// syntheticRFCapture builds a UBX byte stream with one MON-RF (two blocks), one NAV-SAT
// (three SVs incl. a below-horizon one), and one legacy MON-HW — the RF messages the feeder
// forwards. No external capture is needed: the field values are known, so both oracles
// decode a deterministic result.
func syntheticRFCapture() []byte {
	// MON-RF: version, nBlocks=2, reserved[2], then 2×24-byte blocks.
	monrf := []byte{0, 2, 0, 0}
	for _, b := range []struct {
		block, jam, ant byte
		noise, agc      uint16
		cw              byte
	}{
		{0, 0x01, 2, 110, 2900, 30}, // L1: jammingState=ok
		{1, 0x02, 3, 250, 8000, 180}, // L2/L5: jammingState=warning, short antenna, strong CW
	} {
		blk := make([]byte, 24)
		blk[0] = b.block
		blk[1] = b.jam
		blk[2] = b.ant
		binary.LittleEndian.PutUint16(blk[14:], b.noise)
		binary.LittleEndian.PutUint16(blk[16:], b.agc)
		blk[20] = b.cw
		monrf = append(monrf, blk...)
	}

	// NAV-SAT: iTOW U4, version, numSvs=3, reserved[2], then 3×12-byte blocks.
	navsat := []byte{0, 0, 0, 0, 1, 3, 0, 0}
	sv := func(gnss, id, cno byte, elev int8, used bool) []byte {
		blk := make([]byte, 12)
		blk[0], blk[1], blk[2], blk[3] = gnss, id, cno, byte(elev)
		var flags uint32
		if used {
			flags |= 0x08
		}
		binary.LittleEndian.PutUint32(blk[8:], flags)
		return blk
	}
	navsat = append(navsat, sv(0, 5, 47, 61, true)...)
	navsat = append(navsat, sv(2, 14, 33, 12, true)...)
	navsat = append(navsat, sv(6, 3, 0, -9, false)...) // untracked, below horizon

	// MON-HW: 60 bytes with noise@16, agc@18, aStatus@20, flags@22, jamInd@45.
	monhw := make([]byte, 60)
	binary.LittleEndian.PutUint16(monhw[16:], 95)
	binary.LittleEndian.PutUint16(monhw[18:], 4100)
	monhw[20] = 2    // aStatus = ok
	monhw[22] = 0x04 // jammingState = 1 (bits 2-3)
	monhw[45] = 60   // jamInd

	var s []byte
	s = append(s, ubxMsg(ubxClassMON, ubxIDMONRF, monrf)...)
	s = append(s, ubxMsg(ubxClassNAV, ubxIDNAVSAT, navsat)...)
	s = append(s, ubxMsg(ubxClassMON, ubxIDMONHW, monhw)...)
	return s
}
