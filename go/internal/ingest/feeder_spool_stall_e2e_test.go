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
	"syscall"
	"testing"
	"time"
)

// The on-disk spool layout the tests below size their waits by: the NAVSPO01 session
// header, then per record [8B seq][4B len] + the GNF1 record (13-byte header + 40 bytes
// of words for syntheticSFRBXCapture's ten-word frames).
const (
	spoolHeaderLen    = 8 + 1 + 64
	spoolRecordOnDisk = 12 + 13 + 40
)

// stderrLog is what the polling helpers need from a feeder's captured stderr; both
// mutex-guarded buffers in this package (syncBuf, syncBuffer) satisfy it.
type stderrLog interface{ String() string }

// waitForSpoolSize polls until the spool file is at least n bytes long.
func waitForSpoolSize(t *testing.T, spool string, n int64, ferr stderrLog) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if fi, err := os.Stat(spool); err == nil && fi.Size() >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("spool never reached %d bytes; stderr:\n%s", n, ferr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitForLog polls the feeder's stderr for a substring.
func waitForLog(t *testing.T, ferr stderrLog, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !strings.Contains(ferr.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the feeder never logged %q; stderr:\n%s", want, ferr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// serveFakeReceiver streams capBytes once per connection and holds the connection open.
func serveFakeReceiver(ctx context.Context, srcLn net.Listener, capBytes []byte) {
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
}

// TestNavfeederRetiresVanishedReplaySpool: an operator removes a replay file (a realistic
// "cleaning up /var/spool" action) before its connection completes. The feeder used to
// hold that connection open forever sending only PINGs — fopen failed every 50 ms, the
// replay spool stayed at the head of the list, and the live spool behind it was never
// served. A vanished replay file is unrecoverable by waiting: it must be retired, logged,
// and live delivery must follow.
func TestNavfeederRetiresVanishedReplaySpool(t *testing.T) {
	bin := feederBinary(t)
	capBytes := syntheticSFRBXCapture(40, 0)
	spool := filepath.Join(t.TempDir(), "spool.bin")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srcLn.Close()
	go serveFakeReceiver(ctx, srcLn, capBytes)

	// The collector's address is bound up front but not served: connections queue in the
	// kernel backlog, so nothing can complete a handshake until the test says so.
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rawLn.Close()

	// Run 1 captures into the spool with nowhere to deliver, then stops cleanly.
	ferr1 := &syncBuf{}
	cmd1 := exec.Command(bin,
		"--server", rawLn.Addr().String(), "--source", srcLn.Addr().String(),
		"--station", "stall-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "8", "--spool-file", spool)
	cmd1.Stderr = ferr1
	if err := cmd1.Start(); err != nil {
		t.Fatal(err)
	}
	waitForSpoolSize(t, spool, spoolHeaderLen+32*spoolRecordOnDisk, ferr1)
	if err := cmd1.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd1.Process.Wait(); err != nil {
		t.Fatal(err)
	}

	// Run 2 recovers run 1's file as a replay spool and queues a connection for it.
	ferr2 := &syncBuf{}
	cmd2 := exec.CommandContext(ctx, bin,
		"--server", rawLn.Addr().String(), "--source", srcLn.Addr().String(),
		"--station", "stall-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "8", "--spool-file", spool)
	cmd2.Stderr = ferr2
	if err := cmd2.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd2.Process.Kill()
		if t.Failed() {
			t.Logf("run1 stderr:\n%s\nrun2 stderr:\n%s", ferr1.String(), ferr2.String())
		}
	})
	recovered := waitForRecoveredCount(t, ferr2)
	session2 := waitForSession(t, ferr2)
	replays, err := filepath.Glob(spool + ".replay.*")
	if err != nil || len(replays) != 1 {
		t.Fatalf("replay files = %v (%v), want exactly one", replays, err)
	}
	if err := os.Remove(replays[0]); err != nil {
		t.Fatal(err)
	}

	// Only now does the collector answer: the replay connection completes against a file
	// that no longer exists.
	out := make(chan *RawFrame, 1024)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("stall-e2e", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() { _ = srv.serve(ctx, tls.NewListener(rawLn, tc)) }()

	waitForLog(t, ferr2, "replay spool "+replays[0]+" is gone", 20*time.Second)
	want := fmt.Sprintf("%d frame(s) past seq 0 cannot be delivered", recovered)
	if !strings.Contains(ferr2.String(), want) {
		t.Errorf("retirement line does not account for the lost frames (want %q):\n%s", want, ferr2.String())
	}
	deadline := time.After(20 * time.Second)
	select {
	case f := <-out:
		if f.Session != session2 {
			t.Fatalf("first delivered frame carried session %q, want the live session %q", f.Session, session2)
		}
	case <-deadline:
		t.Fatalf("live delivery never followed the retired replay spool; stderr:\n%s", ferr2.String())
	}
}

// TestNavfeederRetiresUnreadableSpools covers the retryable class: a spool file that
// exists but cannot be opened (EACCES after a chown or chmod) is retried, but only for
// REPLAY_RETIRE_STUCK_S — here 1 s through a compile-time override — after which a replay
// spool is retired and the live spool's disk tier is disabled for the run, both with the
// file preserved for the next start, so delivery of everything else resumes instead of a
// connection that only ever sends PINGs.
func TestNavfeederRetiresUnreadableSpools(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not apply to root, so EACCES cannot be provoked")
	}
	bin := feederBinaryWithDefines(t, "navfeeder-fast-retire", "-DREPLAY_RETIRE_STUCK_S=1")
	capBytes := syntheticSFRBXCapture(40, 0)

	t.Run("replay spool", func(t *testing.T) {
		spool := filepath.Join(t.TempDir(), "spool.bin")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		srcLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer srcLn.Close()
		go serveFakeReceiver(ctx, srcLn, capBytes)
		rawLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer rawLn.Close()

		ferr1 := &syncBuf{}
		cmd1 := exec.Command(bin,
			"--server", rawLn.Addr().String(), "--source", srcLn.Addr().String(),
			"--station", "stall-e2e", "--token", "s3cret", "--feed", "ubx",
			"--insecure", "--spool", "8", "--spool-file", spool)
		cmd1.Stderr = ferr1
		if err := cmd1.Start(); err != nil {
			t.Fatal(err)
		}
		waitForSpoolSize(t, spool, spoolHeaderLen+32*spoolRecordOnDisk, ferr1)
		if err := cmd1.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if _, err := cmd1.Process.Wait(); err != nil {
			t.Fatal(err)
		}

		ferr2 := &syncBuf{}
		cmd2 := exec.CommandContext(ctx, bin,
			"--server", rawLn.Addr().String(), "--source", srcLn.Addr().String(),
			"--station", "stall-e2e", "--token", "s3cret", "--feed", "ubx",
			"--insecure", "--spool", "8", "--spool-file", spool)
		cmd2.Stderr = ferr2
		if err := cmd2.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd2.Process.Kill()
			if t.Failed() {
				t.Logf("run1 stderr:\n%s\nrun2 stderr:\n%s", ferr1.String(), ferr2.String())
			}
		})
		recovered := waitForRecoveredCount(t, ferr2)
		session2 := waitForSession(t, ferr2)
		replays, err := filepath.Glob(spool + ".replay.*")
		if err != nil || len(replays) != 1 {
			t.Fatalf("replay files = %v (%v), want exactly one", replays, err)
		}
		if err := os.Chmod(replays[0], 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(replays[0], 0o600) })

		out := make(chan *RawFrame, 1024)
		tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
		srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("stall-e2e", "s3cret", "ubx"),
			25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
		go func() { _ = srv.serve(ctx, tls.NewListener(rawLn, tc)) }()

		waitForLog(t, ferr2, "replay spool "+replays[0]+" unreadable for 1 s (Permission denied); retiring it", 20*time.Second)
		want := fmt.Sprintf("%d frame(s) past seq 0 stay in the file", recovered)
		if !strings.Contains(ferr2.String(), want) {
			t.Errorf("retirement line does not account for the retained frames (want %q):\n%s", want, ferr2.String())
		}
		if fi, err := os.Stat(replays[0]); err != nil || fi.Size() == 0 {
			t.Errorf("the unreadable replay file was not preserved for the next start (%v)", err)
		}
		select {
		case f := <-out:
			if f.Session != session2 {
				t.Fatalf("first delivered frame carried session %q, want the live session %q", f.Session, session2)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("live delivery never followed the retired replay spool; stderr:\n%s", ferr2.String())
		}
	})

	t.Run("live spool", func(t *testing.T) {
		spool := filepath.Join(t.TempDir(), "spool.bin")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		srcLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer srcLn.Close()
		go serveFakeReceiver(ctx, srcLn, capBytes)
		rawLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer rawLn.Close()

		ferr := &syncBuffer{}
		cmd := exec.CommandContext(ctx, bin,
			"--server", rawLn.Addr().String(), "--source", srcLn.Addr().String(),
			"--station", "stall-e2e", "--token", "s3cret", "--feed", "ubx",
			"--insecure", "--spool", "8", "--spool-file", spool)
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
		waitForSpoolSize(t, spool, spoolHeaderLen+32*spoolRecordOnDisk, ferr)
		if err := os.Chmod(spool, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(spool, 0o600) })

		out := make(chan *RawFrame, 1024)
		tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
		srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("stall-e2e", "s3cret", "ubx"),
			25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
		go func() { _ = srv.serve(ctx, tls.NewListener(rawLn, tc)) }()

		waitForLog(t, ferr, "disk spool "+spool+" unreadable for 1 s (Permission denied); disk overflow disabled for this run", 20*time.Second)
		if !strings.Contains(ferr.String(), "32 frame(s) past seq 0 stay in the file") {
			t.Errorf("the disable line does not account for the retained frames:\n%s", ferr.String())
		}
		var got []uint64
		deadline := time.After(20 * time.Second)
		for len(got) < 8 {
			select {
			case f := <-out:
				got = append(got, f.Seq)
			case <-deadline:
				t.Fatalf("only %d/8 ring frames reached the collector after the spool became unreadable; stderr:\n%s",
					len(got), ferr.String())
			}
		}
		for i, seq := range got {
			if seq != uint64(33+i) {
				t.Fatalf("ring delivery resumed with seqs %v, want 33..40 in order", got)
			}
		}
	})
}

// TestNavfeederResetsVanishedLiveSpool: the live spool file is removed while it holds
// frames pending delivery. The writer still holds the unlinked inode, so evictions kept
// "succeeding" into a file no reader could open, disk_max_seq stayed ahead of what was
// sent, and the consumer discarded every ring batch forever — a connection carrying
// nothing but PINGs. The disk accounting must be reset (the lost frames counted and
// logged) so the ring resumes delivery.
func TestNavfeederResetsVanishedLiveSpool(t *testing.T) {
	bin := feederBinary(t)
	capBytes := syntheticSFRBXCapture(40, 0)
	spool := filepath.Join(t.TempDir(), "spool.bin")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srcLn.Close()
	go serveFakeReceiver(ctx, srcLn, capBytes)

	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rawLn.Close()

	ferr := &syncBuffer{}
	cmd := exec.CommandContext(ctx, bin,
		"--server", rawLn.Addr().String(), "--source", srcLn.Addr().String(),
		"--station", "stall-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "8", "--spool-file", spool)
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
	// 40 frames through an 8-frame ring: 32 evicted to disk, the newest 8 in RAM.
	waitForSpoolSize(t, spool, spoolHeaderLen+32*spoolRecordOnDisk, ferr)
	if err := os.Remove(spool); err != nil {
		t.Fatal(err)
	}

	out := make(chan *RawFrame, 1024)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("stall-e2e", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() { _ = srv.serve(ctx, tls.NewListener(rawLn, tc)) }()

	waitForLog(t, ferr, "disk spool "+spool+" is gone", 20*time.Second)
	if !strings.Contains(ferr.String(), "32 frame(s) past seq 0 lost (counted as disk_dropped)") {
		t.Errorf("reset line does not account for the lost frames:\n%s", ferr.String())
	}
	var got []uint64
	deadline := time.After(20 * time.Second)
	for len(got) < 8 {
		select {
		case f := <-out:
			got = append(got, f.Seq)
		case <-deadline:
			t.Fatalf("only %d/8 ring frames reached the collector after the spool vanished; stderr:\n%s",
				len(got), ferr.String())
		}
	}
	for i, seq := range got {
		if seq != uint64(33+i) {
			t.Fatalf("ring delivery resumed with seqs %v, want 33..40 in order", got)
		}
	}
}
