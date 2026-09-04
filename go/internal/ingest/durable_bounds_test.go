package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/wire"
)

func TestDurableOutageAndChurnBounds(t *testing.T) {
	tr := NewDurableTracker()
	tr.maxSessions = 16
	tr.maxPerSession = 64
	tr.maxOutstanding = 512
	// Four hours at 100 attempted frames/sec, no persistence at all. Include
	// sparse/nonmonotone sequences and adversarial per-frame session churn.
	for tick := 0; tick < 4*3600*100; tick++ {
		sess := fmt.Sprint(tick % 8)
		tr.Received("offline", sess, uint64(tick/8)*3+1, true)
		if tick%100 == 0 {
			tr.Received("churn", fmt.Sprint(tick), uint64(tick)+1, true)
		}
		if tick%1000 == 0 {
			if tr.outstanding > 512 || len(tr.m) > 16 {
				t.Fatal("budget exceeded")
			}
			for _, s := range tr.m {
				if len(s.pending) > 64 || len(s.outstanding) != len(s.pending) {
					t.Fatal("per-session budget exceeded")
				}
			}
		}
	}
	// A week of silence cannot discard the only record of an unresolved hole.
	tr.mu.Lock()
	for _, s := range tr.m {
		s.touched = time.Now().Add(-30 * 24 * time.Hour)
	}
	tr.pruneLocked(time.Now())
	tr.mu.Unlock()
	if tr.outstanding != 512 {
		t.Fatalf("forgot holes: %d", tr.outstanding)
	}
	// Unaffected ACK queries stay prompt while other producers pound full budgets.
	tr.Received("healthy", "session", 100, false)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				tr.Received("offline", "0", 999999, true)
			}
		}
	}()
	start := time.Now()
	for i := 0; i < 10000; i++ {
		if got := tr.Watermark("healthy", "session"); got != 100 {
			t.Fatalf("healthy watermark %d", got)
		}
	}
	close(stop)
	wg.Wait()
	if time.Since(start) > 2*time.Second {
		t.Fatal("unrelated ACK lookup stalled")
	}
	// Already-tracked replay remains admissible at capacity. Commit in arbitrary
	// order and verify the watermark against the actual remaining minimum.
	for key, s := range tr.m {
		original := append([]uint64(nil), s.pending...)
		rand.New(rand.NewSource(6)).Shuffle(len(original), func(i, j int) { original[i], original[j] = original[j], original[i] })
		for _, seq := range original {
			if !tr.Received(key.source, key.session, seq, true) {
				t.Fatal("full budget rejected tracked replay")
			}
			tr.Resolved(key.source, key.session, seq)
			want := s.highest
			for n := range s.outstanding {
				if n-1 < want {
					want = n - 1
				}
			}
			if got := tr.Watermark(key.source, key.session); got != want {
				t.Fatalf("watermark=%d, want %d", got, want)
			}
		}
	}
	if tr.outstanding != 0 {
		t.Fatal("commits did not release budget")
	}
	if !tr.Received("offline", "0", 999999, true) {
		t.Fatal("recovered persistence cannot admit new data")
	}
}

func TestDurableSessionChurnAndSparseReplay(t *testing.T) {
	tr := NewDurableTracker()
	tr.maxSessions = 4
	for i := 0; i < 4; i++ {
		if !tr.Received("obs", fmt.Sprint(i), uint64(i+1)*100, true) {
			t.Fatal("initial admission")
		}
	}
	for i := 4; i < 10000; i++ {
		if tr.Received("obs", fmt.Sprint(i), 1, true) {
			t.Fatal("unbounded session admission")
		}
	}
	if !tr.Received("obs", "0", 3, true) || tr.Watermark("obs", "0") != 2 {
		t.Fatal("sparse low replay lost")
	}
	tr.Resolved("obs", "0", 3)
	if tr.Watermark("obs", "0") != 99 {
		t.Fatal("never-received gaps should be skipped")
	}
	tr.Resolved("obs", "0", 100)
	tr.mu.Lock()
	tr.m[durableKey{"obs", "0"}].touched = time.Now().Add(-8 * 24 * time.Hour)
	tr.nextPrune = time.Time{}
	tr.mu.Unlock()
	if !tr.Received("obs", "new", 1, true) || len(tr.m) != 4 {
		t.Fatal("resolved idle session not reclaimed")
	}
}

func TestPushTrackingBudgetStopsBeforeHandoff(t *testing.T) {
	tr := NewDurableTracker()
	tr.maxPerSession = 1
	server, client := net.Pipe()
	defer client.Close()
	out := make(chan *RawFrame, 4)
	p := &PushServer{out: out, ackInterval: time.Millisecond, durable: tr, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		p.stream(context.Background(), server, &connWriter{c: server}, identity.NewPrivateContext("obs", identity.CredentialToken), "ubx", "session")
	}()
	rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.GPS, SvID: 1, Raw: make([]byte, 40)}
	for _, seq := range []uint64{10, 20} {
		if err := wire.WriteFrame(client, wire.Data, wire.EncodeData(seq, rec)); err != nil {
			t.Fatal(err)
		}
	}
	// Sparse seq 10 may safely produce ACK 9, but never ACK 10 or later.
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	for {
		ft, b, err := wire.ReadFrame(client)
		if err != nil {
			break
		}
		if ft == wire.Ack {
			seq, _ := wire.DecodeAck(b)
			if seq >= 10 {
				t.Fatalf("ACK beyond unresolved hole: %d", seq)
			}
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("budget did not close stream")
	}
	if len(out) != 1 || (<-out).Seq != 10 {
		t.Fatal("over-budget frame reached decoder")
	}
	if tr.Watermark("obs", "session") != 9 {
		t.Fatal("old unresolved receipt was forgotten")
	}
}
