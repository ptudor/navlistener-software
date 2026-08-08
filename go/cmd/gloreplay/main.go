// Command gloreplay replays a captured UBX stream through the real ingest and
// live-state path and reports the distribution of the GLONASS orbit/time
// discontinuity measured at every tb changeover.
//
// This is the measurement half of regression fix (technical validation
// and technical validation regression fix). The structural fix landed — the
// position difference is taken at the changeover midpoint and both metrics gate on
// tb adjacency — but the remaining question is calibration: whether a ROUTINE
// changeover still crosses the 1.45 m warn band the sub-metre-continuity Kepler
// family uses, i.e. whether GLONASS needs its own threshold pair (the review's
// option (c)). That cannot be answered from first principles, only measured, and
// the rule is that thresholds change only with the measurement in hand.
//
// Usage:
//
//	gloreplay -capture glo_20260807_1400.ubx [-duration 6h] [-json out.json]
//
// The capture is a raw UBX byte stream (what `cat /dev/ttyACM0 > file.ubx`
// produces with UBX-RXM-SFRBX enabled). It carries no timestamps, so gloreplay
// imposes a synthetic receive clock that advances uniformly across the frames.
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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
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
	shards := flag.Int("shards", 16, "live-state shard count")
	jsonOut := flag.String("json", "", "also write the per-changeover samples to this JSON file")
	flag.Parse()

	if *capture == "" {
		fmt.Fprintln(os.Stderr, "gloreplay: -capture is required")
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*capture, *duration, *shards, *jsonOut); err != nil {
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

	if jsonOut != "" {
		b, err := json.MarshalIndent(samples, "", "  ")
		if err != nil {
			return fmt.Errorf("encode samples: %w", err)
		}
		// 0644: a measurement artifact, not a secret.
		if err := os.WriteFile(jsonOut, b, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", jsonOut, err)
		}
		fmt.Printf("\nwrote %d samples to %s\n", len(samples), jsonOut)
	}
	return nil
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
	watch := newTbWatch()
	var samples []changeover

	err = ingest.ReplayUBX(f, "gloreplay", clock, func(fr *ingest.RawFrame) {
		st.Apply(fr)
		if fr.GnssID != gnss.GLONASS {
			return
		}
		// Only this SV's entry can have changed, so read it by key rather than
		// scanning the whole feed per frame.
		key := state.Key{G: fr.GnssID, Sv: fr.SvID, Sig: fr.SigID}.Name()
		sv, ok := st.FeedSVs(now)[key]
		if !ok || sv.EphAgeM == nil {
			return
		}
		if !watch.observe(key, *sv.EphAgeM) {
			return // no new tb adopted at this frame
		}
		// A changeover was just published. Absent disco is a real outcome, not a
		// gap in the data: it means the adjacency or freshness gate declined to
		// define one, and the rate of that is itself part of the measurement.
		samples = append(samples, changeover{
			SV:         key,
			OrbitM:     sv.OrbitDiscoM,
			TimeNs:     sv.TimeDiscoNs,
			FrameIndex: frame,
		})
	}, func(kind string) { parseErrs[kind]++ })
	if err != nil {
		return nil, nil, err
	}
	return samples, parseErrs, nil
}

func report(path string, total, gloFrames int, spacing time.Duration, samples []changeover, parseErrs map[string]int) {
	fmt.Printf("capture:      %s\n", path)
	fmt.Printf("frames:       %d total, %d GLONASS\n", total, gloFrames)
	fmt.Printf("synth clock:  %v per frame (%v span)\n", spacing, time.Duration(total)*spacing)
	if len(parseErrs) > 0 {
		fmt.Printf("parse errors: %v\n", parseErrs)
	}
	fmt.Printf("changeovers:  %d observed\n", len(samples))
	if len(samples) == 0 {
		fmt.Println("\nNo tb changeover occurred in this capture. GLONASS updates tb every 30-60 min")
		fmt.Println("(GLO-ICD-5.1 Table 4.3), so a capture must span at least that to measure one;")
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
