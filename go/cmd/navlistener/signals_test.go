package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedBuffer collects a child's output while it is still being written.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuffer) waitFor(t *testing.T, needle string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(l.String(), needle) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%q not seen within %v; output:\n%s", needle, within, l.String())
}

// TestDaemonIgnoresHUPAndDrainsOnTERM runs the built collector with a minimal
// configuration (no sources, no historian), confirms a SIGHUP does not end it,
// and that a SIGTERM runs the ordered shutdown to "graceful shutdown complete"
// with exit status 0. It is the behavioural half of the startup-order test:
// that one pins where the signal disposition is installed, this one that the
// disposition is what the rc.d script and newsyslog rely on.
func TestDaemonIgnoresHUPAndDrainsOnTERM(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "navlistener")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// A free loopback port for the metrics listener; port 0 is refused by the
	// configuration (an ephemeral listener is never what an operator means).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfgPath := filepath.Join(dir, "navlistener.toml")
	if err := os.WriteFile(cfgPath, []byte("[metrics]\naddr = \""+addr+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-config", cfgPath)
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	out.waitFor(t, `"msg":"ready"`, 15*time.Second)

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("SIGHUP ended the collector: %v\n%s", err, out.String())
	case <-time.After(500 * time.Millisecond):
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SIGTERM exit: %v\n%s", err, out.String())
		}
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("collector did not exit after SIGTERM\n%s", out.String())
	}
	for _, needle := range []string{`"msg":"shutdown signal"`, `"msg":"graceful shutdown complete"`} {
		if !strings.Contains(out.String(), needle) {
			t.Errorf("%s missing from the collector's output:\n%s", needle, out.String())
		}
	}
	if strings.Contains(out.String(), `"msg":"shutdown incomplete"`) {
		t.Errorf("shutdown reported incomplete:\n%s", out.String())
	}
}
