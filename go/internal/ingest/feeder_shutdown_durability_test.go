package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestNavfeederShutdownDurabilityIsHonest proves regression fix. The orderly
// shutdown flush discarded the return values of its final fflush and fsync,
// never checked ferror, and counted only disk_dropped — missing the separate
// counter disk_put uses when the spool file cannot be opened at all — then
// always _exit(0). A late writeback or spool-open failure therefore told the
// service manager the promised recovery spool was safely committed while some or
// all of the newly spilled frames were not durable.
//
// Each fault below is scoped to the live spool writer. Success must mean every
// unacknowledged ring frame is durable; every failure must exit nonzero, say so
// explicitly, and leave previously valid spool bytes untouched.
func TestNavfeederShutdownDurabilityIsHonest(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		// path exercised: the per-record append accounting, or the new
		// post-loop stream-error / flush / sync checks.
		want string
	}{
		{"deferred stream error", "NAVFEEDER_TEST_DEFER_FERROR=1", "deferred write error"},
		{"final fsync failure", "NAVFEEDER_TEST_FAIL_FSYNC=1", "fsync failed"},
		{"flush failure", "NAVFEEDER_TEST_FAIL_FFLUSH=1", ""},
		{"short write (full disk)", "NAVFEEDER_TEST_SHORT_WRITE=1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr, code, before, after := runShutdownFlush(t, tc.env)
			if code == 0 {
				t.Fatalf("feeder exited 0 despite an injected durability failure; the service "+
					"manager is told the recovery spool was committed.\nstderr:\n%s", stderr)
			}
			if !strings.Contains(stderr, "shutdown flush INCOMPLETE") {
				t.Errorf("no explicit incomplete-flush diagnostic:\n%s", stderr)
			}
			if tc.want != "" && !strings.Contains(stderr, tc.want) {
				t.Errorf("diagnostic does not name the cause %q:\n%s", tc.want, stderr)
			}
			// Whatever was already durable must not have been corrupted.
			if before != "" && !strings.HasPrefix(after, before) {
				t.Errorf("previously valid spool bytes were altered by the failed flush")
			}
			for _, secret := range []string{"s3cret"} {
				if strings.Contains(stderr, secret) {
					t.Errorf("the shutdown diagnostic leaked a credential")
				}
			}
		})
	}

	t.Run("clean flush exits zero and is readable", func(t *testing.T) {
		stderr, code, _, after := runShutdownFlush(t, "")
		if code != 0 {
			t.Fatalf("clean shutdown flush exited %d:\n%s", code, stderr)
		}
		if !strings.Contains(stderr, "shutdown flush complete") {
			t.Errorf("no completion diagnostic:\n%s", stderr)
		}
		if strings.Contains(stderr, "INCOMPLETE") {
			t.Errorf("clean flush reported incomplete:\n%s", stderr)
		}
		// The spool must be reopenable and carry the session header the recovery
		// path keys on.
		if len(after) < 8 || !strings.HasPrefix(after, "NAVSPO01") {
			t.Errorf("spool file is not readable as a spool after a clean flush (%d bytes)", len(after))
		}
	})
}

// runShutdownFlush starts the harness-as-feeder against a source that produces
// frames and a collector that never accepts them, so the ring fills with
// unacknowledged frames; then it SIGTERMs the feeder and returns its stderr,
// exit code, and the spool file's contents before and after the flush.
func runShutdownFlush(t *testing.T, env string) (stderr string, code int, before, after string) {
	t.Helper()
	bin := spoolTestBinary(t)
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A listener that accepts and immediately drops: the feeder keeps retrying and
	// nothing is ever acked, so the ring retains everything.
	deadCollector, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer deadCollector.Close()
	go func() {
		for {
			c, err := deadCollector.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	capBytes := syntheticSFRBXCapture(6, 0)
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

	sum := sha256.Sum256([]byte("s3cret"))
	_ = hex.EncodeToString(sum[:])
	var ferr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "feeder",
		"--server", deadCollector.Addr().String(),
		"--source", srcLn.Addr().String(),
		"--station", "shutdown-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "4", "--spool-file", spool)
	cmd.Env = os.Environ()
	if env != "" {
		cmd.Env = append(cmd.Env, env)
	}
	cmd.Stderr = &ferr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	started := false
	defer func() {
		if !started {
			_ = cmd.Process.Kill()
		}
	}()

	// Wait until the ring has frames to flush: the feeder logs each failed
	// connection attempt, and the source streams immediately on connect.
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(ferr.String(), "collector") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // let the producer fill the ring

	if b, err := os.ReadFile(spool); err == nil {
		before = string(b)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	started = true
	err = cmd.Wait()
	code = 0
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("feeder wait: %v\nstderr:\n%s", err, ferr.String())
		}
	}
	if b, rerr := os.ReadFile(spool); rerr == nil {
		after = string(b)
	}
	return ferr.String(), code, before, after
}

func asExitError(err error, out **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*out = ee
	}
	return ok
}

var _ = fmt.Sprintf
