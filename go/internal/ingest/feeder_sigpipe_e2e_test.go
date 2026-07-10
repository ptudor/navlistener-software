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
	"syscall"
	"testing"
	"time"
)

// capturingListener wraps a plain TCP listener and performs the TLS handshake itself
// (like tls.Listener), but also stashes each accepted *net.TCPConn so the test can force
// an abortive RST close on it later -- simulating "the collector process died mid-write"
// independent of whatever the TLS/application layer believes about the connection's state.
type capturingListener struct {
	net.Listener
	tlsConfig *tls.Config
	raws      chan *net.TCPConn
}

func (l *capturingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tcp, ok := c.(*net.TCPConn); ok {
		select {
		case l.raws <- tcp:
		default:
		}
	}
	return tls.Server(c, l.tlsConfig), nil
}

// TestNavfeederSurvivesCollectorRST guards a write landing on a connection the
// collector has already abortively closed (an ordinary collector restart/deploy, or a
// crash) must not kill navfeeder outright -- it must log the write failure and reconnect.
// Run as an A/B: the pre-fix navfeeder.c (built fresh from the commit before
// `signal(SIGPIPE, SIG_IGN)` was added) is expected to die to signal 13; the current
// tree's binary must survive and reconnect. This also confirms the harness itself
// actually reproduces the write-after-RST scenario (the pre-fix subtest would fail with
// a clear message if it didn't).
func TestNavfeederSurvivesCollectorRST(t *testing.T) {
	t.Run("pre-fix binary dies to SIGPIPE", func(t *testing.T) {
		bin := buildPrefixNavfeeder(t)
		alive, sig, reconnected := runSIGPIPEHarness(t, bin, 8*time.Second)
		if alive {
			t.Fatal("pre-fix navfeeder survived the RST; expected it to die to SIGPIPE (harness assumption broken)")
		}
		if sig != syscall.SIGPIPE {
			t.Fatalf("pre-fix navfeeder died to signal %v, want SIGPIPE -- harness may not be reproducing the write-after-RST case", sig)
		}
		if reconnected {
			t.Fatal("pre-fix navfeeder reconnected despite dying -- test bookkeeping bug")
		}
	})
	t.Run("fixed binary survives and reconnects", func(t *testing.T) {
		bin := feederBinary(t)
		alive, _, reconnected := runSIGPIPEHarness(t, bin, 8*time.Second)
		if !alive {
			t.Fatal("fixed navfeeder died; regression fix regression")
		}
		if !reconnected {
			t.Fatal("fixed navfeeder survived but never reconnected to the collector")
		}
	})
}

// runSIGPIPEHarness stands up a real PushServer (the actual collector code, via
// newPushServer/serve -- same as feeder_e2e_test.go) behind a capturingListener, runs bin
// against it with a one-shot fake receiver streaming a real capture, waits for frames to
// flow, then abortively closes (SO_LINGER 0, forcing an RST rather than a FIN) the
// server's accepted connection -- simulating the collector process dying mid-write.
// Returns whether the feeder process was still alive after wait, the signal it died to
// (if not alive), and whether a second connection reached the collector afterward
// (reconnect succeeded).
func runSIGPIPEHarness(t *testing.T, bin string, wait time.Duration) (alive bool, sig syscall.Signal, reconnected bool) {
	t.Helper()
	capPath := filepath.Join("testdata", "f9t_capture.ubx")
	capBytes, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan *RawFrame, 8192)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("sigpipe-e2e", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rawLn.Close()
	cl := &capturingListener{Listener: rawLn, tlsConfig: tc, raws: make(chan *net.TCPConn, 4)}
	go func() { _ = srv.serve(ctx, cl) }()

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
	args := []string{
		"--server", rawLn.Addr().String(),
		"--source", srcLn.Addr().String(),
		"--station", "sigpipe-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "100000",
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = &ferr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if t.Failed() && ferr.Len() > 0 {
			t.Logf("navfeeder stderr:\n%s", ferr.String())
		}
	})

	// Wait for the first connection and for frames to actually start flowing before
	// pulling the rug -- a write-after-RST needs at least one successful write first so
	// the kernel has something in flight when the RST arrives.
	var first *net.TCPConn
	select {
	case first = <-cl.raws:
	case <-time.After(5 * time.Second):
		t.Fatal("collector never saw a connection")
	}
	drained := 0
	deadline := time.After(5 * time.Second)
	for drained < 5 {
		select {
		case <-out:
			drained++
		case <-deadline:
			t.Fatal("frames never started flowing to the collector")
		case err := <-done:
			t.Fatalf("navfeeder exited before any frames flowed: %v\nstderr:\n%s", err, ferr.String())
		}
	}

	// Simulate the collector process dying mid-write: abortive close sends RST instead
	// of FIN, so the feeder's next write (not just its next read) fails hard.
	_ = first.SetLinger(0)
	_ = first.Close()

	select {
	case err := <-done:
		alive = false
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				sig = ws.Signal()
			}
		}
		return alive, sig, false
	case <-time.After(wait):
	}

	// Still alive: confirm it actually reconnected (a second connection reaches the
	// collector), not just that some other goroutine kept the process technically running.
	select {
	case <-cl.raws:
		reconnected = true
	case <-time.After(wait):
		reconnected = false
	}
	return true, 0, reconnected
}

// buildPrefixNavfeeder compiles feeder/navfeeder.c as it existed at the last commit
// (before this finding's `signal(SIGPIPE, SIG_IGN)` fix) into a temp binary, so the
// SIGPIPE harness can be run against the pre-fix behavior for an A/B comparison. Skips
// the test if `cc`/git aren't available rather than failing the whole suite.
func buildPrefixNavfeeder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "navfeeder_prefix.c")
	src, err := exec.Command("git", "show", "HEAD:feeder/navfeeder.c").Output()
	if err != nil {
		t.Skipf("git show HEAD:feeder/navfeeder.c failed: %v", err)
	}
	if err := os.WriteFile(srcPath, src, 0o644); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(dir, "navfeeder_prefix")
	args := []string{"-O2", "-Wall", "-Wextra", "-std=c11"}
	if _, err := os.Stat("/opt/local/include"); err == nil {
		args = append(args, "-I/opt/local/include", "-L/opt/local/lib")
	}
	args = append(args, "-o", binPath, srcPath, "-lssl", "-lcrypto", "-lzstd", "-lpthread")
	cmd := exec.Command("cc", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("building pre-fix navfeeder failed: %v\n%s", err, out)
	}
	return binPath
}
