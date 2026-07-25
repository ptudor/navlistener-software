package ingest

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNavfeederDeliveredCounter pins the C feeder's two producer counters must stay
// distinct. `recognized` counts checksum-valid UBX of a class/id the feeder handles — the
// regression fix reconnect-backoff signal — while `delivered` counts records that actually reached the
// spool. A source streaming checksum-valid SFRBX whose INNER payload is malformed advances the
// first and not the second, which is precisely the "link alive, nothing spooling" condition an
// operator previously had no way to see.
//
// The stream below is 3 well-formed SFRBX followed by 5 checksum-valid ones with an invalid
// inner length (numWords claiming more words than the payload holds — the review's exact
// verification case), so the feeder must report recognized=8 delivered=3 and the collector
// must receive exactly the 3 valid records. Skips when the binary isn't built.
func TestNavfeederDeliveredCounter(t *testing.T) {
	// 3 well-formed SFRBX + 5 checksum-valid ones with an invalid inner length.
	stderr, records := runFeederStatsCase(t, syntheticSFRBXCapture(3, 5), 3)

	recognized, delivered := parseUbxSessionStats(t, stderr)
	if recognized != 8 {
		t.Errorf("recognized=%d, want 8 (all 8 messages are checksum-valid and of a handled class)",
			recognized)
	}
	if delivered != 3 {
		t.Errorf("delivered=%d, want 3 (only the well-formed SFRBX reach the spool)", delivered)
	}
	// The distinguishing warning must be reserved for recognized>0 && delivered==0.
	if strings.Contains(stderr, "NOTHING is being spooled") {
		t.Errorf("emitted the nothing-spooled warning while %d records were delivered", delivered)
	}
	// Backoff policy kept as-is : recognized traffic makes the connection "useful", so
	// the feeder must NOT log the instant-close backoff message even though 5 of the 8
	// messages produced no record.
	if strings.Contains(stderr, "closed instantly with no data") {
		t.Errorf("source with recognized traffic was treated as useless; backoff policy changed")
	}
	t.Logf("recognized=%d delivered=%d; %d records at the collector", recognized, delivered, records)
}

// TestNavfeederNothingSpooledWarning is the condition regression fix exists to make visible: a source
// that is alive and talking recognizable UBX while every inner payload is rejected, so nothing
// reaches the spool. Two numbers alone leave that to be inferred, so the feeder says it in
// words — and the backoff policy still treats the source as useful, because reconnecting
// cannot fix a malformed payload.
func TestNavfeederNothingSpooledWarning(t *testing.T) {
	stderr, _ := runFeederStatsCase(t, syntheticSFRBXCapture(0, 6), 0)

	recognized, delivered := parseUbxSessionStats(t, stderr)
	if recognized != 6 || delivered != 0 {
		t.Errorf("recognized=%d delivered=%d, want 6/0", recognized, delivered)
	}
	if !strings.Contains(stderr, "NOTHING is being spooled") {
		t.Errorf("no nothing-spooled warning for a source delivering 6 undecodable messages:\n%s", stderr)
	}
	if strings.Contains(stderr, "closed instantly with no data") {
		t.Errorf("live-but-degraded source was treated as useless; the regression fix policy changed")
	}
}

// runFeederStatsCase runs the real navfeeder against a real PushServer over capBytes, waits
// for wantRecords records to arrive (and for the session-stats line), and returns the feeder's
// stderr plus the record count. The fake receiver streams the capture exactly ONCE and closes,
// which ends run_ubx and makes it log the session summary immediately (the periodic line is on
// a 5-minute cadence by design). Later reconnects — the feeder resets backoff and re-dials,
// because recognized traffic made the connection "useful" — are accepted but stay silent, so
// the counts under test come from a single pass and no further records can arrive.
func runFeederStatsCase(t *testing.T, capBytes []byte, wantRecords int) (string, int) {
	t.Helper()
	bin := feederBinary(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan *RawFrame, 32)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("del-e2e", "s3cret", "ubx"),
		25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pushLn, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	defer pushLn.Close()
	go func() { _ = srv.serve(ctx, pushLn) }()

	srcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srcLn.Close()
	var streamed atomic.Bool
	go func() {
		for {
			c, err := srcLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if streamed.CompareAndSwap(false, true) {
					_, _ = c.Write(capBytes)
					return // close: run_ubx returns and logs the session summary
				}
				<-ctx.Done()
			}(c)
		}
	}()

	var ferr syncBuffer
	cmd := exec.CommandContext(ctx, bin,
		"--server", pushLn.Addr().String(),
		"--source", srcLn.Addr().String(),
		"--station", "del-e2e", "--token", "s3cret", "--feed", "ubx",
		"--insecure", "--spool", "1000")
	cmd.Stderr = &ferr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if t.Failed() {
			t.Logf("navfeeder stderr:\n%s", ferr.String())
		}
	})

	records := 0
	deadline := time.After(20 * time.Second)
	for records < wantRecords {
		select {
		case <-out:
			records++
		case <-deadline:
			t.Fatalf("only %d/%d valid records reached the collector", records, wantRecords)
		}
	}
	// Nothing beyond the expected records may arrive: the malformed messages must be dropped
	// at the edge, not forwarded with a truncated payload.
	select {
	case f := <-out:
		t.Errorf("an undecodable SFRBX was forwarded: %+v", f)
	case <-time.After(500 * time.Millisecond):
	}

	// Poll for the summary rather than sleeping once: the feeder logs it as soon as the source
	// closes, which races the records arriving above.
	for wait := time.Now().Add(10 * time.Second); time.Now().Before(wait); {
		if strings.Contains(ferr.String(), "ubx session: recognized=") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ferr.String(), records
}

var ubxSessionStatsRe = regexp.MustCompile(`ubx session: recognized=(\d+) delivered=(\d+)`)

func parseUbxSessionStats(t *testing.T, stderr string) (recognized, delivered int) {
	t.Helper()
	m := ubxSessionStatsRe.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("no 'ubx session: recognized=… delivered=…' line in feeder stderr:\n%s", stderr)
	}
	recognized, _ = strconv.Atoi(m[1])
	delivered, _ = strconv.Atoi(m[2])
	return recognized, delivered
}

// syncBuffer is a mutex-guarded bytes.Buffer. The other feeder e2e tests hand cmd.Stderr a
// plain bytes.Buffer and only read it after the process is gone; this test reads the log
// WHILE the feeder is still running (polling for the stats line), which would otherwise be a
// data race between os/exec's copier goroutine and the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// syntheticSFRBXCapture builds `good` well-formed UBX-RXM-SFRBX messages followed by `bad`
// whose numWords claims more 32-bit words than the payload actually carries. Every message has
// a valid Fletcher checksum and the RXM/SFRBX class/id, so the feeder recognizes and dispatches
// all of them; emit_sfrbx's inner bounds check (numWords == 0 || 8 + numWords*4 > len) declines
// the bad ones. That is the exact shape of the review's verification case.
func syntheticSFRBXCapture(good, bad int) []byte {
	sfrbx := func(numWordsField, actualWords int) []byte {
		p := make([]byte, 8+actualWords*4)
		p[0] = 0                   // gnssId GPS
		p[1] = 7                   // svId
		p[2] = 0                   // sigId L1 C/A
		p[3] = 7                   // freqId
		p[4] = byte(numWordsField) // numWords as CLAIMED
		p[6] = 2                   // version (F9/M9)
		for i := 0; i < actualWords; i++ {
			p[8+i*4] = 0xA5
			p[8+i*4+3] = 0x8B
		}
		return ubxMsg(ubxClassRXM, ubxIDSFRBX, p)
	}
	var s []byte
	for i := 0; i < good; i++ {
		s = append(s, sfrbx(10, 10)...) // well-formed: a full GPS LNAV subframe
	}
	for i := 0; i < bad; i++ {
		s = append(s, sfrbx(10, 2)...) // claims 10 words, carries 2 — inner length invalid
	}
	return s
}
