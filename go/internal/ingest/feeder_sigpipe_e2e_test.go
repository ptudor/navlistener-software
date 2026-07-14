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
	"sync"
	"sync/atomic"
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

// TestNavfeederCorruptTLSRecordReconnects exercises the regression fix failure path: an on-path byte
// flip in collector→feeder TLS traffic makes SSL_read dispatch a fatal alert while DATA is
// flowing. The shared SSL mutex must turn that into an ordinary reconnect, not a crash or a
// concurrent read/write/shutdown access. Run this test with a -fsanitize=thread feeder build
// for the race-detector half of the verification.
func TestNavfeederCorruptTLSRecordReconnects(t *testing.T) {
	bin := feederBinary(t)
	capBytes, err := os.ReadFile(filepath.Join("testdata", "f9t_capture.ubx"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan *RawFrame, 8192)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("tls-race-e2e", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	upstreamLn, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamLn.Close()
	go func() { _ = srv.serve(ctx, upstreamLn) }()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()
	var corruptNext atomic.Bool
	corrupted := make(chan struct{}, 1)
	accepted := make(chan struct{}, 4)
	go runTLSCorruptingProxy(ctx, proxyLn, upstreamLn.Addr().String(), &corruptNext, corrupted, accepted)

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
	cmd := exec.CommandContext(ctx, bin,
		"--server", proxyLn.Addr().String(), "--source", srcLn.Addr().String(),
		"--station", "tls-race-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "100000")
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

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("TLS proxy never saw the first feeder connection")
	}
	for i := 0; i < 5; i++ {
		select {
		case <-out:
		case err := <-done:
			t.Fatalf("navfeeder exited before corruption: %v\nstderr:\n%s", err, ferr.String())
		case <-time.After(5 * time.Second):
			t.Fatal("frames never started flowing through the TLS proxy")
		}
	}
	corruptNext.Store(true)
	select {
	case <-corrupted:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy saw no collector→feeder TLS record to corrupt")
	}
	select {
	case err := <-done:
		t.Fatalf("navfeeder crashed after corrupt TLS record: %v\nstderr:\n%s", err, ferr.String())
	case <-accepted:
		// A second proxy accept proves clean teardown and reconnect/replay.
	case <-time.After(8 * time.Second):
		t.Fatal("navfeeder did not reconnect after corrupt TLS record")
	}
}

func runTLSCorruptingProxy(ctx context.Context, ln net.Listener, upstreamAddr string,
	corruptNext *atomic.Bool, corrupted chan<- struct{}, accepted chan<- struct{}) {
	for {
		downstream, err := ln.Accept()
		if err != nil {
			return
		}
		upstream, err := net.Dial("tcp", upstreamAddr)
		if err != nil {
			downstream.Close()
			continue
		}
		select {
		case accepted <- struct{}{}:
		default:
		}
		go func() {
			<-ctx.Done()
			downstream.Close()
			upstream.Close()
		}()
		go proxyTLSConnection(downstream, upstream, corruptNext, corrupted)
	}
}

func proxyTLSConnection(downstream, upstream net.Conn, corruptNext *atomic.Bool, corrupted chan<- struct{}) {
	var closeOnce sync.Once
	closeBoth := func() { downstream.Close(); upstream.Close() }
	go func() {
		_, _ = io.Copy(upstream, downstream)
		closeOnce.Do(closeBoth)
	}()
	buf := make([]byte, 32*1024)
	for {
		n, err := upstream.Read(buf)
		if n > 0 {
			if corruptNext.CompareAndSwap(true, false) {
				buf[n/2] ^= 0x80
				select {
				case corrupted <- struct{}{}:
				default:
				}
			}
			if _, werr := downstream.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	closeOnce.Do(closeBoth)
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
// crash) must not kill navfeeder outright -- it must log the write failure and reconnect
// instead of dying to the default SIGPIPE disposition.
//
// The regression asserts that the current feeder survives a collector reset
// and reconnects after a failed write.
func TestNavfeederSurvivesCollectorRST(t *testing.T) {
	bin := feederBinary(t)
	alive, _, reconnected := runSIGPIPEHarness(t, bin, 8*time.Second)
	if !alive {
		t.Fatal("navfeeder died on a write to an RST'd collector connection; regression fix regression")
	}
	if !reconnected {
		t.Fatal("navfeeder survived the RST but never reconnected to the collector")
	}
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
