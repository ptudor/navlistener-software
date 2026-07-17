package serve

import (
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/state"
)

// regression fix guard tests: the serve tier must self-report. The feed cache records a
// last-successful-refresh timestamp (a frozen feed becomes alertable as
// time() − timestamp), and the SSE broker exposes client count, slow-client
// drops (kick loop), and cap rejections. Package tests run sequentially, so
// asserting the shared collectors is race-free here.

func TestFeedRefreshSetsTimestampGauge(t *testing.T) {
	s := New("127.0.0.1:0", state.New(1), nil, nil, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	before := testutil.ToFloat64(metrics.ServeFeedRefreshTimestamp.WithLabelValues("global"))
	s.refresh("global")
	after := testutil.ToFloat64(metrics.ServeFeedRefreshTimestamp.WithLabelValues("global"))
	if after <= 0 || after < before {
		t.Errorf("refresh timestamp = %v (was %v), want a fresh positive Unix time after a successful refresh", after, before)
	}
}

func TestSSEBrokerMetricsLifecycle(t *testing.T) {
	oldMax := sseMaxClients
	sseMaxClients = 2
	defer func() { sseMaxClients = oldMax }()

	b := newBroker()
	c1, ok := b.subscribe()
	if !ok {
		t.Fatal("first subscribe rejected")
	}
	if got := testutil.ToFloat64(metrics.SSEClients); got != 1 {
		t.Errorf("SSEClients = %v after one subscribe, want 1", got)
	}
	c2, ok := b.subscribe()
	if !ok {
		t.Fatal("second subscribe rejected")
	}
	if got := testutil.ToFloat64(metrics.SSEClients); got != 2 {
		t.Errorf("SSEClients = %v after two subscribes, want 2", got)
	}

	rejBefore := testutil.ToFloat64(metrics.SSESubscribeRejectedTotal)
	if _, ok := b.subscribe(); ok {
		t.Fatal("subscribe past the cap succeeded")
	}
	if d := testutil.ToFloat64(metrics.SSESubscribeRejectedTotal) - rejBefore; d != 1 {
		t.Errorf("SSESubscribeRejectedTotal delta = %v, want 1", d)
	}

	// Neither client is being read: publishing one event past the buffer size
	// overflows both, dropping one event toward each and kicking them.
	dropBefore := testutil.ToFloat64(metrics.SSEEventsDroppedTotal)
	for i := 0; i <= sseClientBuffer; i++ {
		b.Publish(EventMsg{ID: int64(i + 1)})
	}
	if d := testutil.ToFloat64(metrics.SSEEventsDroppedTotal) - dropBefore; d != 2 {
		t.Errorf("SSEEventsDroppedTotal delta = %v, want 2 (one overflow per slow client)", d)
	}

	b.unsubscribe(c1)
	b.unsubscribe(c2)
	if got := testutil.ToFloat64(metrics.SSEClients); got != 0 {
		t.Errorf("SSEClients = %v after both unsubscribed, want 0", got)
	}
}
