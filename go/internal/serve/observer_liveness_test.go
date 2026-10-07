package serve

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
)

// TestObserversCarryReceiverLiveness: every observer row the collector has
// heard from carries last_seen_s and last_seen from the station_offline
// detector's own liveness source, so a client can classify a station offline
// in steady state. The RF model is withheld once stale, so a station that went
// dark stays listed with a climbing age and no rf; a station that delivers
// only board telemetry has liveness in private views; a configured station
// never heard from omits both fields; and public views know only what
// navigation and RF inputs establish.
func TestObserversCarryReceiverLiveness(t *testing.T) {
	sources := []config.Source{
		{Name: "dial-dark", Type: "ubx", Addr: "10.0.0.2:2947"},
		{Name: "dial-silent", Type: "ubx", Addr: "10.0.0.3:2947"},
	}
	s := testServer(sources)
	now := time.Now()
	// dial-dark reported RF ten minutes ago: past the RF model's staleness,
	// still inside the liveness read model.
	s.store.Apply(&ingest.RawFrame{Source: "dial-dark", Recv: now.Add(-10 * time.Minute), RF: &ingest.RawRF{
		Bands: []ingest.RFBand{{Block: 0, AGC: 3000}},
	}})
	// board-only is a push station whose only input is board telemetry.
	s.store.Apply(&ingest.RawFrame{Source: "board-only", Session: "boot-a", Seq: 1, HasSeq: true,
		Recv: now.Add(-2 * time.Minute), RecvLocal: now.Add(-2 * time.Minute),
		Details: &ingest.ObserverDetails{UptimeMS: 100, Reason: 1}})

	index := func(rows []observer) map[string]observer {
		out := map[string]observer{}
		for _, o := range rows {
			out[o.ID] = o
		}
		return out
	}
	age := func(t *testing.T, o observer, lo, hi int, at time.Time) {
		t.Helper()
		if o.LastSeenS == nil || *o.LastSeenS < lo || *o.LastSeenS > hi {
			t.Fatalf("%s last_seen_s = %v, want %d..%d", o.ID, o.LastSeenS, lo, hi)
		}
		if o.LastSeen == nil || time.Unix(*o.LastSeen, 0).Sub(at).Abs() > 2*time.Second {
			t.Errorf("%s last_seen = %v, want the epoch of %v", o.ID, o.LastSeen, at)
		}
	}

	private := index(s.observers(now, s.store, sources, identity.Audience{Kind: identity.AudienceOperator}))
	if len(private) != 3 {
		t.Fatalf("private observers = %+v, want dial-dark, dial-silent and board-only", private)
	}
	dark := private["dial-dark"]
	if dark.RF != nil {
		t.Error("dial-dark's stale RF model was served")
	}
	age(t, dark, 600, 605, now.Add(-10*time.Minute))
	age(t, private["board-only"], 120, 125, now.Add(-2*time.Minute))
	if silent := private["dial-silent"]; silent.LastSeenS != nil || silent.LastSeen != nil {
		t.Errorf("dial-silent has liveness %v/%v although the collector never heard from it", silent.LastSeenS, silent.LastSeen)
	}

	public := index(s.observers(now, s.store, sources, identity.Audience{Kind: identity.AudiencePublic}))
	if _, ok := public["board-only"]; ok {
		t.Error("public observers enumerate a board-only station")
	}
	if len(public) != 2 {
		t.Fatalf("public observers = %+v, want the two dial sources", public)
	}
	age(t, public["dial-dark"], 600, 605, now.Add(-10*time.Minute))
	if silent := public["dial-silent"]; silent.LastSeenS != nil {
		t.Errorf("public dial-silent has liveness %v", silent.LastSeenS)
	}
}
