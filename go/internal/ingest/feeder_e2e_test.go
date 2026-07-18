package ingest

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestNavfeederEndToEnd runs the real C navfeeder against a real PushServer and asserts
// the frames the collector receives match, byte-for-byte, what the Go dial-mode scanner
// (scanUBX) decodes from the same capture. This is the cross-oracle check for the feeder's
// SFRBX parse + big-endian word packing: the C edge and the Go collector must agree on the
// wire, or the two ingest modes would diverge. The zstd subtest additionally exercises the
// C libzstd compressor against the Go klauspost/compress decompressor on the DATA stream.
// Skips when the binary isn't built.
func TestNavfeederEndToEnd(t *testing.T) {
	t.Run("plaintext", func(t *testing.T) { runFeederE2E(t, false) })
	t.Run("zstd", func(t *testing.T) { runFeederE2E(t, true) })
}

// TestNavfeederDiskSpillWhileConnected guards with a tiny RAM ring (8 frames)
// and a disk spool, streaming the whole capture forces sustained ring→disk eviction
// WHILE the uplink is connected — the regime where disk_drain's per-connection read
// cursor (rather than a rescan from byte 0 per spill) does the work. Every frame must
// still arrive exactly once and in seq order: a cursor bug that advances past an unsent
// record or resumes at a non-record boundary drops or stalls frames here (empirically
// pinned: a deliberately misaligned cursor advance fails this test). The
// delete-then-respill generation reset is pinned separately by
// TestNavfeederDiskSpillAcrossSpoolDeletion.
func TestNavfeederDiskSpillWhileConnected(t *testing.T) {
	runFeederE2E(t, false, "--spool", "8", "--spool-file", filepath.Join(t.TempDir(), "spool.bin"))
}

// TestNavfeederDiskSpillAcrossSpoolDeletion pins disk_gen cursor reset: burst 1
// spills to disk, drains, and is fully acked, so disk_maybe_delete unlinks the spool file
// mid-connection; burst 2 then spills into a NEW file whose offsets restart at zero. A
// cursor that survives the generation change (the exact bug the disk_gen tag prevents)
// seeks past the second file's records — they were already evicted from the 8-frame ring,
// so they can never be delivered and the test times out short. Empirically pinned: a
// build with the generation reset disabled fails this test. The 750 ms pause gives
// drain+ack+delete overwhelming margin on this harness (ack cadence 25 ms); if a slow
// machine ever misses the deletion window the second burst appends to the same file and
// the test still passes — opportunistic exercise, never a false failure.
func TestNavfeederDiskSpillAcrossSpoolDeletion(t *testing.T) {
	bin := feederBinary(t)
	capPath := filepath.Join("testdata", "f9t_capture.ubx")
	single := scanCaptureNavFrames(t, capPath)
	if len(single) < 10 {
		t.Fatalf("capture yielded only %d nav frames; expected many", len(single))
	}
	capBytes, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan *RawFrame, 16384)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("f9t-e2e", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pushLn, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	defer pushLn.Close()
	go func() { _ = srv.serve(ctx, pushLn) }()

	// Fake receiver: burst, pause long enough for drain+ack+unlink, burst again.
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
				time.Sleep(750 * time.Millisecond)
				_, _ = c.Write(capBytes)
				<-ctx.Done()
			}(c)
		}
	}()

	var ferr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin,
		"--server", pushLn.Addr().String(),
		"--source", srcLn.Addr().String(),
		"--station", "f9t-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "8",
		"--spool-file", filepath.Join(t.TempDir(), "spool.bin"))
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

	want := 2 * len(single)
	got := make([]*RawFrame, 0, want)
	deadline := time.After(30 * time.Second)
	for len(got) < want {
		select {
		case f := <-out:
			got = append(got, f)
		case <-deadline:
			t.Fatalf("only %d/%d frames reached the collector across the spool deletion", len(got), want)
		}
	}
	for i := range got {
		e, g := single[i%len(single)], got[i]
		if e.GnssID != g.GnssID || e.SvID != g.SvID || e.SigID != g.SigID || len(e.Words) != len(g.Words) {
			t.Fatalf("frame %d envelope mismatch across spool generations", i)
		}
		for j := range e.Words {
			if e.Words[j] != g.Words[j] {
				t.Fatalf("frame %d word %d mismatch across spool generations", i, j)
			}
		}
	}
}

func runFeederE2E(t *testing.T, useZstd bool, extraArgs ...string) {
	bin := feederBinary(t)

	// Ground truth: the nav frames the Go scanner lifts off the capture.
	capPath := filepath.Join("testdata", "f9t_capture.ubx")
	expected := scanCaptureNavFrames(t, capPath)
	if len(expected) < 10 {
		t.Fatalf("capture yielded only %d nav frames; expected many", len(expected))
	}
	capBytes, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The collector push endpoint. A generous out buffer so the drain loop never loses a
	// frame to the queue-full drop while we read.
	out := make(chan *RawFrame, 8192)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("f9t-e2e", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pushLn, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	defer pushLn.Close()
	go func() { _ = srv.serve(ctx, pushLn) }()

	// A fake receiver: stream the capture once on connect, then hold the connection open
	// (no reconnect, so no duplicate second pass) until the test ends.
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

	// Run the feeder: read the fake receiver over TCP, push to the collector over TLS.
	var ferr bytes.Buffer
	args := []string{
		"--server", pushLn.Addr().String(),
		"--source", srcLn.Addr().String(),
		"--station", "f9t-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure",
	}
	if len(extraArgs) > 0 {
		args = append(args, extraArgs...) // caller overrides/extends (e.g. tiny --spool + --spool-file)
	} else {
		args = append(args, "--spool", "100000")
	}
	if useZstd {
		args = append(args, "--zstd")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
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

	// Collect the first len(expected) nav frames — the feeder pushes in stream order, so
	// they line up with the scanner's output one-to-one.
	got := make([]*RawFrame, 0, len(expected))
	deadline := time.After(20 * time.Second)
	for len(got) < len(expected) {
		select {
		case f := <-out:
			got = append(got, f)
		case <-deadline:
			t.Fatalf("only %d/%d frames reached the collector", len(got), len(expected))
		}
	}

	for i := range expected {
		e, g := expected[i], got[i]
		if e.GnssID != g.GnssID || e.SvID != g.SvID || e.SigID != g.SigID || e.FreqID != g.FreqID {
			t.Fatalf("frame %d envelope: scanner %v/%d/%d/%d vs feeder %v/%d/%d/%d",
				i, e.GnssID, e.SvID, e.SigID, e.FreqID, g.GnssID, g.SvID, g.SigID, g.FreqID)
		}
		if len(e.Words) != len(g.Words) {
			t.Fatalf("frame %d word count: scanner %d vs feeder %d", i, len(e.Words), len(g.Words))
		}
		for j := range e.Words {
			if e.Words[j] != g.Words[j] {
				t.Fatalf("frame %d word %d: scanner %#08x vs feeder %#08x", i, j, e.Words[j], g.Words[j])
			}
		}
	}
	t.Logf("cross-checked %d nav frames C-feeder↔Go-collector", len(got))
}

// scanCaptureNavFrames runs the Go dial-mode UBX scanner over a capture file and returns the
// decoded nav frames (SFRBX; the RAWX observables the scanner also emits carry Obs and are
// excluded — the feeder forwards only nav frames).
func scanCaptureNavFrames(t *testing.T, path string) []*RawFrame {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var frames []*RawFrame
	now := func() time.Time { return time.Unix(0, 0) }
	_ = scanUBX(f, "cap", now, func(fr *RawFrame) {
		if fr.Obs == nil && fr.RF == nil { // nav frames only — the feeder forwards RAWX/RF as telemetry
			frames = append(frames, fr)
		}
	}, func(string) {})
	return frames
}

// feederBinary locates the built navfeeder relative to this package (go/internal/ingest →
// feeder/navfeeder), skipping when it hasn't been compiled but refusing a stale oracle.
func feederBinary(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "feeder", "navfeeder"))
	if err != nil {
		t.Fatal(err)
	}
	binInfo, err := os.Stat(p)
	if err != nil {
		t.Skipf("navfeeder not built (%s); run: make -C feeder", p)
	}
	source := filepath.Join(filepath.Dir(p), "navfeeder.c")
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatalf("stat navfeeder source: %v", err)
	}
	if binInfo.ModTime().Before(sourceInfo.ModTime()) {
		t.Fatalf("navfeeder binary is older than %s; rebuild with: make -C feeder", source)
	}
	return p
}
