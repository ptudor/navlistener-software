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
	"testing"
	"time"

	"github.com/ptudor/gnss"
)

// TestNavfeederFrameTypeMatrix is the C-feeder leg of the regression fix differential test: it
// drives the REAL navfeeder binary with a synthetic UBX stream carrying one RXM-SFRBX per
// (gnssId, sigId) pair and asserts the frame_type byte the collector receives matches the
// golden matrix — the same file Go's NavType generates and the ESP32 host test reads.
//
// Testing the deployed artifact rather than a host-compiled copy of frame_type() is the
// point: the byte under test is what the historian actually persists as msg_type, and the
// bug this test was written for (SBAS: 0x70 for every sigId, where Go reserves L5/DFMC)
// was live in the shipped feeder. Skips when the binary isn't built.
func TestNavfeederFrameTypeMatrix(t *testing.T) {
	bin := feederBinary(t)
	golden := readFrameTypeGolden(t)

	// gnssId 4 is IMES: GNSSID.Valid() rejects it and the push server drops the record
	// before it becomes a RawFrame (docs/CONSTELLATIONS.md §0 — never emitted by a
	// receiver). Its rows are still covered by the Go and ESP32 unit-level checks.
	want := make(map[[2]int]int, len(golden))
	for _, r := range golden {
		if gnss.GNSSID(r.gnssID) == gnss.IMES {
			continue
		}
		want[[2]int{r.gnssID, r.sigID}] = r.frameType
	}

	capBytes := syntheticSFRBXMatrix(want)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan *RawFrame, 4*len(want))
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("ft-e2e", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pushLn, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	defer pushLn.Close()
	go func() { _ = srv.serve(ctx, pushLn) }()

	// A fake receiver: stream the synthetic capture once, then hold the connection open so
	// the feeder has no reason to reconnect and replay.
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
		"--station", "ft-e2e", "--token", "s3cret", "--feed", "ubx",
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

	got := make(map[[2]int]int, len(want))
	deadline := time.After(30 * time.Second)
	for len(got) < len(want) {
		select {
		case f := <-out:
			key := [2]int{int(f.GnssID), f.SigID}
			if prev, dup := got[key]; dup {
				// A reconnect replay would show up here; the feeder is fed once and
				// held open, so a duplicate means the record was resent, not remapped.
				if prev != f.MsgType {
					t.Errorf("(gnssId=%d, sigId=%d) arrived twice with different frame_type 0x%02x then 0x%02x",
						key[0], key[1], prev, f.MsgType)
				}
				continue
			}
			got[key] = f.MsgType
		case <-deadline:
			t.Fatalf("only %d/%d SFRBX records reached the collector", len(got), len(want))
		}
	}

	for key, w := range want {
		if g := got[key]; g != w {
			t.Errorf("navfeeder frame_type(gnssId=%d, sigId=%d) = 0x%02x, golden says 0x%02x",
				key[0], key[1], g, w)
		}
	}
	t.Logf("cross-checked %d (gnssId,sigId) frame_type mappings C-feeder↔golden matrix", len(want))
}

// syntheticSFRBXMatrix builds a UBX stream with exactly one RXM-SFRBX per requested
// (gnssId, sigId). Two nav words of arbitrary content are enough — the frame_type byte is
// derived from the ids alone, and the collector's live decode is not in this test's path.
// Layout per parseSFRBX/emit_sfrbx (F9/M9): gnssId, svId, sigId, freqId, numWords,
// reserved, version, reserved, then numWords little-endian dwrds.
func syntheticSFRBXMatrix(pairs map[[2]int]int) []byte {
	var s []byte
	// Deterministic order (gnssId, then sigId) so a failure log reads in matrix order.
	for g := 0; g < goldenGnssIDs; g++ {
		for sig := 0; sig < goldenSigIDs; sig++ {
			if _, ok := pairs[[2]int{g, sig}]; !ok {
				continue
			}
			const numWords = 2
			p := make([]byte, 8+numWords*4)
			p[0] = byte(g)
			p[1] = byte(sig + 1) // svId: any in-range PRN; distinct per row for legibility
			p[2] = byte(sig)
			p[3] = 7 // freqId: GLONASS k=0; ignored elsewhere
			p[4] = numWords
			p[6] = 2 // version (F9/M9)
			binary.LittleEndian.PutUint32(p[8:], 0x8B0000A5|uint32(g)<<8)
			binary.LittleEndian.PutUint32(p[12:], 0x00C0FFEE)
			s = append(s, ubxMsg(ubxClassRXM, ubxIDSFRBX, p)...)
		}
	}
	return s
}
