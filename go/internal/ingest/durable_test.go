package ingest

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/wire"
)

// TestDurableTrackerWatermark pins the regression fix accounting: only RECEIVED
// persistable frames hold the watermark; resolution frees it; reception gaps
// are skipped past; telemetry advances it without holding.
func TestDurableTrackerWatermark(t *testing.T) {
	tr := NewDurableTracker()
	const obs, sess = "obs1", "boot-a"

	if got := tr.Watermark(obs, sess); got != 0 {
		t.Fatalf("empty watermark = %d, want 0", got)
	}

	// Frames 1..3 received persistable, plus a reception gap at 4, then 5.
	for _, seq := range []uint64{1, 2, 3, 5} {
		tr.Received(obs, sess, seq, true)
	}
	if got := tr.Watermark(obs, sess); got != 0 {
		t.Fatalf("all-outstanding watermark = %d, want 0", got)
	}

	// Resolving out of order: 2 first frees nothing below 1.
	tr.Resolved(obs, sess, 2)
	if got := tr.Watermark(obs, sess); got != 0 {
		t.Fatalf("watermark after resolving 2 = %d, want 0 (1 still outstanding)", got)
	}
	tr.Resolved(obs, sess, 1)
	if got := tr.Watermark(obs, sess); got != 2 {
		t.Fatalf("watermark = %d, want 2 (3 outstanding)", got)
	}
	tr.Resolved(obs, sess, 3)
	// The never-received 4 must NOT hold the watermark (regression fix gap rule).
	if got := tr.Watermark(obs, sess); got != 4 {
		t.Fatalf("watermark = %d, want 4 (gap at 4 skipped; 5 outstanding)", got)
	}
	tr.Resolved(obs, sess, 5)
	if got := tr.Watermark(obs, sess); got != 5 {
		t.Fatalf("watermark = %d, want 5", got)
	}

	// Telemetry (non-persistable) advances highest without holding.
	tr.Received(obs, sess, 6, false)
	if got := tr.Watermark(obs, sess); got != 6 {
		t.Fatalf("watermark after telemetry 6 = %d, want 6", got)
	}

	// A reconnect replay of a low sequence regresses transiently, then recovers
	// on resolution (the store dedups it against the committed claim).
	tr.Received(obs, sess, 5, true)
	if got := tr.Watermark(obs, sess); got != 4 {
		t.Fatalf("watermark during replay = %d, want 4", got)
	}
	tr.Resolved(obs, sess, 5)
	if got := tr.Watermark(obs, sess); got != 6 {
		t.Fatalf("watermark after replay resolution = %d, want 6", got)
	}

	// Sessions are independent : a rebooted feeder starts clean.
	tr.Received(obs, "boot-b", 1, true)
	if got := tr.Watermark(obs, sess); got != 6 {
		t.Fatalf("boot-a watermark disturbed by boot-b: %d", got)
	}
	if got := tr.Watermark(obs, "boot-b"); got != 0 {
		t.Fatalf("boot-b watermark = %d, want 0", got)
	}

	// Seq 0 is not a valid GNF1 sequence and must not wedge the accounting.
	tr.Received(obs, sess, 0, true)
	if got := tr.Watermark(obs, sess); got != 6 {
		t.Fatalf("watermark after seq-0 junk = %d, want 6", got)
	}
	// Resolving something never received is a harmless no-op.
	tr.Resolved(obs, "boot-c", 99)
}

// TestPushStreamAcksDurableWatermark drives stream() with a DurableTracker
// installed: no ack may reach the feeder before the store resolves the frames
// (the regression fix boundary), and resolution then releases exactly the watermark.
// Without the tracker (live-only mode) the same frames ack on receipt — the
// contrast leg keeps the mode split itself pinned.
func TestPushStreamAcksDurableWatermark(t *testing.T) {
	readAck := func(t *testing.T, c net.Conn, wait time.Duration) (uint64, bool) {
		t.Helper()
		_ = c.SetReadDeadline(time.Now().Add(wait))
		ft, payload, err := wire.ReadFrame(c)
		if err != nil {
			return 0, false
		}
		if ft != wire.Ack {
			t.Fatalf("unexpected frame type %d", ft)
		}
		seq, err := wire.DecodeAck(payload)
		if err != nil {
			t.Fatal(err)
		}
		return seq, true
	}
	mkRec := func() wire.RawRecord {
		return wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
	}

	t.Run("durable mode", func(t *testing.T) {
		srvConn, cliConn := net.Pipe()
		defer cliConn.Close()
		defer srvConn.Close()
		tr := NewDurableTracker()
		out := make(chan *RawFrame, 8)
		p := &PushServer{out: out, ackInterval: 10 * time.Millisecond,
			log: slog.New(slog.NewTextHandler(io.Discard, nil)), durable: tr}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go p.stream(ctx, srvConn, &connWriter{c: srvConn}, "obs1", "ubx", "boot-a")

		for seq := uint64(1); seq <= 2; seq++ {
			if err := wire.WriteFrame(cliConn, wire.Data, wire.EncodeData(seq, mkRec())); err != nil {
				t.Fatal(err)
			}
			<-out
		}
		// Undelivered by the store: several ack intervals must pass in silence.
		if seq, ok := readAck(t, cliConn, 100*time.Millisecond); ok {
			t.Fatalf("ack %d sent before the store resolved anything ", seq)
		}
		// The store commits frame 1 only: ack 1, not 2.
		tr.Resolved("obs1", "boot-a", 1)
		if seq, ok := readAck(t, cliConn, time.Second); !ok || seq != 1 {
			t.Fatalf("ack = %d (ok=%v), want 1", seq, ok)
		}
		tr.Resolved("obs1", "boot-a", 2)
		if seq, ok := readAck(t, cliConn, time.Second); !ok || seq != 2 {
			t.Fatalf("ack = %d (ok=%v), want 2", seq, ok)
		}
	})

	t.Run("live-only mode", func(t *testing.T) {
		srvConn, cliConn := net.Pipe()
		defer cliConn.Close()
		defer srvConn.Close()
		out := make(chan *RawFrame, 8)
		p := &PushServer{out: out, ackInterval: 10 * time.Millisecond,
			log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go p.stream(ctx, srvConn, &connWriter{c: srvConn}, "obs1", "ubx", "boot-a")

		if err := wire.WriteFrame(cliConn, wire.Data, wire.EncodeData(7, mkRec())); err != nil {
			t.Fatal(err)
		}
		<-out
		if seq, ok := readAck(t, cliConn, time.Second); !ok || seq != 7 {
			t.Fatalf("live-only ack = %d (ok=%v), want 7 on receipt", seq, ok)
		}
	})
}
