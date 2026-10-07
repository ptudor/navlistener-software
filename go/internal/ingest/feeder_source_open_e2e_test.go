package ingest

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestNavfeederSourceOpenFailureNamesTheCause: an unplugged device (ENOENT), a device node
// the service account cannot open (EACCES — the --configure-ubx read/write case) and a
// refused TCP bridge all used to print the same "source open failed" line. A field
// operator must be able to tell a cabling fault from a permission fault from a dead
// bridge, so the producer's retry line names the failing step and its errno.
func TestNavfeederSourceOpenFailureNamesTheCause(t *testing.T) {
	bin := feederBinary(t)
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "ttyLOCKED")
	if err := os.WriteFile(unreadable, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	refused, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedAddr := refused.Addr().String()
	_ = refused.Close()

	for _, tc := range []struct {
		name   string
		source string
		extra  []string
		want   string
	}{
		{"missing device", filepath.Join(dir, "ttyUNPLUGGED"), nil, "open: No such file or directory"},
		{"unreadable device node with --configure-ubx", unreadable, []string{"--configure-ubx", "uart1"},
			"open for read/write (--configure-ubx): Permission denied"},
		{"refused tcp bridge", refusedAddr, nil, "connecting to " + refusedAddr + ": Connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.extra != nil && os.Geteuid() == 0 {
				t.Skip("file permissions do not apply to root, so EACCES cannot be provoked")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ferr := &syncBuffer{}
			args := []string{"--server", "127.0.0.1:1", "--source", tc.source,
				"--station", "src-e2e", "--token", "s3cret", "--feed", "ubx", "--insecure", "--spool", "16"}
			cmd := exec.CommandContext(ctx, bin, append(args, tc.extra...)...)
			cmd.Stderr = ferr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			waitForLog(t, ferr, "source open failed ("+tc.source+"): "+tc.want+"; retry in", 10*time.Second)
		})
	}
}
