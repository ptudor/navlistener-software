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
//	gloreplay -capture glo_20260807_1400.ubx -start 2026-08-07T14:00:00Z -duration 6h
//
// Beyond the changeover disco distributions, the tool follows every DEFERRED
// time-disco to its outcome. State adopts a new tb at the string-3-triggered
// assembly, before the same frame's string 4 (the SV clock) has arrived — so at
// the changeover instant the time-disco is pending by construction, and a
// sampler that reads the feed only at that instant misfiles nearly every
// completed measurement as absent (the first regression fix run's "111 gated"). The
// resolution section reports immediate/completed/superseded/censored counts,
// completion lags, and per-SV string delivery; see resolve.go.
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
// The synthetic clock has TWO consumers on the GLONASS path, so -duration is
// effectively required alongside -start, not an option: (1) computeGloDisco's
// discoTrustAge (4 h) freshness gate on the outgoing set — non-adjacent sets are
// independently excluded by the tb-adjacency gate (gloDiscoMaxTk = 60 min,
// GLO-ICD-5.1 Table 4.3), which reads broadcast time out of the frames and is
// clock-immune; and (2) the regression fix/regression fix frame-coherence windows over string
// reception times (state's glonassFrameWindow, 8 s), which decide WHICH strings
// may assemble into one set — including whether string 4's clock joins it. The
// default compressed clock (1 ms per frame) keeps (1) trivially satisfied but
// breaks (2)'s premise: at typical capture frame rates ~30 s of broadcast
// compresses inside the 8 s window, so strings from DIFFERENT broadcast frames
// pass the coherence gate and can assemble chimera sets at changeovers, and
// every reported completion lag is compressed with the clock. Pass the
// capture's true wall-clock span for any real measurement; the compressed
// default is only good as a parse smoke check.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/detect"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
)

// Warn/alert bands the shipped detector applies to these metrics, sourced from
// the detector's own constants so the tool cannot drift from what ships. The
// first regression fix run printed a 25 ns alert band — a literal transcribed from the
// regression fix runbook, which mis-stated TimeDiscoSevereThreshold; the detector has
// shipped 10 ns all along (detect/thresholds.go, docs/INTEGRITY.md §2), and
// against the real band the 2026-08-08 capture's routine time-disco max of
// 9.99 ns sits 0.1% below CRIT — the calibration fact the duplicate hid.
// GLONASS currently shares the Kepler family's pair; whether it should is
// exactly what this tool measures (regression fix option (c)).
const (
	orbitDiscoWarnM   = detect.OrbitDiscoThreshold
	orbitDiscoAlertM  = detect.OrbitDiscoSevereThreshold
	timeDiscoWarnNs   = detect.TimeDiscoThreshold
	timeDiscoAlertNs  = detect.TimeDiscoSevereThreshold
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

// changeover is one observed GLONASS tb transition for one SV. OrbitM/TimeNs
// are the served values AT the changeover instant; the Time* tail fields are
// the deferral's eventual outcome (resolve.go) — TimeNs nil with TimeNsLate set
// means the pendClk path completed the measurement after the sampler's first
// look, which is its designed behavior, not a gap in the data.
type changeover struct {
	SV         string   `json:"sv"`
	OrbitM     *float64 `json:"orbit_disco_m"`
	TimeNs     *float64 `json:"time_disco_ns"`
	FrameIndex int      `json:"frame_index"`
	// TimeOutcome is one of the outcome* constants (resolve.go).
	TimeOutcome string `json:"time_outcome"`
	// TimeNsLate/TimeLagS: the completed value and how long after the changeover
	// the feed first served it. S4LagS: how long after the changeover the first
	// string 4 was SEEN for this SV (set for completed/superseded/censored alike
	// — its presence without a completion is the state-side failure signature).
	TimeNsLate *float64 `json:"time_disco_ns_late,omitempty"`
	TimeLagS   *float64 `json:"time_disco_lag_s,omitempty"`
	S4LagS     *float64 `json:"first_string4_lag_s,omitempty"`
}

func main() {
	capture := flag.String("capture", "", "path to a raw UBX capture (UBX-RXM-SFRBX stream)")
	duration := flag.Duration("duration", 0, "true wall-clock span of the capture; 0 compresses (see package doc)")
	startAt := flag.String("start", "", "capture's real start time, RFC3339 — REQUIRED for GLONASS (see package doc)")
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
		err = run(*capture, *duration, *startAt, *shards, *jsonOut)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gloreplay: %v\n", err)
		os.Exit(1)
	}
}

func run(capture string, duration time.Duration, startAt string, shards int, jsonOut string) error {
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
	} else {
		// See the package doc: the regression fix/regression fix frame-coherence windows read the
		// replay clock too, and under the compressed default ~30 s of broadcast
		// fits inside state's 8 s window — cross-frame chimera assemblies at
		// changeovers, and every completion lag compressed. Same severity as the
		// missing -start warning below: results are unreliable, not just coarse.
		fmt.Fprintln(os.Stderr, "gloreplay: WARNING -duration not given; the synthetic clock compresses the")
		fmt.Fprintln(os.Stderr, "  capture to 1 ms/frame, so strings from DIFFERENT broadcast frames fit the")
		fmt.Fprintln(os.Stderr, "  8 s frame-coherence window  and can assemble chimera sets at")
		fmt.Fprintln(os.Stderr, "  changeovers. Pass the capture's true wall-clock span for a real measurement.")
	}

	base := time.Unix(1_700_000_000, 0).UTC()
	if startAt != "" {
		t, perr := time.Parse(time.RFC3339, startAt)
		if perr != nil {
			return fmt.Errorf("parse -start: %w", perr)
		}
		base = t.UTC()
	} else {
		fmt.Fprintln(os.Stderr, "gloreplay: WARNING -start not given; GLONASS tb is a time-of-DAY,")
		fmt.Fprintln(os.Stderr, "  so an arbitrary epoch offsets every eph_age and can fold changeovers")
		fmt.Fprintln(os.Stderr, "  across the EphAgeDay +/-12 h wrap. Results are unreliable without it.")
	}

	smp, parseErrs, diag, err := replay(capture, base, spacing, shards)
	if err != nil {
		return err
	}

	report(capture, total, gloFrames, spacing, smp, parseErrs)
	if diag != nil {
		fmt.Println("\n--- diagnostic: final GLONASS feed state ---")
		diag()
	}

	return writeJSON(jsonOut, smp.samples)
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
	st, err := store.OpenReader(ctx, dsn)
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
	smp.finish() // censor deferrals still pending at the window's end

	fmt.Printf("source:       historian %s\n", redactDSN(dsn))
	fmt.Printf("window:       %s .. %s\n", since.Format(time.RFC3339), untilStr)
	fmt.Printf("frames:       %d GLONASS replayed with real reception times\n", smp.frames)
	if errors.Is(qErr, store.ErrNavFrameLimit) {
		fmt.Printf("\nWARNING: %v\n", qErr)
		fmt.Println("The distribution below is built from a TRUNCATED head of the window and understates it.")
	}
	fmt.Printf("changeovers:  %d observed\n", len(smp.samples))
	reportSamples(smp.samples, smp.strs)

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
// published — and every frame after a deferring changeover, to catch the pendClk
// completion when the feed first serves it. Sampling through the feed (rather
// than reaching into svState) keeps this tool on the same exported surface the
// collector serves, so what it measures is what a consumer would see.
func replay(path string, base time.Time, spacing time.Duration, shards int) (*sampler, map[string]int, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	defer f.Close()

	st := state.New(shards)
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
		return nil, nil, nil, err
	}
	smp.finish() // censor deferrals still pending at the capture's end
	diag := func() {
		feed := st.FeedSVs(now)
		n := 0
		for k, sv := range feed {
			if k == "" || k[0] != 'R' {
				continue
			}
			n++
			age := "nil"
			if sv.EphAgeM != nil {
				age = fmt.Sprintf("%.1f min", *sv.EphAgeM)
			}
			pos := "no"
			if sv.XM != nil {
				pos = "yes"
			}
			ch := "nil"
			if sv.FreqCh != nil {
				ch = fmt.Sprintf("%d", *sv.FreqCh)
			}
			fmt.Printf("  %-8s eph_age=%-12s pos=%-4s freq_ch=%s\n", k, age, pos, ch)
		}
		fmt.Printf("  (%d GLONASS entries in feed of %d total)\n", n, len(feed))
	}
	return smp, parseErrs, diag, nil
}

// sampler feeds frames through live state and records every GLONASS tb changeover
// the store publishes. Shared by both replay sources so a file replay and a store
// replay measure identically — the point of replay is that the numbers describe what
// the collector computes, which fails the moment two paths sample differently.
type sampler struct {
	st      *state.Store
	watch   *tbWatch
	pend    *pendSet
	strs    map[string]*svStrings
	samples []changeover
	frames  int
}

func samplerOn(st *state.Store) *sampler {
	return &sampler{st: st, watch: newTbWatch(), pend: newPendSet(), strs: map[string]*svStrings{}}
}

// strings returns (creating if needed) the per-SV string-delivery counters.
func (s *sampler) strings(key string) *svStrings {
	c := s.strs[key]
	if c == nil {
		c = &svStrings{}
		s.strs[key] = c
	}
	return c
}

// finish finalizes deferrals still pending when the replay's data ran out.
// Call once, after the last frame and before reporting.
func (s *sampler) finish() { s.pend.finish(s.samples) }

// apply feeds one frame through live state. now must be the frame's reception time:
// the file path synthesises it, the store path has the receiver's real stamp.
func (s *sampler) apply(fr *ingest.RawFrame, now time.Time) {
	s.frames++
	s.st.Apply(fr)
	if fr.GnssID != gnss.GLONASS {
		return
	}
	// State keys every GLONASS signal into one Sig-0 entry (applyGlonass; 	// L2OF shares the L1OF entry) — sample the feed at that key regardless of the
	// frame's own SigID. Keying by fr.SigID made every L2OF frame look up a
	// nonexistent feed entry, so an L2OF-triggered adoption or completion was
	// only observed at the next L1OF frame, charging seconds of sampling delay
	// to the detector.
	key := state.Key{G: fr.GnssID, Sv: fr.SvID, Sig: 0}.Name()
	// Count decoded string numbers for the delivery table, mirroring state's own
	// slot guard (1..24, regression fix): out-of-range frames never reach an svState,
	// so counting them would describe traffic the detector cannot see.
	isS4 := false
	if fr.SvID >= 1 && fr.SvID <= 24 {
		if num, err := gloStringNumber(fr.Words); err != nil {
			s.strings(key).err++
		} else {
			s.strings(key).count(num)
			isS4 = num == 4
		}
	}
	if isS4 {
		s.pend.sawS4(key, now)
	}
	// Only this SV's entry can have changed, so read it by key rather than
	// scanning the whole feed per frame.
	sv, ok := s.st.FeedSVs(now)[key]
	if !ok || sv.EphAgeM == nil {
		return
	}
	if s.watch.observe(key, *sv.EphAgeM) {
		// A changeover was just published. Absent orbit disco is a real outcome,
		// not a gap in the data: the adjacency or freshness gate declined to
		// define one, and the rate of that is itself part of the measurement.
		// An absent time-disco is different — state defers it while the incoming
		// set is clockless (string 4 arrives ~2 s after string 3), so finalize
		// the previous deferral (the pend is voided inside computeGloDisco's
		// publish at every changeover) and start watching this one.
		s.pend.supersede(s.samples, key)
		c := changeover{SV: key, OrbitM: sv.OrbitDiscoM, TimeNs: sv.TimeDiscoNs, FrameIndex: s.frames}
		if sv.TimeDiscoNs != nil {
			c.TimeOutcome = outcomeImmediate
		} else {
			s.pend.arm(key, len(s.samples), now)
		}
		s.samples = append(s.samples, c)
		return
	}
	if sv.TimeDiscoNs != nil {
		// Between changeovers the only nil→value writer on the served
		// time_disco_ns is the pendClk completion (applyGlonass), so a value
		// appearing while a deferral is armed belongs to that changeover.
		// No-op when nothing is pending.
		s.pend.complete(s.samples, key, *sv.TimeDiscoNs, now)
	}
}

func report(path string, total, gloFrames int, spacing time.Duration, smp *sampler, parseErrs map[string]int) {
	fmt.Printf("capture:      %s\n", path)
	fmt.Printf("frames:       %d total, %d GLONASS\n", total, gloFrames)
	fmt.Printf("synth clock:  %v per frame (%v span)\n", spacing, time.Duration(total)*spacing)
	if len(parseErrs) > 0 {
		fmt.Printf("parse errors: %v\n", parseErrs)
	}
	fmt.Printf("changeovers:  %d observed\n", len(smp.samples))
	reportSamples(smp.samples, smp.strs)
}

// reportSamples prints the distributions and the deferral resolution. Shared by
// both sources so a file replay and a store replay are compared on identical
// arithmetic.
func reportSamples(samples []changeover, strs map[string]*svStrings) {
	if len(samples) == 0 {
		fmt.Println("\nNo tb changeover occurred in this window. GLONASS updates tb every 30-60 min")
		fmt.Println("(GLO-ICD-5.1 Table 4.3), so a window must span at least that to measure one;")
		fmt.Println("regression fix asks for >= 6 h to get a population rather than a single sample.")
		reportStrings(strs)
		return
	}

	orbit := make([]float64, 0, len(samples))
	timeNs := make([]float64, 0, len(samples))
	timeAll := make([]float64, 0, len(samples))
	var orbitAbsent, timeAbsent, timeUnresolved int
	for _, s := range samples {
		if s.OrbitM != nil {
			orbit = append(orbit, *s.OrbitM)
		} else {
			orbitAbsent++
		}
		switch {
		case s.TimeNs != nil:
			timeNs = append(timeNs, *s.TimeNs)
			timeAll = append(timeAll, *s.TimeNs)
		case s.TimeNsLate != nil:
			timeAbsent++
			timeAll = append(timeAll, *s.TimeNsLate)
		default:
			timeAbsent++
			timeUnresolved++
		}
	}

	fmt.Println()
	summarize("orbit_disco_m", orbit, orbitAbsent, "absent (gated)", orbitDiscoWarnM, orbitDiscoAlertM)
	fmt.Println()
	summarize("time_disco_ns (at the changeover instant)", timeNs, timeAbsent,
		"deferred at the instant — resolution below", timeDiscoWarnNs, timeDiscoAlertNs)
	fmt.Println()
	reportResolution(samples)
	fmt.Println()
	summarize("time_disco_ns (combined: immediate + completed)", timeAll, timeUnresolved,
		"never resolved (superseded or censored)", timeDiscoWarnNs, timeDiscoAlertNs)

	fmt.Println("\nRJU-334 option (c) decision input: judge orbit_disco_m and the COMBINED")
	fmt.Println("time_disco_ns. If a large fraction of ROUTINE changeovers exceeds a warn band,")
	fmt.Println("that band is mis-set for GLONASS and the constellation needs its own pair; if")
	fmt.Println("p95 sits well inside it, the shared band stands as shipped.")

	reportStrings(strs)
}

func summarize(name string, v []float64, absent int, absentLabel string, warn, alert float64) {
	fmt.Printf("%s: %d measured, %d %s\n", name, len(v), absent, absentLabel)
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
