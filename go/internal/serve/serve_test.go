package serve

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
)

func testServer(sources []config.Source) *Server {
	return newTestServer(sources, nil)
}

func newTestServer(sources []config.Source, events EventStore) *Server {
	return New("127.0.0.1:0", state.New(4), events, sources, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestServerIdleTimeoutSet guards an idle keep-alive connection between requests
// must be bounded, distinct from the SSE per-write deadline  and unset WriteTimeout
// (SSE streams are exempt from that by design).
func TestServerIdleTimeoutSet(t *testing.T) {
	s := testServer(nil)
	if s.http.IdleTimeout <= 0 {
		t.Error("http.Server.IdleTimeout is unset, want a bounded value")
	}
	if s.http.WriteTimeout != 0 {
		t.Errorf("http.Server.WriteTimeout = %v, want unset (SSE streams need long-lived writes)", s.http.WriteTimeout)
	}
}

// TestEnvelopeAndSchema verifies every v2 feed returns the standard envelope with
// ok=true, an RFC3339 time, and the schema version in data (docs/OUTPUT.md §0).
func TestEnvelopeAndSchema(t *testing.T) {
	s := testServer(nil)
	s.refreshAll()
	for _, feed := range []string{"svs", "global", "observers", "almanac", "sbas"} {
		rr := httptest.NewRecorder()
		s.serveFeed(feed)(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/"+feed, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", feed, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: content-type %q", feed, ct)
		}
		var env struct {
			OK   bool                       `json:"ok"`
			Time string                     `json:"time"`
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: unmarshal: %v", feed, err)
		}
		if !env.OK {
			t.Errorf("%s: ok=false", feed)
		}
		if _, err := time.Parse(time.RFC3339, env.Time); err != nil {
			t.Errorf("%s: time %q not RFC3339: %v", feed, env.Time, err)
		}
		var schema string
		if err := json.Unmarshal(env.Data["schema"], &schema); err != nil || schema != schemaVersion {
			t.Errorf("%s: schema %q, want %q", feed, schema, schemaVersion)
		}
	}
}

// TestSnapshotFeeds verifies the historian backfill accessor returns a copy of each warmed
// feed's current body, omits un-warmed feeds, and does not alias the live cache.
func TestSnapshotFeeds(t *testing.T) {
	s := testServer(nil)
	if len(s.SnapshotFeeds()) != 0 {
		t.Fatalf("cold server should snapshot no feeds, got %d", len(s.SnapshotFeeds()))
	}
	s.refreshAll()
	snap := s.SnapshotFeeds()
	for _, feed := range []string{"svs", "global", "observers", "almanac", "sbas"} {
		body, ok := snap[feed]
		if !ok || len(body) == 0 {
			t.Fatalf("feed %q missing from snapshot", feed)
		}
		var env struct {
			OK   bool `json:"ok"`
			Data struct {
				Schema string `json:"schema"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil || !env.OK || env.Data.Schema != schemaVersion {
			t.Errorf("feed %q snapshot not a valid envelope: %v", feed, err)
		}
	}
	// The returned slice is a copy: mutating it must not corrupt the served cache.
	snap["svs"][0] = 'X'
	s.mu.RLock()
	cached := s.cache["svs"][0]
	s.mu.RUnlock()
	if cached == 'X' {
		t.Error("SnapshotFeeds aliased the live cache")
	}
}

// TestGlobalCounters verifies the global feed carries the flat leap-second and
// live-total scalars (docs/OUTPUT.md §1.2).
func TestGlobalCounters(t *testing.T) {
	s := testServer(nil)
	s.refreshAll()
	rr := httptest.NewRecorder()
	s.serveFeed("global")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/global", nil))
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.Data["leap_seconds"]; !ok {
		t.Error("global feed missing leap_seconds")
	}
	if _, ok := env.Data["total_live_svs"]; !ok {
		t.Error("global feed missing total_live_svs")
	}
}

// TestObservers verifies configured ingest sources are published as observers with
// their strings sanitized (docs/OUTPUT.md §1.3, §INTEGRITY.md §9).
func TestObservers(t *testing.T) {
	s := testServer([]config.Source{
		{Name: "observer16", Type: "ubx", Addr: "10.0.0.2:2947"},
		{Name: "bad\x00name", Type: "sbf", Addr: "x"},
	})
	s.refreshAll()
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Observers) != 2 {
		t.Fatalf("got %d observers, want 2", len(env.Data.Observers))
	}
	if env.Data.Observers[0].ID != "observer16" {
		t.Errorf("observer id %q", env.Data.Observers[0].ID)
	}
	if env.Data.Observers[1].ID != "badname" { // NUL stripped by sanitize
		t.Errorf("unsanitized observer id %q", env.Data.Observers[1].ID)
	}
}

// TestObserversRemarkNeverLeaksAddr guards the internal dial address
// (LAN topology + the exact port of an unauthenticated raw receiver TCP
// stream) must never reach the public observers feed's remark field -- only
// an operator-supplied Source.Remark may.
func TestObserversRemarkNeverLeaksAddr(t *testing.T) {
	s := testServer([]config.Source{
		{Name: "observer16", Type: "ubx", Addr: "10.0.0.2:2947"},
		{Name: "observer17", Type: "ubx", Addr: "192.168.1.5:2948", Remark: "roof antenna"},
	})
	s.refreshAll()
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	body := rr.Body.String()
	if strings.Contains(body, "10.0.0.2") || strings.Contains(body, "192.168.1.5") {
		t.Fatalf("observers body leaks a configured dial address: %s", body)
	}
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Observers[0].Remark != "" {
		t.Errorf("observer16 remark = %q, want empty (no operator remark configured)", env.Data.Observers[0].Remark)
	}
	if env.Data.Observers[1].Remark != "roof antenna" {
		t.Errorf("observer17 remark = %q, want the operator-supplied remark", env.Data.Observers[1].Remark)
	}
}

// TestObserversUnionsPushStations guards FeedStationRF/FeedCapabilityReports
// key stations by dial source OR authenticated push station id, and the detector
// already sees both -- but the observers feed must union in a push-fleet station
// that has no [[ingest]] entry at all, not just dial sources.
func TestObserversUnionsPushStations(t *testing.T) {
	s := testServer([]config.Source{{Name: "dial1", Type: "ubx", Addr: "10.0.0.2:2947"}})
	now := time.Now()
	// "pushstation1" never appears in cfg.Ingest -- only via authenticated RF telemetry.
	s.store.Apply(&ingest.RawFrame{Source: "pushstation1", Recv: now, RF: &ingest.RawRF{
		Bands: []ingest.RFBand{{Block: 0, AGC: 3000}},
	}})
	s.refresh("observers")
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Observers) != 2 {
		t.Fatalf("got %d observers, want 2 (dial1 + pushstation1): %+v", len(env.Data.Observers), env.Data.Observers)
	}
	var push *observer
	for i := range env.Data.Observers {
		if env.Data.Observers[i].ID == "pushstation1" {
			push = &env.Data.Observers[i]
		}
	}
	if push == nil {
		t.Fatal("pushstation1 missing from observers feed despite reporting RF telemetry")
	}
	if push.RF == nil {
		t.Error("pushstation1's RF data missing from its observer entry")
	}
}

// gpsLNAVWords/beidouD1Words/galileoINAVWords build the minimal Words payload
// each decoder needs to succeed (since recordCapability now fires only
// after a real decode, capability tests must route through frames the
// decoders actually accept, not an undersized placeholder). None of the
// decoded field values matter here -- only that decode succeeds.
func gpsLNAVWords() []uint32  { return make([]uint32, 10) }
func beidouD1Words() []uint32 { return make([]uint32, 10) }
func galileoINAVWords() []uint32 {
	w := make([]uint32, 8)
	w[4] = 0x80000000 // odd-part Even/Odd flag (page bit 128) must be 1
	return w
}

// TestObserversCarryCapabilities confirms a station's demonstrated (gnss, sig) fingerprint
// attaches to its observer record once it has produced nav frames (docs/OUTPUT.md §1.3,
// CONSTELLATIONS §7).
func TestObserversCarryCapabilities(t *testing.T) {
	s := testServer([]config.Source{{Name: "observer16", Type: "ubx", Addr: "10.0.0.2:2947"}})
	now := time.Now()
	// A GPS L1 nav frame and a Galileo E1-B I/NAV frame from this station.
	s.store.Apply(&ingest.RawFrame{Source: "observer16", GnssID: gnss.GPS, SigID: 0, Recv: now, Words: gpsLNAVWords()})
	s.store.Apply(&ingest.RawFrame{Source: "observer16", GnssID: gnss.Galileo, SigID: 0, Recv: now, Words: galileoINAVWords()})
	s.refresh("observers")
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Observers) != 1 {
		t.Fatalf("got %d observers, want 1", len(env.Data.Observers))
	}
	caps := env.Data.Observers[0].Capabilities
	if len(caps) != 2 {
		t.Fatalf("capabilities = %+v, want 2 signals", caps)
	}
	if caps[0].Gnss != int(gnss.GPS) || caps[1].Gnss != int(gnss.Galileo) || caps[1].Sig != 0 {
		t.Errorf("capabilities not the expected sorted set: %+v", caps)
	}
}

// TestObserversCapabilityMismatch confirms a node's declared-vs-observed mismatch surfaces in
// the feed: an observed signal outside the declared set is flagged unexpected, a declared
// signal never observed is flagged missing (docs/INTEGRITY.md §6).
func TestObserversCapabilityMismatch(t *testing.T) {
	s := testServer([]config.Source{{Name: "observer16", Type: "ubx", Addr: "10.0.0.2:2947"}})
	// Declared: GPS L1 (0:0) and Galileo I/NAV (2:0).
	s.store.SetDeclaredCapabilities(map[string][]state.CapSignal{
		"observer16": {{Gnss: 0, Sig: 0}, {Gnss: 2, Sig: 0}},
	})
	now := time.Now()
	// Observed: GPS L1 (declared, fine) + BeiDou D1 (3:0, not declared → unexpected).
	// Galileo I/NAV is declared but never seen → missing.
	s.store.Apply(&ingest.RawFrame{Source: "observer16", GnssID: gnss.GPS, SigID: 0, Recv: now, Words: gpsLNAVWords()})
	s.store.Apply(&ingest.RawFrame{Source: "observer16", GnssID: gnss.BeiDou, SigID: 0, Recv: now, Words: beidouD1Words()})
	s.refresh("observers")
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	o := env.Data.Observers[0]
	if len(o.Declared) != 2 {
		t.Errorf("declared = %+v, want 2", o.Declared)
	}
	if len(o.Unexpected) != 1 || o.Unexpected[0] != (state.CapSignal{Gnss: 3, Sig: 0}) {
		t.Errorf("unexpected = %+v, want [3:0]", o.Unexpected)
	}
	if len(o.Missing) != 1 || o.Missing[0] != (state.CapSignal{Gnss: 2, Sig: 0}) {
		t.Errorf("missing = %+v, want [2:0]", o.Missing)
	}
}

// TestMethodNotAllowed verifies non-GET/HEAD requests are rejected with 405 and an
// error envelope (docs/OUTPUT.md §0).
func TestMethodNotAllowed(t *testing.T) {
	s := testServer(nil)
	s.refreshAll()
	rr := httptest.NewRecorder()
	s.serveFeed("svs")(rr, httptest.NewRequest(http.MethodPost, "/gnss/api/v2/svs", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rr.Code)
	}
	var env struct {
		OK   bool `json:"ok"`
		Code int  `json:"code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Code != http.StatusMethodNotAllowed {
		t.Errorf("error envelope = %+v", env)
	}
}

// TestObserversCarryRF confirms a station's PNT-defense RF metrics attach to its observer
// record when the receiver has reported RF telemetry (docs/OUTPUT.md §1.3, DEFENSE-PNT §6).
func TestObserversCarryRF(t *testing.T) {
	s := testServer([]config.Source{{Name: "observer16", Type: "ubx", Addr: "10.0.0.2:2947"}})
	// Feed a clear-sky RF sample so the station's RF read model exists.
	for i := 0; i < 10; i++ {
		s.store.Apply(&ingest.RawFrame{
			Source: "observer16", Recv: time.Now(),
			RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 4000, AntStatus: 2}}},
		})
	}
	s.refresh("observers")
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Observers) != 1 || env.Data.Observers[0].RF == nil {
		t.Fatalf("observer RF not attached: %+v", env.Data.Observers)
	}
	if got := env.Data.Observers[0].RF.RFTrust; got != 1.0 {
		t.Errorf("rf_trust = %v, want 1.0 (clear sky)", got)
	}
}
