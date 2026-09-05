package serve

import (
	"context"
	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/store"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeConditions struct {
	fakeEvents
	snapshot store.ConditionSnapshot
}

func (f *fakeConditions) CurrentConditions(ctx context.Context, a string, since time.Time) (store.ConditionSnapshot, error) {
	f.lastAudience = a
	f.lastSince = since
	f.lastCtx = ctx
	return f.snapshot, f.err
}
func TestCurrentConditionsAudienceEpochAndCompleteness(t *testing.T) {
	at := time.Now().Add(-48 * time.Hour)
	epochs := audience.NewPolicyEpochs(at)
	backend := &fakeConditions{snapshot: store.ConditionSnapshot{Cursor: 250, Events: []store.StoredEvent{{ID: 1, Time: at.Add(time.Hour), SV: "roof", Type: "jamming_detected", NewValue: "jammed"}}}}
	s := newTestServer(nil, backend)
	s.SetPolicyEpochs(epochs)
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/events/conditions", nil))
	data := decodeEnvelope(t, rr)
	if data["complete"] != true || data["cursor"] != float64(250) || data["audience"] != "operator:local" || backend.lastAudience != "operator:local" || !backend.lastSince.Equal(at) {
		t.Fatalf("snapshot=%v scope=%s since=%v", data, backend.lastAudience, backend.lastSince)
	}
	if _, ok := backend.lastCtx.Deadline(); !ok {
		t.Fatal("unbounded query")
	}
	epochs.Advance([]identity.Audience{{Kind: identity.AudienceOperator, ID: identity.LocalCollectorInstance}}, at.Add(2*time.Hour))
	rr = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/events/conditions", nil))
	if rr.Code != 200 || !backend.lastSince.Equal(at.Add(2*time.Hour)) {
		t.Fatal("epoch not enforced")
	}
	rr = httptest.NewRecorder()
	testServer(nil).http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/events/conditions", nil))
	if rr.Code != 503 {
		t.Fatal("missing backend claimed complete state")
	}
}

func TestReplaySnapshotDetectsGapAndReset(t *testing.T) {
	b := newBroker()
	for id := int64(1); id <= sseRecentCap+20; id++ {
		b.Publish(EventMsg{ID: id})
	}
	if _, gap := b.replaySnapshot(1, true); !gap {
		t.Fatal("evicted cursor not signaled")
	}
	if _, gap := b.replaySnapshot(30, true); gap {
		t.Fatal("retained cursor signaled gap")
	}
	b.Reset()
	if _, gap := b.replaySnapshot(30, true); !gap {
		t.Fatal("reset not signaled")
	}
}
