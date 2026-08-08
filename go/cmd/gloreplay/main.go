// Command gloreplay replays a captured UBX stream through the real ingest and
// live-state path and reports the distribution of the GLONASS orbit/time
// discontinuity measured at every tb changeover.
//
// Position differences are measured at the changeover midpoint, with both metrics
// gated on tb adjacency. Measurements determine whether GLONASS needs separate
// thresholds from the Kepler family. Change thresholds only after calibration.
//
// Two sources, same measurement:
//
//	gloreplay -from-store "$DSN" -since 2026-08-08T02:50:09Z [-until …] [-source obs]
//	gloreplay -capture glo_20260807_1400.ubx [-duration 6h]
//
// **Prefer -from-store.** It is the historian's own record, so no capture has to
// have been taken in advance, any collector's history is queryable, and every frame
// carries the receiver's real reception time — none of the synthetic-clock reasoning
// below applies. This is the design's "replayable over stored raw frames" promise
// (docs/DESIGN.md §1) actually exercised rather than asserted.
//
// -capture is the fallback for when there is no collector to have persisted to (an
// outage, or a receiver on a bench). It reads a raw UBX byte stream — what a plain
// redirect of the serial port produces with UBX-RXM-SFRBX enabled. That stream
// carries no timestamps, so gloreplay imposes a synthetic receive clock that advances
// uniformly across the frames.
//
// On why that is sound: the only wall-clock input to computeGloDisco is the
// discoTrustAge (4 h) freshness gate on the OUTGOING set. Non-adjacent sets — the
// case that gate is really protecting against — are already excluded by the
// independent tb-adjacency gate (gloDiscoMaxTk = 60 min, GLO-ICD-5.1 Table 4.3),
// which reads broadcast time out of the frames themselves and so is unaffected by
// the synthetic clock. The default clock therefore COMPRESSES the capture (1 ms
// per frame), which keeps every outgoing set trivially "fresh" and lets the tb
// gate do the real work. Pass -duration with the capture's true wall-clock span
// when you want the freshness gate exercised as it would be in production — which
// matters only for a capture containing multi-hour reception gaps.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
)

// Warn/alert bands the shipped detector applies to these metrics. GLONASS
// currently shares the Kepler family's pair; whether it should is exactly what
// this tool measures (regression fix option (c)).
const (
	orbitDiscoWarnM   = 1.45
	orbitDiscoAlertM  = 10.0
	timeDiscoWarnNs   = 2.5
	timeDiscoAlertNs  = 25.0
	defaultFrameSpace = time.Millisecond
)

// ephAgeDropMin is how far the served GLONASS eph_age_m must fall between two
// consecutive samples of one SV for the drop to mean "a new tb was adopted"
// rather than clock jitter. Ages otherwise only grow, and a real changeover
// resets the age by at least the update interval (30 min minimum, Table 4.3), so
// a 1-minute floor separates the two without assuming the interval.
const ephAgeDropMin = 1.0

// tbWatch spots GLONASS tb changeovers from the served eph_age_m alone. The feed
// does not publish tb, but it publishes the age derived from it, and a changeover
// is the only thing that makes that age fall.
//
// Caveat worth knowing when reading results: eph_age_m comes from
// gnsstime.EphAgeDay, which wraps the broadcast-day difference into +/-12 h. A
// changeover whose age difference straddles that wrap registers as a large jump
// rather than a ~30 min drop. It stays a detection (the drop test still passes)
// but the once-per-replay-per-SV possibility of a spurious or missed edge there
// is real, and is why the tool reports the changeover COUNT alongside the
// distribution — a count wildly off the capture-span/30 min expectation is the
// signal that something in the sampling, not the math, needs looking at.
type tbWatch struct{ last map[string]float64 }

func newTbWatch() *tbWatch { return &tbWatch{last: map[string]float64{}} }

// observe records this sample's age for sv and reports whether it marks a
// changeover. The first observation of an SV never counts: with no previous set
// there is nothing to difference against, which is exactly why computeGloDisco
// itself returns early on a zero gloEphAt. Counting it would put an undefined
// disco into the population.
func (w *tbWatch) observe(sv string, age float64) bool {
	prev, seen := w.last[sv]
	w.last[sv] = age
	return seen && prev-age >= ephAgeDropMin
}

// changeover is one observed GLONASS tb transition for one SV.
type changeover struct {
	SV         string   `json:"sv"`
	OrbitM     *float64 `json:"orbit_disco_m"`
	TimeNs     *float64 `json:"time_disco_ns"`
	FrameIndex int      `json:"frame_index"`
}

func main() {
	capture := flag.String("capture", "", "path to a raw UBX capture (UBX-RXM-SFRBX stream)")
	duration := flag.Duration("duration", 0, "true wall-clock span of the capture; 0 compresses (see package doc)")
	dsn := flag.String("from-store", "", "replay from the historian instead of a file: a TimescaleDB DSN")
	since := flag.String("since", "", "-from-store: window start, RFC3339 (required)")
	until := flag.String("until", "", "-from-store: window end, RFC3339 (default: no upper bound)")
	source := flag.String("source", "", "-from-store: restrict to one ingest source / observer id")
	shards := flag.Int("shards", 16, "live-state shard count")
	jsonOut := flag.String("json", "", "also write the per-changeover samples to this JSON file")
	flag.Parse()

	switch {
	case *capture == "" && *dsn == "":
		fmt.Fprintln(os.Stderr, "gloreplay: one of -capture or -from-store is required")
		flag.Usage()
		os.Exit(2)
	case *capture != "" && *dsn != "":
		fmt.Fprintln(os.Stderr, "gloreplay: -capture and -from-store are mutually exclusive")
		os.Exit(2)
	}

	var err error
	if *dsn != "" {
		err = runStore(*dsn, *since, *until, *source, *shards, *jsonOut)
	} else {
		err = run(*capture, *duration, *shards, *jsonOut)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gloreplay: %v\n", err)
		os.Exit(1)
	}
}

func run(capture string, duration time.Duration, shards int, jsonOut string) error {
	total, gloFrames, err := countFrames(capture)
	if err != nil {
		return err
	}
	if total == 0 {
		return fmt.Errorf("%s: no UBX-RXM-SFRBX frames parsed — is this a raw UBX capture with SFRBX enabled?", capture)
	}
	if gloFrames == 0 {
		return fmt.Errorf("%s: %d frames parsed but none are GLONASS (gnssId 6) — nothing to calibrate", capture, total)
	}

	spacing := defaultFrameSpace
	if duration > 0 {
		spacing = duration / time.Duration(total)
		if spacing <= 0 {
			spacing = time.Nanosecond
		}
	}

	samples, parseErrs, err := replay(capture, spacing, shards)
	if err != nil {
		return err
	}

	report(capture, total, gloFrames, spacing, samples, parseErrs)

	return writeJSON(jsonOut, samples)
}

// writeJSON persists the per-changeover samples. A no-op when no path was given.
func writeJSON(path string, samples []changeover) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(samples, "", "  ")
	if err != nil {
		return fmt.Errorf("encode samples: %w", err)
	}
	// 0644: a measurement artifact, not a secret.
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("\nwrote %d samples to %s\n", len(samples), path)
	return nil
}

// runStore replays from the historian. This is the path that makes the design's
// "replayable over stored raw frames" promise real: no capture file has to have been
// taken in advance, any collector's history is queryable, and — the substantive
// difference from a file — every frame carries the receiver's own reception time, so
// none of the file path's synthetic-clock reasoning applies.
func runStore(dsn, sinceStr, untilStr, source string, shards int, jsonOut string) error {
	if sinceStr == "" {
		return fmt.Errorf("-since is required with -from-store (RFC3339)")
	}
	since, err := time.Parse(time.RFC3339, sinceStr)
	if err != nil {
		return fmt.Errorf("parse -since: %w", err)
	}
	var until time.Time
	if untilStr != "" {
		if until, err = time.Parse(time.RFC3339, untilStr); err != nil {
			return fmt.Errorf("parse -until: %w", err)
		}
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(ctx, config.Store{DSN: dsn}, log)
	if err != nil {
		return fmt.Errorf("open historian: %w", err)
	}
	defer st.Close()

	glo := int(gnss.GLONASS)
	live := state.New(shards)
	smp := samplerOn(live)

	qErr := st.QueryNavFrames(ctx, store.NavFrameQuery{
		GnssID: &glo, SourceID: source, Since: since, Until: until,
	}, func(r store.StoredNavFrame) error {
		fr := &ingest.RawFrame{
			Recv:   r.ReceivedAt,
			Source: r.SourceID,
			GnssID: gnss.GNSSID(r.GnssID),
			SvID:   r.SvID,
			SigID:  r.SigID,
			FreqID: r.FreqID,
			Words:  wordsFromRaw(r.Raw),
		}
		smp.apply(fr, r.ReceivedAt)
		return nil
	})
	if qErr != nil && !errors.Is(qErr, store.ErrNavFrameLimit) {
		return qErr
	}

	fmt.Printf("source:       historian %s\n", redactDSN(dsn))
	fmt.Printf("window:       %s .. %s\n", since.Format(time.RFC3339), untilStr)
	fmt.Printf("frames:       %d GLONASS replayed with real reception times\n", smp.frames)
	if errors.Is(qErr, store.ErrNavFrameLimit) {
		fmt.Printf("\nWARNING: %v\n", qErr)
		fmt.Println("The distribution below is built from a TRUNCATED head of the window and understates it.")
	}
	fmt.Printf("changeovers:  %d observed\n", len(smp.samples))
	reportSamples(smp.samples)

	return writeJSON(jsonOut, smp.samples)
}

// wordsFromRaw reverses RawBytes for a word-oriented frame: the persisted `raw` is
// the nav words stored big-endian back to back. A trailing partial word cannot occur
// for SFRBX and is dropped rather than zero-extended, which would invent bits.
func wordsFromRaw(b []byte) []uint32 {
	w := make([]uint32, 0, len(b)/4)
	for i := 0; i+4 <= len(b); i += 4 {
		w = append(w, binary.BigEndian.Uint32(b[i:i+4]))
	}
	return w
}

// redactDSN keeps a connection string out of stdout: a QA run's output gets pasted
// into review docs and tickets, and the DSN carries a password.
func redactDSN(dsn string) string {
	if i := strings.Index(dsn, "@"); i >= 0 {
		return "…@" + dsn[i+1:]
	}
	return "(local)"
}

// countFrames is the sizing pass: the synthetic clock's spacing depends on how
// many frames the capture holds, which is only knowable by parsing it.
func countFrames(path string) (total, glonass int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	clock := func() time.Time { return time.Unix(0, 0) }
	err = ingest.ReplayUBX(f, "gloreplay", clock, func(fr *ingest.RawFrame) {
		total++
		if fr.GnssID == gnss.GLONASS {
			glonass++
		}
	}, func(string) {})
	return total, glonass, err
}

// replay runs the capture through the real live-state Apply path, sampling the
// served feed after every GLONASS frame to catch each tb changeover as it is
// published. Sampling through the feed (rather than reaching into svState) keeps
// this tool on the same exported surface the collector serves, so what it
// measures is what a consumer would see.
func replay(path string, spacing time.Duration, shards int) ([]changeover, map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	st := state.New(shards)
	base := time.Unix(1_700_000_000, 0).UTC()
	frame := 0
	now := base
	clock := func() time.Time {
		frame++
		now = base.Add(time.Duration(frame) * spacing)
		return now
	}

	parseErrs := map[string]int{}
	smp := samplerOn(st)

	err = ingest.ReplayUBX(f, "gloreplay", clock, func(fr *ingest.RawFrame) {
		smp.apply(fr, now)
	}, func(kind string) { parseErrs[kind]++ })
	if err != nil {
		return nil, nil, err
	}
	return smp.samples, parseErrs, nil
}

// sampler feeds frames through live state and records every GLONASS tb changeover
// the store publishes. Shared by both replay sources so a file replay and a store
// replay measure identically — the point of replay is that the numbers describe what
// the collector computes, which fails the moment two paths sample differently.
type sampler struct {
	st      *state.Store
	watch   *tbWatch
	samples []changeover
	frames  int
}

func samplerOn(st *state.Store) *sampler {
	return &sampler{st: st, watch: newTbWatch()}
}

// apply feeds one frame through live state. now must be the frame's reception time:
// the file path synthesises it, the store path has the receiver's real stamp.
func (s *sampler) apply(fr *ingest.RawFrame, now time.Time) {
	s.frames++
	s.st.Apply(fr)
	if fr.GnssID != gnss.GLONASS {
		return
	}
	// Only this SV's entry can have changed, so read it by key rather than
	// scanning the whole feed per frame.
	key := state.Key{G: fr.GnssID, Sv: fr.SvID, Sig: fr.SigID}.Name()
	sv, ok := s.st.FeedSVs(now)[key]
	if !ok || sv.EphAgeM == nil {
		return
	}
	if !s.watch.observe(key, *sv.EphAgeM) {
		return // no new tb adopted at this frame
	}
	// A changeover was just published. Absent disco is a real outcome, not a gap in
	// the data: it means the adjacency or freshness gate declined to define one, and
	// the rate of that is itself part of the measurement.
	s.samples = append(s.samples, changeover{
		SV:         key,
		OrbitM:     sv.OrbitDiscoM,
		TimeNs:     sv.TimeDiscoNs,
		FrameIndex: s.frames,
	})
}

func report(path string, total, gloFrames int, spacing time.Duration, samples []changeover, parseErrs map[string]int) {
	fmt.Printf("capture:      %s\n", path)
	fmt.Printf("frames:       %d total, %d GLONASS\n", total, gloFrames)
	fmt.Printf("synth clock:  %v per frame (%v span)\n", spacing, time.Duration(total)*spacing)
	if len(parseErrs) > 0 {
		fmt.Printf("parse errors: %v\n", parseErrs)
	}
	fmt.Printf("changeovers:  %d observed\n", len(samples))
	reportSamples(samples)
}

// reportSamples prints the distribution. Shared by both sources so a file replay and
// a store replay are compared on identical arithmetic.
func reportSamples(samples []changeover) {
	if len(samples) == 0 {
		fmt.Println("\nNo tb changeover occurred in this window. GLONASS updates tb every 30-60 min")
		fmt.Println("(GLO-ICD-5.1 Table 4.3), so a window must span at least that to measure one;")
		fmt.Println("regression fix asks for >= 6 h to get a population rather than a single sample.")
		return
	}

	orbit := make([]float64, 0, len(samples))
	timeNs := make([]float64, 0, len(samples))
	var orbitAbsent, timeAbsent int
	for _, s := range samples {
		if s.OrbitM != nil {
			orbit = append(orbit, *s.OrbitM)
		} else {
			orbitAbsent++
		}
		if s.TimeNs != nil {
			timeNs = append(timeNs, *s.TimeNs)
		} else {
			timeAbsent++
		}
	}

	fmt.Println()
	summarize("orbit_disco_m", orbit, orbitAbsent, orbitDiscoWarnM, orbitDiscoAlertM)
	fmt.Println()
	summarize("time_disco_ns", timeNs, timeAbsent, timeDiscoWarnNs, timeDiscoAlertNs)

	fmt.Println("\nRJU-334 option (c) decision input: if a large fraction of ROUTINE changeovers")
	fmt.Println("exceeds the warn band above, the band is mis-set for GLONASS and the constellation")
	fmt.Println("needs its own pair; if p95 sits well inside it, the shared band stands as shipped.")
}

func summarize(name string, v []float64, absent int, warn, alert float64) {
	fmt.Printf("%s: %d measured, %d absent (gated)\n", name, len(v), absent)
	if len(v) == 0 {
		return
	}
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	var overWarn, overAlert int
	for _, x := range sorted {
		if x >= warn {
			overWarn++
		}
		if x >= alert {
			overAlert++
		}
	}
	fmt.Printf("  p50 %.4g   p95 %.4g   max %.4g\n", percentile(sorted, 50), percentile(sorted, 95), sorted[len(sorted)-1])
	fmt.Printf("  >= warn (%.4g): %d/%d (%.1f%%)   >= alert (%.4g): %d/%d (%.1f%%)\n",
		warn, overWarn, len(sorted), 100*float64(overWarn)/float64(len(sorted)),
		alert, overAlert, len(sorted), 100*float64(overAlert)/float64(len(sorted)))
}

// percentile returns the nearest-rank percentile of an already-sorted slice.
// Nearest-rank (rather than an interpolating definition) keeps every reported
// figure a value that was actually measured, which is what a threshold decision
// should be argued from.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(float64(len(sorted))*p/100 + 0.5)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
