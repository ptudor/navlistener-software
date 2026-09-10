package ingest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/wire"
)

// navRecord builds a valid non-telemetry word-feed record with an n-byte body.
func navRecord(n int) wire.RawRecord {
	raw := make([]byte, n)
	for i := range raw {
		raw[i] = byte(0xA0 + i)
	}
	return wire.RawRecord{
		RecvUnixNs: time.Now().UnixNano(),
		GnssID:     gnss.GPS,
		SvID:       5,
		FrameType:  0x10, // GpsLnav — a raw-nav type, deliberately not telemetry
		Raw:        raw,
	}
}

func streamOnPipe(t *testing.T, feed string, tr *DurableTracker) (client net.Conn, out chan *RawFrame, done chan struct{}) {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	t.Cleanup(func() { cliConn.Close(); srvConn.Close() })
	out = make(chan *RawFrame, 16)
	p := &PushServer{out: out, ackInterval: 10 * time.Millisecond,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), durable: tr}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done = make(chan struct{})
	go func() {
		defer close(done)
		p.stream(ctx, srvConn, &connWriter{c: srvConn},
			identity.NewPrivateContext("obs1", identity.CredentialToken), feed, "boot-a")
	}()
	return cliConn, out, done
}

// expectNoFrame drains nothing: it asserts p.out stays empty for a short window.
func expectNoFrame(t *testing.T, out chan *RawFrame, why string) {
	t.Helper()
	select {
	case f := <-out:
		t.Fatalf("%s: frame reached the decode stage: %+v", why, f)
	case <-time.After(150 * time.Millisecond):
	}
}

// sequence 0 is outside the GNF1 sequence space (both reference
// feeders emit ++seq). It could never advance the durable ACK watermark, so the
// collector used to apply and persist it on every reconnect while the feeder's
// spool could never retire it. It is now a protocol error that closes the
// connection, registering nothing and acking nothing.
func TestPushRejectsSequenceZero(t *testing.T) {
	t.Run("registers no receipt and emits no ack", func(t *testing.T) {
		tr := NewDurableTracker()
		cli, out, done := streamOnPipe(t, "ubx", tr)

		if err := wire.WriteFrame(cli, wire.Data, wire.EncodeData(0, navRecord(40))); err != nil {
			t.Fatal(err)
		}
		expectNoFrame(t, out, "sequence zero")

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("stream did not return after a sequence-zero DATA frame")
		}
		// No ACK may have been written before the stream tore down.
		_ = cli.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if ft, _, err := wire.ReadFrame(cli); err == nil {
			t.Fatalf("frame type %d written to the feeder; want nothing before close", ft)
		}
		tr.mu.Lock()
		sessions := len(tr.m)
		tr.mu.Unlock()
		if sessions != 0 {
			t.Fatalf("durable tracker created %d session(s) for sequence zero, want 0", sessions)
		}
		if got := tr.Watermark("obs1", "boot-a"); got != 0 {
			t.Fatalf("watermark = %d after sequence zero, want 0", got)
		}
	})

	t.Run("closes the connection and counts a protocol error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		addr, out := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"))
		counter := metrics.PushErrorsTotal.WithLabelValues("observer16", "seq_zero")
		before := testutil.ToFloat64(counter)

		conn := dialPush(t, addr)
		defer conn.Close()
		if err := wire.WriteHello(conn, wire.HelloMsg{
			Token: "s3cret", Station: "observer16", Feed: "ubx", Session: "boot-test"}); err != nil {
			t.Fatal(err)
		}
		if ft, payload, err := wire.ReadFrame(conn); err != nil || ft != wire.Welcome {
			t.Fatalf("welcome read: ft=%d err=%v", ft, err)
		} else if w, _ := parseWelcome(payload); !w.OK {
			t.Fatalf("welcome = %+v, want ok", w)
		}
		if err := wire.WriteFrame(conn, wire.Data, wire.EncodeData(0, navRecord(40))); err != nil {
			t.Fatal(err)
		}
		expectNoFrame(t, out, "sequence zero over TLS")

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, _, err := wire.ReadFrame(conn); err == nil {
			t.Fatal("connection stayed open after a sequence-zero DATA frame")
		}
		if got := testutil.ToFloat64(counter) - before; got != 1 {
			t.Fatalf("push_errors_total{seq_zero} rose by %v, want 1", got)
		}
	})
}

// a word-feed record must carry whole 32-bit words. Unaligned or
// empty bodies used to be silently truncated by bytesToWords and then stored as
// "raw evidence" that differed from the authenticated feeder's bytes, with the
// sequence acked so the lost suffix was unrecoverable from the edge spool. They
// are now rejected before conversion, under their own metric label, and acked
// past (never closed on) so one bad record cannot wedge the valid spool records
// queued behind it.
func TestPushRejectsUnalignedWordRecords(t *testing.T) {
	tr := NewDurableTracker()
	cli, out, _ := streamOnPipe(t, "ubx", tr)
	counter := metrics.PushErrorsTotal.WithLabelValues("obs1", "word_alignment")
	before := testutil.ToFloat64(counter)

	// Sequence n+1 carries an n-byte body, for n in 0..9.
	for n := 0; n <= 9; n++ {
		if err := wire.WriteFrame(cli, wire.Data, wire.EncodeData(uint64(n+1), navRecord(n))); err != nil {
			t.Fatalf("write body length %d: %v", n, err)
		}
	}

	// Only the aligned, non-empty lengths may be forwarded, in order, byte-exact.
	for _, n := range []int{4, 8} {
		var f *RawFrame
		select {
		case f = <-out:
		case <-time.After(2 * time.Second):
			t.Fatalf("body length %d never reached the decode stage", n)
		}
		if f.Seq != uint64(n+1) {
			t.Fatalf("forwarded seq %d, want %d (an unaligned body was forwarded)", f.Seq, n+1)
		}
		if len(f.Words) != n/4 {
			t.Fatalf("body length %d decoded to %d words, want %d", n, len(f.Words), n/4)
		}
		if got, want := f.RawBytes(), navRecord(n).Raw; !bytes.Equal(got, want) {
			t.Fatalf("body length %d did not round-trip: got %x, want %x", n, got, want)
		}
	}
	expectNoFrame(t, out, "unaligned bodies")

	if got := testutil.ToFloat64(counter) - before; got != 8 {
		t.Fatalf("push_errors_total{word_alignment} rose by %v, want 8 (lengths 0,1,2,3,5,6,7,9)", got)
	}

	// The ACK policy must be deterministic and non-wedging: every rejected
	// sequence was registered as never-persistable, so the watermark sits just
	// below the oldest genuinely outstanding record (sequence 5, the 4-byte body)
	// rather than stalling at the first malformed one, and resolving the two real
	// records releases all ten.
	if got := tr.Watermark("obs1", "boot-a"); got != 4 {
		t.Fatalf("watermark = %d while sequence 5 is outstanding, want 4", got)
	}
	tr.Resolved("obs1", "boot-a", 5)
	tr.Resolved("obs1", "boot-a", 9)
	if got := tr.Watermark("obs1", "boot-a"); got != 10 {
		t.Fatalf("watermark = %d after resolving both real records, want 10", got)
	}
}

// The alignment invariant applies to every word-oriented feed and to no other
// record class: telemetry bodies have their own exact-length codecs, and rtcm
// carries byte-oriented messages.
func TestWordRecordWellFormedScope(t *testing.T) {
	for _, feed := range []string{"ubx", "sbf"} {
		for n := 0; n <= 9; n++ {
			want := n > 0 && n%4 == 0
			if got := wordRecordWellFormed(navRecord(n), feed); got != want {
				t.Errorf("wordRecordWellFormed(%s, %d bytes) = %v, want %v", feed, n, got, want)
			}
		}
	}
	for n := 0; n <= 9; n++ {
		if !wordRecordWellFormed(navRecord(n), "rtcm") {
			t.Errorf("rtcm body of %d bytes rejected; rtcm is byte-oriented", n)
		}
		telem := navRecord(n)
		telem.FrameType = TelemJammingStats
		if !wordRecordWellFormed(telem, "ubx") {
			t.Errorf("telemetry body of %d bytes rejected by the word-alignment gate", n)
		}
	}
}
