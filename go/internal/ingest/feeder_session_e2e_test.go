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
	"regexp"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuf is a mutex-guarded bytes.Buffer: these tests read the feeder's stderr
// WHILE os/exec's copier goroutine writes it (the other e2e tests only read it
// after Kill), so unguarded access would be a data race.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var sessionLogRe = regexp.MustCompile(`navfeeder: session ([0-9a-f]{32})`)

// waitForSession polls the feeder's stderr for the startup "session <hex32>" line.
func waitForSession(t *testing.T, ferr *syncBuf) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m := sessionLogRe.FindStringSubmatch(ferr.String()); m != nil {
			return m[1]
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("feeder never logged its session; stderr:\n%s", ferr.String())
	return ""
}

// TestNavfeederSessionContinuityAcrossRestart drives the regression fix edge
// contract end to end with the real C feeder:
//
//  1. Run the feeder against a DOWN collector so everything it captures lands in
//     the ring/disk spool, and record the session it minted.
//  2. SIGTERM it (the regression fix shutdown flush spills the unacked ring to disk).
//  3. Restart it against a LIVE collector: spool_recover must ADOPT the stored
//     session — the replayed and post-restart frames continue one
//     (observer, session, seq) space — which the collector observes on every
//     admitted frame.
//
// Without the spool-header adoption, the restart would mint a fresh session for
// records that belong to the old sequence space (or, pre-regression fix, reuse
// (observer, seq) and have the historian discard fresh frames as replays).
func TestNavfeederSessionContinuityAcrossRestart(t *testing.T) {
	bin := feederBinary(t)
	capPath := filepath.Join("testdata", "f9t_capture.ubx")
	expected := scanCaptureNavFrames(t, capPath)
	if len(expected) < 10 {
		t.Fatalf("capture yielded only %d nav frames; expected many", len(expected))
	}
	capBytes, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(t.TempDir(), "spool.bin")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A fake receiver, shared by both runs: streams the capture once per connect.
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

	// Reserve a collector address that is DOWN for run 1: bind, note the port, close.
	downLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	collectorAddr := downLn.Addr().String()
	_ = downLn.Close()

	// Run 1: capture into the spool with nowhere to deliver.
	ferr1 := &syncBuf{}
	cmd1 := exec.Command(bin,
		"--server", collectorAddr,
		"--source", srcLn.Addr().String(),
		"--station", "f9t-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "8", "--spool-file", spool)
	cmd1.Stderr = ferr1
	if err := cmd1.Start(); err != nil {
		t.Fatal(err)
	}
	killed1 := false
	defer func() {
		if !killed1 {
			_ = cmd1.Process.Kill()
		}
	}()
	session1 := waitForSession(t, ferr1)

	// Wait until the ring (8 frames) has demonstrably overflowed to disk, then
	// stop run 1 gracefully so the shutdown flush spills the ring tail too.
	sizeDeadline := time.Now().Add(10 * time.Second)
	for {
		if fi, err := os.Stat(spool); err == nil && fi.Size() > 1024 {
			break
		}
		if time.Now().After(sizeDeadline) {
			t.Fatalf("spool never grew; stderr:\n%s", ferr1.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := cmd1.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd1.Process.Wait(); err != nil {
		t.Fatal(err)
	}
	killed1 = true

	// Run 2: a live collector on a fresh port; the feeder recovers the spool.
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

	ferr2 := &syncBuf{}
	cmd2 := exec.CommandContext(ctx, bin,
		"--server", pushLn.Addr().String(),
		"--source", srcLn.Addr().String(),
		"--station", "f9t-e2e", "--token", "s3cret", "--feed", "ubx",
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

	// The restart must ADOPT run 1's session, not mint a fresh one.
	if session2 := waitForSession(t, ferr2); session2 != session1 {
		t.Fatalf("restart minted session %s; want the spool header's %s adopted", session2, session1)
	}

	// The recovered frames replay into the collector carrying that same session.
	deadline := time.After(20 * time.Second)
	for n := 0; n < len(expected); n++ {
		select {
		case f := <-out:
			if f.Session != session1 {
				t.Fatalf("frame %d carried session %q, want %q", n, f.Session, session1)
			}
		case <-deadline:
			t.Fatalf("only %d/%d frames reached the collector after restart", n, len(expected))
		}
	}
}

// TestNavfeederDiscardsHeaderlessSpool pins the documented no-compat rule:
// a spool file without the regression fix session header (a pre-session build's format,
// or corruption) is discarded — logged, unlinked, fresh session — rather than
// misparsed into the new sequence space.
func TestNavfeederDiscardsHeaderlessSpool(t *testing.T) {
	bin := feederBinary(t)
	spool := filepath.Join(t.TempDir(), "spool.bin")
	// A plausible OLD-format spool: records with no header (8B seq + 4B len + payload).
	legacy := []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 4, 0xde, 0xad, 0xbe, 0xef}
	if err := os.WriteFile(spool, legacy, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
		<-ctx.Done()
	}()

	ferr := &syncBuf{}
	cmd := exec.CommandContext(ctx, bin,
		"--server", "127.0.0.1:1", // never reached before the check below
		"--source", srcLn.Addr().String(),
		"--station", "f9t-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "8", "--spool-file", spool)
	cmd.Stderr = ferr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	deadline := time.Now().Add(10 * time.Second)
	for {
		if bytes.Contains([]byte(ferr.String()), []byte("lacks a valid session header")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("headerless spool was not rejected; stderr:\n%s", ferr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	// The incompatible file is unlinked, and a fresh session was still minted.
	if _, err := os.Stat(spool); !os.IsNotExist(err) {
		t.Errorf("headerless spool still exists (stat err %v); want unlinked", err)
	}
	waitForSession(t, ferr)
}
