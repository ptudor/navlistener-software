package ingest

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestNavfeederPingThroughZstd: a PING is written through the negotiated zstd stream like
// any DATA frame, and the collector must decode it in-stream — a framing mistake there
// desyncs the decoder and tears the session down. Every other e2e run finishes well under
// the 30 s keepalive, so the PING never fired in a test; a 1 s keepalive build and a
// source that goes quiet for several seconds force it. The session must survive the idle
// gap on one connection: frames sent after the gap arrive, and nothing reconnected.
func TestNavfeederPingThroughZstd(t *testing.T) {
	bin := feederBinaryWithDefines(t, "navfeeder-keepalive", "-DKEEPALIVE_S=1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan *RawFrame, 64)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("ping-e2e", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pushLn, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	defer pushLn.Close()
	go func() { _ = srv.serve(ctx, pushLn) }()

	// The fake receiver: a burst, a silence longer than several keepalives, a second burst.
	const burst = 6
	const quiet = 3500 * time.Millisecond
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
				_, _ = c.Write(syntheticSFRBXCapture(burst, 0))
				select {
				case <-time.After(quiet):
				case <-ctx.Done():
					return
				}
				_, _ = c.Write(syntheticSFRBXCapture(burst, 0))
				<-ctx.Done()
			}(c)
		}
	}()

	ferr := &syncBuffer{}
	cmd := exec.CommandContext(ctx, bin,
		"--server", pushLn.Addr().String(), "--source", srcLn.Addr().String(),
		"--station", "ping-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "64", "--zstd")
	cmd.Stderr = ferr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if t.Failed() {
			t.Logf("navfeeder stderr:\n%s", ferr.String())
		}
	})

	deadline := time.After(20 * time.Second)
	for n := 0; n < 2*burst; n++ {
		select {
		case <-out:
		case <-deadline:
			t.Fatalf("only %d/%d frames reached the collector across the idle gap; stderr:\n%s",
				n, 2*burst, ferr.String())
		}
	}
	log := ferr.String()
	if !strings.Contains(log, "zstd=1") {
		t.Fatalf("the session did not negotiate zstd:\n%s", log)
	}
	if strings.Count(log, "connected:") != 1 || strings.Contains(log, "reconnecting") || strings.Contains(log, "disconnected") {
		t.Fatalf("the session did not survive the keepalive gap on one connection:\n%s", log)
	}
}
