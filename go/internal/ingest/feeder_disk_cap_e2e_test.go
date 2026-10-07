package ingest

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// captureListener hands every accepted raw connection to the test so it can close one
// from the collector side and read the feeder's disconnect summary.
type captureListener struct {
	net.Listener
	conns chan net.Conn
}

func (l *captureListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		select {
		case l.conns <- c:
		default:
		}
	}
	return c, err
}

// TestNavfeederDiskCapExhaustionAndRecovery pins the --spool-disk-mb budget end to end:
// with the collector down, evictions past the cap are dropped oldest-first (the newest
// ring frames survive), the disconnect summary counts exactly those drops as
// disk_dropped, and once the delivered file is acked and cleared the disk tier accepts a
// fresh spill — nothing stays disabled after exhaustion.
func TestNavfeederDiskCapExhaustionAndRecovery(t *testing.T) {
	bin := feederBinary(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const ring = 8
	const framesToCap = (1<<20 - spoolHeaderLen) / spoolRecordOnDisk // records that fit under --spool-disk-mb 1
	const batch1 = framesToCap + ring + 2000
	const dropped = batch1 - ring - framesToCap // evictions the full file refused
	const batch2 = 100

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

	// The collector's address is bound but not served until batch 1 has settled.
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rawLn.Close()

	spool := filepath.Join(t.TempDir(), "spool.bin")
	ferr := &syncBuffer{}
	cmd := exec.CommandContext(ctx, bin,
		"--server", rawLn.Addr().String(), "--source", srcLn.Addr().String(),
		"--station", "cap-e2e", "--token", "s3cret", "--feed", "ubx",
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
	// The file stops growing exactly at the last record that fits; the remaining
	// evictions are drops. Let the producer finish the batch before serving.
	waitForSpoolSize(t, spool, spoolHeaderLen+framesToCap*spoolRecordOnDisk, ferr)
	time.Sleep(500 * time.Millisecond)

	out := make(chan *RawFrame, 32768)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	clog := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("collector log:\n%s", clog.String())
		}
	})
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("cap-e2e", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(clog, nil)))
	cl := &captureListener{Listener: rawLn, conns: make(chan net.Conn, 8)}
	go func() { _ = srv.serve(ctx, tls.NewListener(cl, tc)) }()

	// Batch 1: everything that fit on disk, then the ring; the drops in between never
	// arrive.
	got := map[uint64]bool{}
	deadline := time.After(60 * time.Second)
	for len(got) < framesToCap+ring {
		select {
		case f := <-out:
			got[f.Seq] = true
		case <-deadline:
			t.Fatalf("only %d/%d frames of batch 1 reached the collector; stderr:\n%s",
				len(got), framesToCap+ring, ferr.String())
		}
	}
	for seq := uint64(1); seq <= framesToCap; seq++ {
		if !got[seq] {
			t.Fatalf("seq %d (fits on disk) was not delivered", seq)
		}
	}
	for seq := uint64(batch1 - ring + 1); seq <= batch1; seq++ {
		if !got[seq] {
			t.Fatalf("seq %d (the surviving ring) was not delivered", seq)
		}
	}
	select {
	case f := <-out:
		t.Fatalf("seq %d arrived although it was evicted past the full disk", f.Seq)
	case <-time.After(500 * time.Millisecond):
	}

	// The acked file is cleared and the next spill goes to a fresh file: all of batch 2
	// arrives.
	waitForLog(t, ferr, "disk spool delivered and cleared", 20*time.Second)
	close(release)
	deadline = time.After(20 * time.Second)
	for seq := uint64(batch1 + 1); seq <= batch1+batch2; seq++ {
		select {
		case f := <-out:
			if f.Seq != seq {
				t.Fatalf("batch 2 delivered seq %d, want %d", f.Seq, seq)
			}
		case <-deadline:
			t.Fatalf("batch 2 stalled at seq %d; stderr:\n%s", seq, ferr.String())
		}
	}

	// Batch 2's second spill is unlinked once its last record is acked, which is also when
	// the ring's tail is pruned: only then does a disconnect summary reflect the whole run.
	deadline = time.After(10 * time.Second)
	for {
		if _, err := os.Stat(spool); os.IsNotExist(err) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the second spill was never acked and cleared; stderr:\n%s", ferr.String())
		case <-time.After(25 * time.Millisecond):
		}
	}

	// The disconnect summary must count exactly the evictions the full disk refused.
	var conn net.Conn
	select {
	case conn = <-cl.conns:
	case <-time.After(time.Second):
		t.Fatal("the collector never recorded the feeder's connection")
	}
	_ = conn.Close()
	waitForLog(t, ferr, fmt.Sprintf("disconnected (spooled=0 dropped=0 disk_dropped=%d)", dropped), 15*time.Second)
}
