package state

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
)

// neighbourFleet builds a store with n fixed, surveyed stations on a grid of
// stepDeg around a base point, each with an established AGC baseline of 4000
// counts (restored, not learned, so the setup is cheap), and returns the
// instant after the baselines were installed.
func neighbourFleet(tb testing.TB, s *Store, n int, stepDeg float64) time.Time {
	tb.Helper()
	profiles := make(map[string]integrity.StationProfile, n)
	for i := 0; i < n; i++ {
		profiles[fmt.Sprintf("n%04d", i)] = integrity.StationProfile{
			Mode:     integrity.ModeFixed,
			Position: &integrity.Surveyed{LatDeg: 37.0 + stepDeg*float64(i%50), LonDeg: -122.0 + stepDeg*float64(i/50), HeightM: 10},
		}
	}
	cfg, err := NewIntegrityConfig(integrity.DefaultProfile(), profiles)
	if err != nil {
		tb.Fatal(err)
	}
	s.SetIntegrity(cfg)
	now := integrityT0
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("n%04d", i)
		if got := s.RestoreAGCBaselines(id, []AGCBaseline{{Block: 0, Baseline: 4000, BaselineAt: now}}, now); got != 1 {
			tb.Fatalf("%s: restored %d baselines, want 1", id, got)
		}
	}
	return now
}

// TestNeighbourNamedCapAndCount: a departed station names only the nearest
// maxNamedNeighbours corroborating neighbours (sorted by id) while NeighbourCount
// carries the total, so a regional jammer's event payload stays bounded whatever
// the fleet size; the integrity agc check consumes the total.
func TestNeighbourNamedCapAndCount(t *testing.T) {
	s := New(1)
	// Twelve corroborated neighbours strung north of n0000 at ~110 m intervals
	// (lexical id order is distance order), all within the 30 km radius.
	now := neighbourFleet(t, s, 13, 0.001)
	for i := 0; i < 13; i++ {
		s.Apply(rfSampleJam(fmt.Sprintf("n%04d", i), 3000, 2, now)) // departure + jam flag
	}
	rf := s.FeedStationRF(now)["n0000"]
	want := make([]string, 0, maxNamedNeighbours)
	for i := 1; i <= maxNamedNeighbours; i++ {
		want = append(want, fmt.Sprintf("n%04d", i))
	}
	if !slices.Equal(rf.Neighbours, want) {
		t.Fatalf("named neighbours = %v, want the %d nearest %v", rf.Neighbours, maxNamedNeighbours, want)
	}
	if rf.NeighbourCount != 12 {
		t.Fatalf("neighbour count = %d, want 12", rf.NeighbourCount)
	}
	// The next second's frames answer from the set the tick just built, and the
	// agc check takes the total, not the named few.
	next := now.Add(time.Second)
	for i := 0; i < 13; i++ {
		s.Apply(rfSampleJam(fmt.Sprintf("n%04d", i), 3000, 2, next))
	}
	if r := integrityResult(t, s.FeedStationIntegrity(next)["n0000"], integrity.CheckAGC); r.Metrics["neighbours"] != 12 {
		t.Fatalf("agc neighbours metric = %v, want the total 12", r.Metrics["neighbours"])
	}
}

// TestNeighbourCandidatesReusedWithinWindow: the per-frame path answers from the
// candidate set built within neighbourEvidenceReuse instead of rescanning, and a
// FeedStationRF call always rebuilds at its own instant.
func TestNeighbourCandidatesReusedWithinWindow(t *testing.T) {
	s := New(1)
	now := neighbourFleet(t, s, 3, 0.001)
	for i := 0; i < 3; i++ {
		s.Apply(rfSampleJam(fmt.Sprintf("n%04d", i), 3000, 2, now))
	}
	s.rfMu.Lock()
	// The per-frame builds during Apply saw the fleet one station at a time; a
	// tick (zero reuse) rebuilds at its own instant and sees all three.
	first := s.neighbourCandidates(now, 0)
	builtAt := s.neighbours.at
	again := s.neighbourCandidates(now.Add(neighbourEvidenceReuse), neighbourEvidenceReuse)
	reusedAt := s.neighbours.at
	rebuilt := s.neighbourCandidates(now.Add(neighbourEvidenceReuse+time.Second), neighbourEvidenceReuse)
	rebuiltAt := s.neighbours.at
	s.rfMu.Unlock()
	if len(first) != 3 || !builtAt.Equal(now) {
		t.Fatalf("tick build = %d candidates at %v, want 3 at %v", len(first), builtAt, now)
	}
	if len(again) != 3 || &first[0] != &again[0] || !reusedAt.Equal(now) {
		t.Fatalf("candidate set within the reuse window was rebuilt (%d entries, built %v)", len(again), reusedAt)
	}
	if !rebuiltAt.Equal(now.Add(neighbourEvidenceReuse+time.Second)) || len(rebuilt) != 3 {
		t.Fatalf("candidate set past the reuse window not rebuilt: built %v, %d entries", rebuiltAt, len(rebuilt))
	}
}

// BenchmarkFeedStationRFNeighbours: 2 000 stations, 500 of them departed with
// corroborated evidence, so every call answers 500 neighbour queries. The
// candidate set is built once per call, so the cost is O(N + departed × E).
func BenchmarkFeedStationRFNeighbours(b *testing.B) {
	s := New(1)
	now := neighbourFleet(b, s, 2000, 0.01)
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("n%04d", i)
		if i%4 == 0 {
			s.Apply(rfSampleJam(id, 3000, 2, now))
		} else {
			s.Apply(rfSample(id, 0, 4000, 0, 2, now))
		}
	}
	b.ResetTimer()
	for b.Loop() {
		s.FeedStationRF(now)
	}
}

// TestFeedSVsDoesNotMutateIntegrity: rendering the svs feed (every detect tick
// and every authenticated /svs request) derives the vote weights from the
// checks' served states without evaluating them, so the request's clock cannot
// age a station's checks out or move any check's since/evaluated_at.
func TestFeedSVsDoesNotMutateIntegrity(t *testing.T) {
	const board = "board-0001-aa"
	s := New(1)
	cfg, err := NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{board: fixedSite()})
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrity(cfg)
	for i := 0; i < 30; i++ {
		s.Apply(solutionFrame(i, true, 1))
	}
	at := integrityT0.Add(30 * time.Second)
	before := s.FeedStationIntegrity(at)[board]
	assured := 0
	for _, r := range before.Checks {
		if r.State == integrity.Assured {
			assured++
		}
	}
	if assured == 0 {
		t.Fatalf("setup: no assured check to protect: %+v", before.Checks)
	}

	// A feed render twenty minutes on, as an authenticated request would make it:
	// every check is past the staleness bound at that instant.
	s.FeedSVs(at.Add(20 * time.Minute))

	after := s.FeedStationIntegrity(at)[board]
	if len(after.Checks) != len(before.Checks) {
		t.Fatalf("check count changed: %d -> %d", len(before.Checks), len(after.Checks))
	}
	for i, r := range before.Checks {
		got := after.Checks[i]
		if got.Check != r.Check || got.State != r.State || got.Since != r.Since || got.EvaluatedAt != r.EvaluatedAt {
			t.Errorf("%s changed under the feed render: %s since %d at %d -> %s since %d at %d",
				r.Check, r.State, r.Since, r.EvaluatedAt, got.State, got.Since, got.EvaluatedAt)
		}
	}
	if after.State != before.State {
		t.Errorf("fused state changed under the feed render: %s -> %s", before.State, after.State)
	}
}

// BenchmarkFeedSVsIntegrityStations renders the svs feed with 1 000 integrity
// stations: the vote weights must not re-assess every station per call.
func BenchmarkFeedSVsIntegrityStations(b *testing.B) {
	s := New(4)
	profiles := make(map[string]integrity.StationProfile, 1000)
	for i := 0; i < 1000; i++ {
		profiles[fmt.Sprintf("s%04d", i)] = fixedSite()
	}
	cfg, err := NewIntegrityConfig(integrity.DefaultProfile(), profiles)
	if err != nil {
		b.Fatal(err)
	}
	s.SetIntegrity(cfg)
	for i := 0; i < 1000; i++ {
		for j := 0; j < 4; j++ {
			f := solutionFrame(j, true, 1)
			f.Source = fmt.Sprintf("s%04d", i)
			s.Apply(f)
		}
	}
	at := integrityT0.Add(10 * time.Second)
	for _, words := range [][]uint32{sf1Words(85), sf2Words(85, 205075516), sf3Words(85)} {
		s.Apply(&ingest.RawFrame{Recv: at, Source: "s0000", GnssID: gnss.GPS, SvID: 5, Words: words})
	}
	b.ResetTimer()
	for b.Loop() {
		s.FeedSVs(at)
	}
}
