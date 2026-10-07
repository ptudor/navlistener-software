package ingest

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/wire"
)

// miniCollector is a minimal GNF1 server for the collector→feeder behaviours the real
// PushServer never produces: an arbitrary frame injected right after WELCOME (a notice of
// a type the feeder does not consume, larger than its reader buffer) and an ack stream
// that lags the received seq by design. It speaks exactly the wire the feeder expects —
// magic, HELLO/WELCOME, DATA acked with the received seq (minus the lag), PING answered
// with PONG — and reports every delivered seq and every accepted connection.
type miniCollector struct {
	tc           *tls.Config
	afterWelcome [][]byte // already-framed bytes written right after WELCOME
	ackLag       uint64   // ack seq-ackLag instead of seq; 0 acks every received seq
	seqs         chan uint64
	conns        chan net.Conn // every accepted raw connection, so a test can close one
}

func (m *miniCollector) serve(ctx context.Context, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case m.conns <- c:
		default:
		}
		go m.handle(ctx, c)
	}
}

func (m *miniCollector) handle(ctx context.Context, raw net.Conn) {
	conn := tls.Server(raw, m.tc)
	defer conn.Close()
	if err := conn.Handshake(); err != nil {
		return
	}
	if err := wire.ReadMagic(conn); err != nil {
		return
	}
	ft, payload, err := wire.ReadFrameMax(conn, helloMaxLen)
	if err != nil || ft != wire.Hello {
		return
	}
	if _, err := wire.ParseHello(payload); err != nil {
		return
	}
	if err := wire.WriteWelcome(conn, wire.WelcomeMsg{OK: true}); err != nil {
		return
	}
	for _, f := range m.afterWelcome {
		if _, err := conn.Write(f); err != nil {
			return
		}
	}
	for ctx.Err() == nil {
		ft, payload, err := wire.ReadFrame(conn)
		if err != nil {
			return
		}
		switch ft {
		case wire.Data:
			seq, _, err := wire.DecodeData(payload)
			if err != nil {
				return
			}
			select {
			case m.seqs <- seq:
			case <-ctx.Done():
				return
			}
			if seq > m.ackLag {
				if err := wire.WriteFrame(conn, wire.Ack, wire.EncodeAck(seq-m.ackLag)); err != nil {
					return
				}
			}
		case wire.Ping:
			if err := wire.WriteFrame(conn, wire.Pong, nil); err != nil {
				return
			}
		}
	}
}

// TestNavfeederSkipsOversizedCollectorFrame: a collector→feeder frame larger than the
// reader's 256-byte buffer used to be a fatal error that cycled the whole session with a
// replay — invisible except as "disconnected"/"feeder disconnected" on the two sides.
// Today's collector only sends ACK, PONG and a 36-byte control frame to this feeder, but
// it already has larger reception frames for other observers, and any future notice
// would have put every C feeder into a reconnect loop. A well-formed oversized frame of
// an unknown type must be discarded with the stream intact: the ACK behind it is still
// applied (the ring is empty at disconnect) and the connection stays up.
func TestNavfeederSkipsOversizedCollectorFrame(t *testing.T) {
	bin := feederBinary(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var notice bytes.Buffer
	if err := wire.WriteFrame(&notice, wire.FrameType(0x7f), bytes.Repeat([]byte{0xa5}, 300)); err != nil {
		t.Fatal(err)
	}
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	mc := &miniCollector{tc: tc, afterWelcome: [][]byte{notice.Bytes()},
		seqs: make(chan uint64, 64), conns: make(chan net.Conn, 8)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go mc.serve(ctx, ln)

	const frames = 6
	srcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srcLn.Close()
	go serveFakeReceiver(ctx, srcLn, syntheticSFRBXCapture(frames, 0))

	ferr := &syncBuffer{}
	cmd := exec.CommandContext(ctx, bin,
		"--server", ln.Addr().String(), "--source", srcLn.Addr().String(),
		"--station", "oversize-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "64")
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

	var first net.Conn
	select {
	case first = <-mc.conns:
	case <-time.After(10 * time.Second):
		t.Fatal("the feeder never connected")
	}
	deadline := time.After(15 * time.Second)
	for n := 0; n < frames; n++ {
		select {
		case <-mc.seqs:
		case <-deadline:
			t.Fatalf("only %d/%d frames arrived after the oversized frame; stderr:\n%s", n, frames, ferr.String())
		}
	}
	// Give the feeder's reader time to apply the trailing ACK, then prove the session
	// never cycled: no second connection, no reconnect in the log.
	time.Sleep(300 * time.Millisecond)
	select {
	case <-mc.conns:
		t.Fatalf("the feeder reconnected after the oversized frame:\n%s", ferr.String())
	default:
	}
	if strings.Contains(ferr.String(), "reconnecting") || strings.Contains(ferr.String(), "disconnected") {
		t.Fatalf("the session did not survive the oversized frame:\n%s", ferr.String())
	}
	// Close from the collector side: the disconnect summary shows whether the ACK that
	// followed the discarded frame pruned the ring (spooled=0) or was lost with it.
	_ = first.Close()
	waitForLog(t, ferr, "disconnected (spooled=0 dropped=0 disk_dropped=0)", 15*time.Second)
}

// TestNavfeederReportsRetainedAckedPrefixAtCap: the disk spool file is freed only once
// EVERY record in it is acked. An ack stream that flows but lags behind the ring's span
// (a historian persistently behind) keeps appending behind a growing acked prefix until
// --spool-disk-mb is hit, and from then on every eviction is dropped although most of the
// file is dead weight — the one way the disk tier loses data while the collector is up,
// and the ack-stall watchdog cannot see it because acks do progress. The condition must at
// least be observable: a throttled line naming the retained acked prefix.
func TestNavfeederReportsRetainedAckedPrefixAtCap(t *testing.T) {
	bin := feederBinary(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const ring = 8
	const framesToCap = (1<<20 - spoolHeaderLen) / spoolRecordOnDisk // records that fit under --spool-disk-mb 1
	const batch1 = framesToCap + ring + 200                          // enough evictions to reach the cap
	const batch2 = 200

	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	mc := &miniCollector{tc: tc, ackLag: 100, seqs: make(chan uint64, 1024), conns: make(chan net.Conn, 8)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go mc.serve(ctx, ln)
	var received atomic.Int64
	go func() {
		for {
			select {
			case <-mc.seqs:
				received.Add(1)
			case <-ctx.Done():
				return
			}
		}
	}()

	// The source streams batch 1 on connect and batch 2 when the test releases it.
	release := make(chan struct{})
	srcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srcLn.Close()
	go func() {
		c, err := srcLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write(syntheticSFRBXCapture(batch1, 0))
		select {
		case <-release:
			_, _ = c.Write(syntheticSFRBXCapture(batch2, 0))
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()

	spool := filepath.Join(t.TempDir(), "spool.bin")
	ferr := &syncBuffer{}
	cmd := exec.CommandContext(ctx, bin,
		"--server", ln.Addr().String(), "--source", srcLn.Addr().String(),
		"--station", "lag-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "8", "--spool-file", spool, "--spool-disk-mb", "1")
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

	// Batch 1 fills the file to the cap; everything that fit (plus the ring) is delivered,
	// and the lagging acks leave the file permanently short of fully acked. The wait is
	// progress-based: under a full parallel test run the feeder's throughput drops well
	// below the single-package rate, so a fixed wall-clock bound would time out a healthy
	// transfer. Only a stall (no new frames for 30 s) or an absolute 5 min cap fails it.
	const (
		stallAfter   = 30 * time.Second
		absoluteWait = 5 * time.Minute
	)
	start := time.Now()
	lastProgress, lastCount := start, received.Load()
	for lastCount < framesToCap+ring {
		time.Sleep(50 * time.Millisecond)
		if n := received.Load(); n != lastCount {
			lastCount, lastProgress = n, time.Now()
			continue
		}
		if time.Since(lastProgress) > stallAfter || time.Since(start) > absoluteWait {
			t.Fatalf("only %d/%d frames of batch 1 delivered after %s; stderr:\n%s", lastCount, framesToCap+ring, time.Since(start).Round(time.Second), ferr.String())
		}
	}
	// Batch 2's evictions hit the cap while an acked prefix sits in the file.
	close(release)
	waitForLog(t, ferr, "disk spool at cap (", 30*time.Second)
	if !strings.Contains(ferr.String(), "already-acked frame(s) (seq 1..") {
		t.Errorf("the at-cap line does not name the retained acked prefix:\n%s", ferr.String())
	}
}
