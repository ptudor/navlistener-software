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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/store"
)

func spoolTestBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "spool-test")
	args := []string{"-O1", "-g", "-Wall", "-Wextra", "-std=c11", "-D_FILE_OFFSET_BITS=64"}
	if runtime.GOOS == "darwin" {
		args = append(args, "-I/opt/local/include", "-L/opt/local/lib")
	}
	args = append(args, "-o", bin, "../../../feeder/spool_test.c", "-lssl", "-lcrypto", "-lzstd", "-lpthread")
	if out, err := exec.Command("cc", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile spool harness: %v\n%s", err, out)
	}
	return bin
}

func TestNavfeederRecoveryFilesystemFaults(t *testing.T) {
	if out, err := exec.Command(spoolTestBinary(t), t.TempDir()).CombinedOutput(); err != nil {
		t.Fatalf("spool faults: %v\n%s", err, out)
	}
}

// The harness runs the actual feeder main/producer/TLS/ACK/spool code, injecting
// only unlink(EACCES) after ACK so the already-committed older prefix survives
// deterministically until SIGKILL. The historian and replay ledger are real.
func TestIntegrationNavfeederCrashIdentity(t *testing.T) {
	dsn := os.Getenv("NAVLISTENER_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable TimescaleDB NAVLISTENER_TEST_DSN")
	}
	bin := spoolTestBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	station := fmt.Sprintf("crash-%d", time.Now().UnixNano())
	writer, err := store.New(ctx, config.Store{DSN: dsn, BatchSize: 1, BatchEvery: 10 * time.Millisecond}, log)
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewDurableTracker()
	committed := make(chan struct{}, 100)
	writer.SetDurableNotify(func(source, session string, seq uint64) {
		tracker.Resolved(source, session, seq)
		committed <- struct{}{}
	})
	writerDone := make(chan struct{})
	go func() { writer.Run(ctx); close(writerDone) }()
	defer func() { cancel(); <-writerDone }()
	out := make(chan *RawFrame, 100)
	go func() {
		for {
			select {
			case f := <-out:
				writer.Enqueue(&store.NavFrame{Ts: f.Recv, ReceivedAt: f.RecvLocal, SourceID: f.Source, Session: f.Session, SourceSeq: f.Seq, HasSourceSeq: true, GnssID: int(f.GnssID), SvID: f.SvID, SigID: f.SigID, Raw: f.RawBytes(), MsgType: f.NavType(), DecoderVer: "ra6-crash-test"})
			case <-ctx.Done():
				return
			}
		}
	}()
	spool := filepath.Join(t.TempDir(), "spool")
	sessions := map[string]bool{}
	for run := 0; run < 3; run++ {
		src, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		sourceDone := make(chan struct{})
		go func(marker uint32) {
			c, e := src.Accept()
			if e != nil {
				return
			}
			defer c.Close()
			var capture []byte
			for i := uint32(0); i < 2; i++ {
				capture = append(capture, buildUBX(2, 0x13, buildSFRBXPayload(gnss.GPS, 1, 0, 0, []uint32{marker + i}))...)
			}
			_, _ = c.Write(capture)
			<-sourceDone
		}(uint32(run*2 + 101))
		reserved, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := reserved.Addr().String()
		reserved.Close()
		ferr := &syncBuf{}
		cmd := exec.Command(bin, "feeder", "--server", addr, "--source", src.Addr().String(), "--station", station, "--token", "test", "--feed", "ubx", "--insecure", "--spool", "1", "--spool-file", spool)
		cmd.Env = append(os.Environ(), "NAVFEEDER_TEST_KEEP_ACKED="+spool)
		cmd.Stderr = ferr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		session := waitForSession(t, ferr)
		if sessions[session] {
			t.Fatalf("reused fresh session %s", session)
		}
		sessions[session] = true
		deadline := time.Now().Add(10 * time.Second)
		for {
			b, _ := os.ReadFile(spool)
			if len(b) > 73 && string(b[9:41]) == session {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("new spool missing: %s", ferr.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
		ln, err := tls.Listen("tcp", addr, tc)
		if err != nil {
			t.Fatal(err)
		}
		srv := newPushServer(addr, tc, out, tokenAuth(station, "test", "ubx"), 10*time.Millisecond, 0, log)
		srv.SetDurableTracker(tracker)
		runCtx, runCancel := context.WithCancel(ctx)
		serverDone := make(chan struct{})
		go func() { _ = srv.serve(runCtx, ln); close(serverDone) }()
		for !strings.Contains(ferr.String(), "acked disk spool could not be removed") {
			select {
			case <-committed:
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatalf("no ACK before crash: %s", ferr.String())
			}
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait() // SIGKILL: no shutdown flush
		close(sourceDone)
		src.Close()
		runCancel()
		ln.Close()
		<-serverDone
		var count int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM nav_frames WHERE source_id=$1`, station).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != (run+1)*2 {
			t.Fatalf("restart %d persisted %d rows, want %d", run, count, (run+1)*2)
		}
	}
	var copies, distinctPayloads, keys int
	if err := db.QueryRow(ctx, `SELECT count(*),count(DISTINCT raw) FROM nav_frames WHERE source_id=$1`, station).Scan(&copies, &distinctPayloads); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM nav_frames_seq_seen WHERE source_id=$1`, station).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if copies != 6 || distinctPayloads != 6 || keys != 6 {
		t.Fatalf("copies=%d distinct fresh payloads=%d replay keys=%d; want six each", copies, distinctPayloads, keys)
	}
}
