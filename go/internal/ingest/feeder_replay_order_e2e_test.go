package ingest

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"
)

var recoveredSessionRe = regexp.MustCompile(`recovered disk spool: (\d+) frame\(s\), session ([0-9a-f]{32})`)

// recoveredSessions returns every "recovered disk spool" line's session and frame count.
func recoveredSessions(log string) map[string]int {
	m := map[string]int{}
	for _, match := range recoveredSessionRe.FindAllStringSubmatch(log, -1) {
		n, _ := strconv.Atoi(match[1])
		m[match[2]] = n
	}
	return m
}

// TestNavfeederReplaysTwoFilesBeforeLive pins the multi-file replay contract: after two
// restarts with nowhere to deliver, the third start recovers one already-archived replay
// file plus the previous run's current file, and serves each replay file WHOLE on its own
// connection — every frame of a session contiguous and in ascending seq, under that
// session's identity — before a single live frame. Earlier tests covered one replay
// file; this is the first with two, so an interleaving or a lost second file shows here.
func TestNavfeederReplaysTwoFilesBeforeLive(t *testing.T) {
	bin := feederBinary(t)
	capBytes := syntheticSFRBXCapture(30, 0)
	spool := filepath.Join(t.TempDir(), "spool.bin")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srcLn.Close()
	go serveFakeReceiver(ctx, srcLn, capBytes)

	// A collector address that is down for runs 1 and 2.
	downLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	downAddr := downLn.Addr().String()
	_ = downLn.Close()

	var sessions []string
	logs := make([]*syncBuf, 0, 2)
	for run := 1; run <= 2; run++ {
		ferr := &syncBuf{}
		logs = append(logs, ferr)
		cmd := exec.Command(bin,
			"--server", downAddr, "--source", srcLn.Addr().String(),
			"--station", "order-e2e", "--token", "s3cret", "--feed", "ubx",
			"--insecure", "--spool", "8", "--spool-file", spool)
		cmd.Stderr = ferr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, waitForSession(t, ferr))
		// 30 frames through an 8-frame ring: 22 on disk before the orderly stop.
		waitForSpoolSize(t, spool, spoolHeaderLen+22*spoolRecordOnDisk, ferr)
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.Process.Wait(); err != nil {
			t.Fatal(err)
		}
	}

	out := make(chan *RawFrame, 4096)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("order-e2e", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pushLn, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	defer pushLn.Close()
	go func() { _ = srv.serve(ctx, pushLn) }()

	ferr3 := &syncBuf{}
	cmd3 := exec.CommandContext(ctx, bin,
		"--server", pushLn.Addr().String(), "--source", srcLn.Addr().String(),
		"--station", "order-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "8", "--spool-file", spool)
	cmd3.Stderr = ferr3
	if err := cmd3.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd3.Process.Kill()
		if t.Failed() {
			for i, l := range logs {
				t.Logf("run%d stderr:\n%s", i+1, l.String())
			}
			t.Logf("run3 stderr:\n%s", ferr3.String())
		}
	})
	session3 := waitForSession(t, ferr3)
	recovered := recoveredSessions(ferr3.String())
	for _, s := range sessions {
		if recovered[s] != 30 {
			t.Fatalf("session %s recovered with %d frames, want all 30 (ring flush included); recovered=%v",
				s, recovered[s], recovered)
		}
	}

	// Every replayed frame, then the first live frame.
	want := recovered[sessions[0]] + recovered[sessions[1]]
	var got []*RawFrame
	deadline := time.After(30 * time.Second)
	for len(got) <= want {
		select {
		case f := <-out:
			got = append(got, f)
		case <-deadline:
			t.Fatalf("only %d/%d replayed frames (plus a live one) reached the collector", len(got), want+1)
		}
	}
	seen := map[string]int{}
	var order []string
	var lastSeq uint64
	for i, f := range got[:want] {
		if f.Session == session3 {
			t.Fatalf("frame %d is live (session %s) before the replay files completed", i, session3)
		}
		if seen[f.Session] == 0 {
			order = append(order, f.Session)
			lastSeq = 0
		} else if order[len(order)-1] != f.Session {
			t.Fatalf("frame %d (session %s) interleaves with %s; each replay file must be served whole", i, f.Session, order[len(order)-1])
		}
		if f.Seq <= lastSeq {
			t.Fatalf("frame %d of session %s has seq %d after %d; replay must be ascending", i, f.Session, f.Seq, lastSeq)
		}
		lastSeq = f.Seq
		seen[f.Session]++
	}
	for _, s := range sessions {
		if seen[s] != recovered[s] {
			t.Errorf("session %s delivered %d frames, want %d", s, seen[s], recovered[s])
		}
	}
	if live := got[want]; live.Session != session3 {
		t.Errorf("the frame after both replay files carried session %s, want the live session %s", live.Session, session3)
	}
	t.Logf("replay order observed: %v (run sessions %v), live session %s", order, sessions, session3)
}
