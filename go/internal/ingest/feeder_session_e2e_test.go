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
	"strconv"
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
var recoveredLogRe = regexp.MustCompile(`recovered disk spool: (\d+) frame`)

// waitForRecoveredCount polls the feeder's stderr for the "recovered disk
// spool: N frame(s)" line a restarted process logs for its replay file.
func waitForRecoveredCount(t *testing.T, ferr *syncBuf) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m := recoveredLogRe.FindStringSubmatch(ferr.String()); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("feeder never logged a recovered spool; stderr:\n%s", ferr.String())
	return 0
}

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

// TestNavfeederSessionContinuityAcrossRestart verifies that retained records keep
// their original identity while the restarted producer gets a new session.
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

	// New captures must use a fresh session; replay retains run 1's identity.
	session2 := waitForSession(t, ferr2)
	if session2 == session1 {
		t.Fatalf("restart reused session %s; retained session %s must be replay-only", session2, session1)
	}

	// The recovered frames replay into the collector carrying that same
	// session, on their own connection, before run 2's live capture follows
	// under the fresh one. Run 1 was stopped as soon as its ring had spilled,
	// so how much it captured is whatever run 2 recovered — read that count
	// from the recovery log rather than assuming the whole capture was spooled.
	recovered := waitForRecoveredCount(t, ferr2)
	if recovered < 8 || recovered > len(expected) {
		t.Fatalf("recovered %d frames, want between the 8-frame ring and the %d-frame capture", recovered, len(expected))
	}
	deadline := time.After(20 * time.Second)
	for n := 0; n < recovered; n++ {
		select {
		case f := <-out:
			if f.Session != session1 {
				t.Fatalf("replayed frame %d carried session %q, want %q", n, f.Session, session1)
			}
		case <-deadline:
			t.Fatalf("only %d/%d recovered frames reached the collector after restart", n, recovered)
		}
	}
	select {
	case f := <-out:
		if f.Session != session2 {
			t.Fatalf("first live frame after replay carried session %q, want %q", f.Session, session2)
		}
	case <-deadline:
		t.Fatal("live capture never followed the replayed session")
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

// TestNavfeederEmptySpoolKeepsFreshSession pins the fix: a spool file
// with a VALID session header but no complete record (a kill or power cut
// between the header flush and the first record flush, or a torn first append
// rolled back to the bare header) must NOT adopt the stored session. Adoption
// with s->seq still 0 would reconnect the feeder as the previous session with
// a sequence space restarting at 1 — the exact regression fix collision where the
// collector's (observer, session, seq) ledger classifies fresh frames as
// replays and silently drops them. There is nothing to replay from such a
// spool, so the freshly-minted session keeps a clean sequence space.
func TestNavfeederEmptySpoolKeepsFreshSession(t *testing.T) {
	bin := feederBinary(t)
	const stored = "deadbeefdeadbeefdeadbeefdeadbeef"

	// Two variants of the regression fix spool: a bare 73-byte header, and a header
	// followed by a torn partial record header (6 of 12 bytes).
	for _, tc := range []struct {
		name string
		tail []byte
	}{
		{"bare_header", nil},
		{"torn_first_record", []byte{0, 0, 0, 0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spool := filepath.Join(t.TempDir(), "spool.bin")
			hdr := make([]byte, 0, 73+len(tc.tail))
			hdr = append(hdr, []byte("NAVSPO01")...)
			hdr = append(hdr, byte(len(stored)))
			sess := make([]byte, 64)
			copy(sess, stored)
			hdr = append(hdr, sess...)
			hdr = append(hdr, tc.tail...)
			if err := os.WriteFile(spool, hdr, 0o644); err != nil {
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
				if bytes.Contains([]byte(ferr.String()), []byte("no complete record")) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("record-less spool was not discarded; stderr:\n%s", ferr.String())
				}
				time.Sleep(25 * time.Millisecond)
			}
			// The record-less file is unlinked, and the session in use is the
			// fresh mint, NOT the stored one from the discarded header.
			if _, err := os.Stat(spool); !os.IsNotExist(err) {
				t.Errorf("record-less spool still exists (stat err %v); want unlinked", err)
			}
			if got := waitForSession(t, ferr); got == stored {
				t.Errorf("feeder adopted the stored session %q from a record-less spool; want a fresh mint", got)
			}
		})
	}
}
