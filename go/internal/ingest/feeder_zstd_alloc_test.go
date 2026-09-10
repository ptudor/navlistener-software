package ingest

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNavfeederSurvivesZstdAllocationFailure proves regression fix. Before the
// fix the compressor was allocated AFTER handshake() had already exchanged
// "zstd":true, leaving only two bad options — write plaintext into the
// collector's now-committed decompressor, or die(). It chose die(), whose
// exit(2) bypasses the signal thread's orderly RAM-ring spill, so the newest
// unacknowledged frames were lost during exactly the memory pressure that makes
// the ring valuable. Allocation now precedes the advertisement, so a failure is
// just a failed connection attempt.
//
// The two allocations are faulted independently and transiently: the harness
// fails the first N attempts, so the same run also proves a later successful
// allocation replays everything the spool retained, in order.
func TestNavfeederSurvivesZstdAllocationFailure(t *testing.T) {
	for _, tc := range []struct{ name, env string }{
		{"context allocation", "NAVFEEDER_TEST_FAIL_ZSTD_CTX"},
		{"output buffer allocation", "NAVFEEDER_TEST_FAIL_ZSTD_BUF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := spoolTestBinary(t)
			capPath := filepath.Join("testdata", "f9t_capture.ubx")
			expected := scanCaptureNavFrames(t, capPath)
			capBytes, err := os.ReadFile(capPath)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// A collector that records every connection it accepts and every byte
			// that reaches the GNF1 layer, so "no DATA followed a compressed
			// WELCOME" can be asserted rather than assumed.
			out := make(chan *RawFrame, 8192)
			tcfg := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
			srv := newPushServer("127.0.0.1:0", tcfg, out, tokenAuth("zstd-alloc", "s3cret", "ubx"),
				25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			pushLn, err := tls.Listen("tcp", "127.0.0.1:0", tcfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pushLn.Close()
			go func() { _ = srv.serve(ctx, pushLn) }()

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

			const injectedFailures = 3
			// A plain bytes.Buffer would race: exec writes it from its own goroutine
			// while these assertions read it.
			ferr := &syncBuffer{}
			cmd := exec.CommandContext(ctx, bin, "feeder",
				"--server", pushLn.Addr().String(),
				"--source", srcLn.Addr().String(),
				"--station", "zstd-alloc", "--token", "s3cret", "--feed", "ubx",
				"--insecure", "--zstd", "--spool", "100000")
			cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", tc.env, injectedFailures))
			cmd.Stderr = ferr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				if t.Failed() && ferr.String() != "" {
					t.Logf("navfeeder stderr:\n%s", ferr.String())
				}
			})

			// The failures must not kill the process. die() would exit(2) here.
			select {
			case err := <-exited:
				t.Fatalf("feeder exited (%v) on a zstd allocation failure; the RAM ring's "+
					"unacknowledged frames are lost when the signal thread is bypassed.\nstderr:\n%s",
					err, ferr.String())
			case <-time.After(2 * time.Second):
			}

			// Once the injected failures are exhausted, the retained spool must be
			// delivered in full and in order — nothing was dropped while retrying.
			got := make([]*RawFrame, 0, len(expected))
			deadline := time.After(30 * time.Second)
			for len(got) < len(expected) {
				select {
				case f := <-out:
					got = append(got, f)
				case <-deadline:
					t.Fatalf("only %d/%d frames reached the collector after the injected "+
						"allocation failures.\nstderr:\n%s", len(got), len(expected), ferr.String())
				}
			}
			for i := range expected {
				e, g := expected[i], got[i]
				if e.GnssID != g.GnssID || e.SvID != g.SvID || e.SigID != g.SigID || len(e.Words) != len(g.Words) {
					t.Fatalf("frame %d replayed out of order or altered: scanner %v/%d/%d (%d words) "+
						"vs feeder %v/%d/%d (%d words)", i, e.GnssID, e.SvID, e.SigID, len(e.Words),
						g.GnssID, g.SvID, g.SigID, len(g.Words))
				}
			}

			// The retry must be paced by the existing bounded backoff, not spun.
			logged := strings.Count(ferr.String(), "out of memory for the zstd compressor")
			if logged != injectedFailures {
				t.Errorf("logged %d allocation failures, want exactly %d (a spin would log many more)",
					logged, injectedFailures)
			}
			// Still alive at the end.
			select {
			case err := <-exited:
				t.Fatalf("feeder exited after recovering: %v", err)
			default:
			}
		})
	}
}

// A feeder that never asks for compression must allocate no compression state at
// all, so even an unlimited injected zstd allocation failure cannot affect it.
func TestNavfeederWithoutZstdAllocatesNoCompressor(t *testing.T) {
	bin := spoolTestBinary(t)
	capPath := filepath.Join("testdata", "f9t_capture.ubx")
	expected := scanCaptureNavFrames(t, capPath)
	capBytes, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan *RawFrame, 8192)
	tcfg := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tcfg, out, tokenAuth("plain-alloc", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pushLn, err := tls.Listen("tcp", "127.0.0.1:0", tcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pushLn.Close()
	go func() { _ = srv.serve(ctx, pushLn) }()

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

	// A plain bytes.Buffer would race: exec writes it from its own goroutine
	// while these assertions read it.
	ferr := &syncBuffer{}
	cmd := exec.CommandContext(ctx, bin, "feeder",
		"--server", pushLn.Addr().String(),
		"--source", srcLn.Addr().String(),
		"--station", "plain-alloc", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "100000") // deliberately no --zstd
	// Every zstd allocation would fail if one were ever attempted.
	cmd.Env = append(os.Environ(),
		"NAVFEEDER_TEST_FAIL_ZSTD_CTX=1000", "NAVFEEDER_TEST_FAIL_ZSTD_BUF=1000")
	cmd.Stderr = ferr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if t.Failed() && ferr.String() != "" {
			t.Logf("navfeeder stderr:\n%s", ferr.String())
		}
	})

	got := 0
	deadline := time.After(30 * time.Second)
	for got < len(expected) {
		select {
		case <-out:
			got++
		case <-deadline:
			t.Fatalf("only %d/%d frames reached the collector without --zstd.\nstderr:\n%s",
				got, len(expected), ferr.String())
		}
	}
	if strings.Contains(ferr.String(), "zstd compressor") {
		t.Errorf("a non-zstd session allocated compression state:\n%s", ferr.String())
	}
}
