package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

// TestNavfeederPresentsTokensByteForByte proves regression fix. The feeder read a
// token file through one 512-byte fgets and then escaped it into another
// 512-byte buffer that truncated silently, so a credential the collector
// explicitly accepts and tests (900 bytes, TestPushHelloMaxSizeStillAuthenticates)
// could never authenticate through this feeder — it presented a DIFFERENT,
// shortened credential and produced an endless "unauthorized" reconnect loop with
// no local diagnostic.
func TestNavfeederPresentsTokensByteForByte(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"511", strings.Repeat("a", 511)},
		{"512 (the old fgets boundary)", strings.Repeat("b", 512)},
		{"900 (the collector's tested size)", strings.Repeat("c", 900)},
		{"1023", strings.Repeat("d", 1023)},
		{"1024 (the documented maximum)", strings.Repeat("e", 1024)},
		{"json-significant bytes", `tok"with\backslash and "quotes"`},
		{"embedded whitespace", "tok with inner spaces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !runFeederWithToken(t, tc.token, tc.token+"\n") {
				t.Fatalf("token of %d bytes did not authenticate byte-for-byte", len(tc.token))
			}
		})
	}
}

// Unsupported input must fail locally, at startup, instead of opening a
// reconnect loop — and must never appear in diagnostics.
func TestNavfeederRejectsUnpresentableTokensAtStartup(t *testing.T) {
	secret := strings.Repeat("z", 1025) // one past the documented maximum
	for _, tc := range []struct{ name, fileBody, wantMsg string }{
		{"token longer than the maximum", secret + "\n", "longer than the supported maximum"},
		{"content after the first line", "goodtoken\nsecond line\n", "content after the first line"},
		{"empty file", "", "empty --token-file"},
		{"whitespace-only file", "   \n", "empty --token-file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "token")
			if err := os.WriteFile(path, []byte(tc.fileBody), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, feederBinary(t),
				"--server", "127.0.0.1:1", "--source", "127.0.0.1:1",
				"--station", "tok-e2e", "--token-file", path, "--feed", "ubx",
				"--insecure").CombinedOutput()
			if err == nil {
				t.Fatalf("feeder started with an unpresentable credential:\n%s", out)
			}
			if !strings.Contains(string(out), tc.wantMsg) {
				t.Errorf("diagnostic %q does not explain the failure (want %q)", out, tc.wantMsg)
			}
			if strings.Contains(string(out), secret) || strings.Contains(string(out), "goodtoken") {
				t.Errorf("the credential appeared in the feeder's diagnostics: %s", out)
			}
		})
	}
}

// runFeederWithToken starts a collector that accepts exactly `token` and runs the
// feeder with `fileBody` in its --token-file. It reports whether a frame arrived,
// which can only happen if the presented credential matched byte-for-byte.
func runFeederWithToken(t *testing.T, token, fileBody string) bool {
	t.Helper()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(fileBody), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	auth := NewConfigAuthenticator([]config.PushObserver{
		{Station: "tok-e2e", TokenSHA256: hex.EncodeToString(sum[:]), Feeds: []string{"ubx"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan *RawFrame, 64)
	tcfg := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tcfg, out, auth, 25*time.Millisecond, 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	pushLn, err := tls.Listen("tcp", "127.0.0.1:0", tcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pushLn.Close()
	go func() { _ = srv.serve(ctx, pushLn) }()

	capBytes := syntheticSFRBXCapture(4, 0)
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

	ferr := &syncBuffer{}
	cmd := exec.CommandContext(ctx, feederBinary(t),
		"--server", pushLn.Addr().String(), "--source", srcLn.Addr().String(),
		"--station", "tok-e2e", "--token-file", tokenPath, "--feed", "ubx",
		"--insecure", "--spool", "1000")
	cmd.Stderr = ferr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	select {
	case <-out:
		return true
	case <-time.After(15 * time.Second):
		t.Logf("feeder stderr:\n%s", ferr.String())
		if strings.Contains(ferr.String(), token) {
			t.Error("the credential appeared in the feeder's diagnostics")
		}
		return false
	}
}

var _ = fmt.Sprintf
