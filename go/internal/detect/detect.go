package detect

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/ptudor/navlistener/internal/state"
)

// Event is one confirmed integrity transition (docs/OUTPUT.md §3). The daemon
// assigns the monotonic id when it persists the event; the detector fills the rest.
type Event struct {
	Time     time.Time
	SV       string
	Type     string
	OldValue string
	NewValue string
	Severity int
	Message  string
	Params   map[string]any
}

// Detector holds the per-(subject, metric) debounced state machines and confirms a
// transition only after it has persisted for the debounce window (docs/INTEGRITY.md
// §4). It is safe for one goroutine to call Tick on a cadence; the mutex guards
// against a concurrent reset.
type Detector struct {
	mu sync.Mutex
	// machines is keyed per (subject, metric) and never shrinks except via Reset
	// : an SV/station going away leaves its entries in place. This is a
	// bounded key space (finite SVs × signals × metrics + stations), not an
	// unbounded leak — documented here so it isn't later "fixed" as one. Pruning
	// on a TTL or read-model absence is a possible future optimization but not a
	// correctness requirement at this scale.
	machines map[string]*machine
	debounce time.Duration
	// gen counts completed classification rounds, one counter per classifier
	// family. Each Tick* entry point advances only its own counter and
	// classifies a DISJOINT set of (subject, metric) machines, so "was this
	// machine observed in the previous round?" is only a meaningful question
	// within a family — the four entry points run on the same daemon cadence but
	// are separate calls, and a shared counter would read every machine as
	// perpetually skipped.
	gen [numFamilies]uint64
}

// tickFamily identifies one classifier family's round counter. The key spaces are
// disjoint by construction: SV/xsig/SBAS subjects and metrics from Tick, the four
// RF metrics from TickStations, "offline" from TickStationLiveness, and
// "cap_lost:*"/"cap_impossible" from TickCapabilities.
type tickFamily int

const (
	familySV tickFamily = iota
	familyStationRF
	familyStationLiveness
	familyCapability
	numFamilies
)

// machine is one metric's state: the confirmed current classification, a pending
// provisional one, and when the pending one was first seen.
type machine struct {
	current     string
	provisional string
	since       time.Time
	// lastGen is the round of this machine's classifier family in which it was
	// last OBSERVED. A gap means the classifier held it (the regression fix
	// hold rule) or its subject was absent from the read model — either way the
	// pending provisional's dwell was not continuous.
	lastGen uint64
	// lastSeen is the wall time of the last observation, used to measure how
	// LONG a lastGen gap actually held the machine : only a hold at
	// least as long as the debounce window restarts the dwell.
	lastSeen time.Time
}

// New builds a Detector with the standard debounce window. A non-positive debounce
// uses DebounceDuration.
func New(debounce time.Duration) *Detector {
	if debounce <= 0 {
		debounce = DebounceDuration
	}
	return &Detector{machines: map[string]*machine{}, debounce: debounce}
}

// observe folds a new classification for one (subject, metric) into its state
// machine and reports whether this is a confirmed transition, with the old value.
// The first-ever observation seeds the current state silently (no phantom event).
// gen is the current round of the caller's classifier family (see tickFamily).
func (d *Detector) observe(subject, metric, newState string, now time.Time, gen uint64) (changed bool, old string) {
	key := subject + "\x00" + metric
	m := d.machines[key]
	if m == nil {
		d.machines[key] = &machine{current: newState, lastGen: gen, lastSeen: now}
		return false, "" // seed; first sighting is not a transition
	}
	// the debounce promises a CONTINUOUS dwell — the provisional state
	// must have been *observed* for the whole window, not merely have been
	// pending across it. Every hold path (the regression fix rule: the fleet-floor
	// silence skip below SilenceMinReceivers, an SBAS entry past
	// SBASHealthCurrentWindow, a station gone dark in detectCapability, missing
	// MON-RF bands, an absent subject) works by NOT calling observe, so a
	// provisional captured just before a hold used to keep its pre-hold `since`
	// and confirm on the FIRST post-resume observation with zero fresh dwell.
	// The fleet-floor skip makes that fleet-wide: one dip below the floor holds
	// every SV's silence machine at once. Fixed here in the spine rather than
	// at each hold site, so every present and future hold inherits it.
	//
	// regression fix bounds the restart by the hold's WALL length: an unconditional
	// restart on every interruption meant a machine held more often than once
	// per debounce window (with detectInterval 15 s and a 60 s window, any hold
	// recurring within 4 rounds) re-stamped `since` on every observation and
	// could NEVER confirm — a silent, permanent classifier outage on a genuine
	// continuous fault, the exact failure the defensive note below warns about.
	// A hold shorter than the window can hide at most (window − ε) of unobserved
	// state between two consistent observations that still span the full window,
	// so the accumulated dwell is kept; only a hold at least one full window
	// long forces the provisional to re-earn it from zero.
	//
	// The `!= gen` half is defensive: no current classifier observes one
	// (subject, metric) twice in a round (subjects come from map keys, metrics
	// are literals or per-signal keys), but if one ever did, reading the second
	// observation as "interrupted" would re-stamp `since` forever and the
	// machine could never confirm — a silent detector outage. Only a genuine gap
	// counts.
	interrupted := m.lastGen != gen && m.lastGen != gen-1
	heldFor := now.Sub(m.lastSeen)
	m.lastGen = gen
	m.lastSeen = now
	switch {
	case newState == m.current:
		m.provisional = "" // pending change reverted
		return false, ""
	case newState == m.provisional:
		if interrupted && heldFor >= d.debounce {
			m.since = now // held a full window: the dwell must start over
			return false, ""
		}
		if now.Sub(m.since) >= d.debounce {
			old = m.current
			m.current, m.provisional = newState, ""
			return true, old
		}
		return false, ""
	default:
		m.provisional, m.since = newState, now // start a new pending change
		return false, ""
	}
}

// Tick classifies the current SV and SBAS metrics, advances every state machine, and
// returns the transitions confirmed at this instant. Events are returned sorted for
// deterministic output. now is the wall clock; the daemon supplies the live read
// model (the same one the feeds serve). liveReceivers is the current live-station
// count (state.Store.LiveReceivers) — the fleet-footprint signal the silence
// classifier is gated on (regression fix, SilenceMinReceivers).
func (d *Detector) Tick(now time.Time, svs map[string]state.FeedSV, sbas map[string]state.SBASEntry, liveReceivers int) []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.run(now, familySV, func(emit emitFunc) {
		for name, sv := range svs {
			d.detectSV(name, sv, now, liveReceivers, emit)
		}
		d.detectXSig(svs, emit) // cross-signal broadcast agreement (needs the whole map)
		for prn, s := range sbas {
			d.detectSBAS(prn, s, emit)
		}
	})
}

// TickStations advances the station-scoped PNT-defense classifiers (jamming/spoofing/RF-
// degraded/antenna, docs/DEFENSE-PNT.md §4) over the same debounced state machines as the
// SV/SBAS metrics, so a station's RF events share the confirmation discipline and event
// contract. The daemon calls it alongside Tick on the detector cadence.
func (d *Detector) TickStations(now time.Time, stations map[string]state.StationRF) []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.run(now, familyStationRF, func(emit emitFunc) {
		for id, rf := range stations {
			d.detectStationRF(id, rf, emit)
		}
	})
}

// TickStationLiveness advances the station_offline classifier (regression fix, the regression fix
// wiring) over the same debounced machines. lastSeen is the UNFILTERED per-station
// age map (state.StationLastSeen): it must come from retained state, not the
// staleness-filtered StationRF map TickStations consumes, or an offline station
// vanishes from the read model before its machine can ever observe the outage —
// the same trap regression fix closed for SBAS.
func (d *Detector) TickStationLiveness(now time.Time, lastSeen map[string]int) []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.run(now, familyStationLiveness, func(emit emitFunc) {
		for id, age := range lastSeen {
			d.detectStationOffline(id, age, emit)
		}
	})
}

// run collects the events a classifier set produces, deterministically ordered. The
// caller holds d.mu (observe mutates the shared machines). fam names the classifier
// family whose round counter this call advances — the regression fix hold detection.
func (d *Detector) run(now time.Time, fam tickFamily, classify func(emit emitFunc)) []Event {
	d.gen[fam]++
	gen := d.gen[fam]
	var events []Event
	emit := func(subject, metric, newState string, ev func(old string) Event) {
		if changed, old := d.observe(subject, metric, newState, now, gen); changed {
			e := ev(old)
			e.Time, e.SV = now, subject
			events = append(events, e)
		}
	}
	classify(emit)
	// sort.Slice is not stable, and (SV, Type) alone is not a unique key --
	// a subject can emit several events of the same type in one tick (e.g. one
	// capability_signal_lost per lost signal at a station). Without a stable sort
	// plus a tertiary key, persisted gnss_events row order and SSE ids vary
	// run-to-run for that family. Message is a per-event free-form string but
	// distinct enough (it embeds the differentiating params, e.g. the signal
	// id) to give a deterministic tertiary order; SliceStable preserves emission
	// order for any remaining ties.
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].SV != events[j].SV {
			return events[i].SV < events[j].SV
		}
		if events[i].Type != events[j].Type {
			return events[i].Type < events[j].Type
		}
		return events[i].Message < events[j].Message
	})
	return events
}

// Reset clears all state machines (used at shutdown/tests). It does not emit.
// The round counters stay monotonic on purpose : every machine is gone,
// so the next observation of any subject is a fresh silent seed stamped with the
// current round — rewinding gen would buy nothing and could alias a stale lastGen.
func (d *Detector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.machines = map[string]*machine{}
}

// emitFunc is the closure Tick passes to the per-subject detectors.
type emitFunc func(subject, metric, newState string, ev func(old string) Event)

// detectSV runs every SV-level classifier for one satellite×signal.
func (d *Detector) detectSV(name string, sv state.FeedSV, now time.Time, liveReceivers int, outer emitFunc) {
	// every SV event carries the corroboration count at confirmation
	// time, stamped centrally so no classifier can forget it — a consumer must
	// always be able to tell "five stations agree" (conf ≥ 2) from "one station
	// said so" (conf 1) on the event itself, not just the live feed.
	// sigid rides beside it, because subjects are satellite×signal keys
	// (E14@0 and E14@3 are separate detector subjects) and one physical SV with
	// two decoded signals double-emits SV-level types at slightly different
	// instants — params.sv (the physical name, no @sig) is the documented
	// grouping key consumers coalesce on, and params.sigid disambiguates the
	// emitting signal without string-parsing the subject (docs/OUTPUT.md §3).
	emit := func(subject, metric, newState string, ev func(old string) Event) {
		outer(subject, metric, newState, func(old string) Event {
			e := ev(old)
			if e.Params == nil {
				e.Params = map[string]any{}
			}
			e.Params["conf"] = sv.Conf
			e.Params["sigid"] = sv.SigID
			return e
		})
	}
	// Health transition. QZSS/NavIC get their own event types (docs/INTEGRITY.md §5).
	// skip the classifier while health is unknown (health_code 0) — 0 is not a
	// broadcast value (regression fix serves it until an SV's health bits decode, or for iono-only
	// RAWX tracking). Classifying it would seed the machine at "0" and then fire a phantom
	// critical health_change on the unknown→decoded transition, or a spurious 1→0→1 pair
	// when a marginal SV drops to RAWX-only and reacquires. Like the disco gate below, treat
	// unknown as "no classification": the machine seeds on the first DECODED health.
	if sv.HealthCode != 0 {
		healthType, healthSev := healthEvent(sv.GnssID, sv.HealthIssueLevel)
		emit(name, "health", fmt.Sprintf("%d", sv.HealthCode), func(old string) Event {
			return Event{
				Type: healthType, OldValue: old, NewValue: fmt.Sprintf("%d", sv.HealthCode),
				Severity: healthSev,
				Message:  fmt.Sprintf("%s health %s→%d", sv.Name, old, sv.HealthCode),
				Params: map[string]any{"sv": sv.Name, "gnssid": sv.GnssID,
					"health_code": sv.HealthCode, "health_issue_level": sv.HealthIssueLevel},
			}
		})
	}

	// Ephemeris age crossing.
	if sv.EphAgeM != nil {
		aged := *sv.EphAgeM > ephAgeThreshold(sv.GnssID)
		emit(name, "eph_age", boolState(aged, "aged", "fresh"), func(old string) Event {
			return Event{
				Type: "eph_aged", OldValue: old, NewValue: boolState(aged, "aged", "fresh"),
				Severity: SevWarning,
				Message:  fmt.Sprintf("%s ephemeris age %.0f min", sv.Name, *sv.EphAgeM),
				Params:   map[string]any{"sv": sv.Name, "eph_age_m": *sv.EphAgeM},
			}
		})
	}

	// Orbit-disco band (absent = not computable; no classification).
	if sv.OrbitDiscoM != nil {
		band, sev := discoBand(*sv.OrbitDiscoM, OrbitDiscoThreshold, OrbitDiscoSevereThreshold)
		emit(name, "orbit_disco", band, func(old string) Event {
			return Event{
				Type: "orbit_disco", OldValue: old, NewValue: band, Severity: sev,
				Message: fmt.Sprintf("%s orbit disco %.2f m", sv.Name, *sv.OrbitDiscoM),
				Params:  map[string]any{"sv": sv.Name, "orbit_disco_m": *sv.OrbitDiscoM},
			}
		})
	}

	// Time-disco / clock jump band.
	if sv.TimeDiscoNs != nil {
		band, sev := discoBand(*sv.TimeDiscoNs, TimeDiscoThreshold, TimeDiscoSevereThreshold)
		emit(name, "clock_jump", band, func(old string) Event {
			return Event{
				Type: "clock_jump", OldValue: old, NewValue: band, Severity: sev,
				Message: fmt.Sprintf("%s clock jump %.2f ns", sv.Name, *sv.TimeDiscoNs),
				Params:  map[string]any{"sv": sv.Name, "time_disco_ns": *sv.TimeDiscoNs},
			}
		})
	}

	// SISA/URA accuracy degradation, with hysteresis so a boundary value doesn't flap.
	// an accuracy index that decodes to the ICD's "no accuracy prediction —
	// use at own risk" sentinel (GPS/QZSS URA 15 per IS-GPS-200N §20.3.3.3.1.3, CNAV
	// URA_ED 15/−16, Galileo SISA 255/spare) has no metres value, so sisa_m is
	// absent — but it MUST still classify. Mapping the sentinel to absence
	// structurally skipped this classifier, silencing the one broadcast field that
	// says "do not trust this SV's accuracy". acc_index distinguishes that sentinel
	// from "no accuracy field decoded yet" (both serve sisa_m-less entries).
	switch {
	case sv.SISAM != nil:
		band := d.sisaBand(name, *sv.SISAM)
		emit(name, "sisa", band, func(old string) Event {
			return Event{
				Type: "sisa_change", OldValue: old, NewValue: band, Severity: SevWarning,
				Message: fmt.Sprintf("%s SISA %.2f m", sv.Name, *sv.SISAM),
				Params:  map[string]any{"sv": sv.Name, "sisa_m": *sv.SISAM},
			}
		})
	case sv.AccIndex != nil && sv.AccIndexRawOnly:
		// a raw accuracy index with NO published metres table (BeiDou
		// B-CNAV2 SISAI — the B2a ICD v1.0 defers the tables). Unlike the
		// sentinel branch below, every index value here is a distinct state,
		// so a broadcast accuracy revision fires a sisa_change even though no
		// metres threshold can ever classify it. Info severity: the semantics
		// of the values are unpublished, so a change is notable, not an alarm.
		emit(name, "sisa", fmt.Sprintf("raw_%d", *sv.AccIndex), func(old string) Event {
			return Event{
				Type: "sisa_change", OldValue: old, NewValue: fmt.Sprintf("raw_%d", *sv.AccIndex), Severity: SevInfo,
				Message: fmt.Sprintf("%s broadcast raw accuracy index changed to %d (no published decode table)", sv.Name, *sv.AccIndex),
				Params:  map[string]any{"sv": sv.Name, "acc_index": *sv.AccIndex},
			}
		})
	case sv.AccIndex != nil:
		emit(name, "sisa", "no_accuracy", func(old string) Event {
			return Event{
				Type: "sisa_change", OldValue: old, NewValue: "no_accuracy", Severity: SevWarning,
				Message: fmt.Sprintf("%s broadcast accuracy index %d: no accuracy prediction — use at own risk", sv.Name, *sv.AccIndex),
				Params:  map[string]any{"sv": sv.Name, "acc_index": *sv.AccIndex},
			}
		})
	}

	// URA-alert flag transition : the SV's own broadcast "URA may
	// be worse than indicated — use at own risk" declaration (IS-GPS-200N
	// §20.3.3.2 HOW bit 18 / CNAV bit 38), one of the ICD's §6.4.6.3 marginal
	// conditions — surfaced alongside health/URA changes instead of being dropped
	// at decode. Absent (nil) = flag not decoded: no classification (regression fix rule).
	if sv.Alert != nil {
		raised := *sv.Alert
		sev := SevInfo
		if raised {
			sev = SevWarning // marginal, not critical — the URA/health detectors carry the hard states
		}
		emit(name, "ura_alert", boolState(raised, "raised", "clear"), func(old string) Event {
			return Event{
				Type: "ura_alert", OldValue: old, NewValue: boolState(raised, "raised", "clear"),
				Severity: sev,
				Message:  fmt.Sprintf("%s URA alert flag %s", sv.Name, boolState(raised, "raised", "clear")),
				Params:   map[string]any{"sv": sv.Name, "alert": raised},
			}
		})
	}

	// Broadcast week-number anomaly : the SV's transmitted week (LNAV
	// 10-bit / CNAV 13-bit, rollover-disambiguated in state) disagrees with the
	// collector wall-clock week — an upload error, an SV time fault, or a
	// replayed/spoofed signal carrying a plausible TOW under a wrong week (the
	// cheapest time-domain anomaly in docs/DEFENSE-PNT.md's plausibility set).
	// Critical on mismatch; debounced like every metric here.
	if sv.WnMismatch != nil {
		bad := *sv.WnMismatch
		sev := SevInfo
		if bad {
			sev = SevCritical
		}
		emit(name, "wn", boolState(bad, "mismatch", "ok"), func(old string) Event {
			return Event{
				Type: "wn_mismatch", OldValue: old, NewValue: boolState(bad, "mismatch", "ok"),
				Severity: sev,
				Message:  fmt.Sprintf("%s broadcast week number %s wall-clock week", sv.Name, map[bool]string{true: "disagrees with", false: "matches"}[bad]),
				Params:   map[string]any{"sv": sv.Name, "wn_mismatch": bad},
			}
		})
	}

	// BeiDou B-CNAV2 per-signal integrity flags (regression fix, BDS-SIS-ICD-B2a v1.0
	// Table 7-23): DIF/SIF/AIF are the constellation's own real-time
	// per-signal integrity channel, broadcast in every message (~3 s) — a
	// satellite actively flagging "my broadcast ephemeris exceeds its accuracy
	// bound" (DIF) or "this signal is abnormal" (SIF) must surface even while
	// HS still reads healthy. Each flag combination is its own debounced
	// state; warning severity when any flag is raised (the ICD defers the
	// flags' numeric thresholds to a future update, so a raise is a warning,
	// not an automatic critical), info on return to clear. Absent (nil) = flag
	// block never decoded: no classification (regression fix rule).
	if sv.Dif != nil && sv.Sif != nil && sv.Aif != nil {
		flagState := "ok"
		if s := integrityFlagState(*sv.Dif, *sv.Sif, *sv.Aif); s != "" {
			flagState = s
		}
		sev := SevInfo
		if flagState != "ok" {
			sev = SevWarning
		}
		emit(name, "bds_integrity", flagState, func(old string) Event {
			return Event{
				Type: "bds_integrity_flag", OldValue: old, NewValue: flagState, Severity: sev,
				Message: fmt.Sprintf("%s B2a integrity flags %s", sv.Name, flagState),
				Params: map[string]any{"sv": sv.Name, "dif": *sv.Dif, "sif": *sv.Sif,
					"aif": *sv.Aif, "sismai": derefInt(sv.Sismai)},
			}
		})
	}

	// Broadcast-vs-configured leap-second cross-check : the SV's
	// broadcast UTC set carries the current leap count (BeiDou B-CNAV2 MT34's
	// BDT-UTC ΔtLS today); state compares it — through the fixed BDT↔GPST
	// alignment — against the collector's configured GPS−UTC count (	// compiled-in/config value). A mismatch means every UTC conversion this
	// collector performs is shifted by whole seconds: a stale config after a
	// real leap event, or a bogus broadcast. Warning, not critical — the
	// GNSS-side time axes (all leap-free) are unaffected; only UTC-facing
	// output is. Absent (nil) = no UTC set decoded: no classification.
	if sv.LeapMismatch != nil {
		bad := *sv.LeapMismatch
		sev := SevInfo
		if bad {
			sev = SevWarning
		}
		emit(name, "leap", boolState(bad, "mismatch", "ok"), func(old string) Event {
			return Event{
				Type: "leap_mismatch", OldValue: old, NewValue: boolState(bad, "mismatch", "ok"),
				Severity: sev,
				Message:  fmt.Sprintf("%s broadcast leap-second count %s configured value", sv.Name, map[bool]string{true: "disagrees with", false: "matches"}[bad]),
				Params:   map[string]any{"sv": sv.Name, "leap_mismatch": bad, "dt_ls": derefInt(sv.DtLS)},
			}
		})
	}

	// Galileo OSNMA authentication presence (regression fix, INTEGRITY.md §7 v1): the
	// promised osnma_change on↔off transition. Info severity per the
	// INTEGRITY.md event table — presence going away is expected operational
	// behavior (the distributing subset changes dynamically per the OSNMA ICD),
	// but a fleet-wide flat-off during an active spoofing scenario is exactly
	// the corroborating signal DEFENSE-PNT wants on record. Absent (nil) = no
	// OSNMA field observed: no classification (regression fix rule).
	if sv.Osnma != nil {
		on := *sv.Osnma
		emit(name, "osnma", boolState(on, "on", "off"), func(old string) Event {
			return Event{
				Type: "osnma_change", OldValue: old, NewValue: boolState(on, "on", "off"),
				Severity: SevInfo,
				Message:  fmt.Sprintf("%s OSNMA authentication %s", sv.Name, boolState(on, "on", "off")),
				Params:   map[string]any{"sv": sv.Name, "osnma": on},
			}
		})
	}

	// Silence (observation lost). classified only when the fleet is at
	// least plausibly constellation-footprint-sized (SilenceMinReceivers) — the
	// event's "always in view" premise holds for a distributed fleet, not for one
	// station's sky, where every non-GEO SV goes silent once per orbital pass and
	// the classifier manufactured a confirmed false warning pair per pass. Below
	// the floor the classifier is skipped entirely (the regression fix hold-state rule:
	// machines keep their last confirmed state, and nothing seeds), so a fleet
	// dipping below the floor mid-silence neither fires nor falsely recovers.
	if liveReceivers >= SilenceMinReceivers {
		silent := float64(sv.LastSeenS) > SilentThreshold
		emit(name, "silence", boolState(silent, "silent", "seen"), func(old string) Event {
			return Event{
				Type: "observation_lost", OldValue: old, NewValue: boolState(silent, "silent", "seen"),
				Severity: SevWarning,
				Message:  fmt.Sprintf("%s unseen %ds", sv.Name, sv.LastSeenS),
				Params:   map[string]any{"sv": sv.Name, "last_seen_s": sv.LastSeenS},
			}
		})
	}

	// A monitored SV with no computable position.
	unknown := sv.XM == nil
	emit(name, "position", boolState(unknown, "unknown", "known"), func(old string) Event {
		return Event{
			Type: "position_unknown", OldValue: old, NewValue: boolState(unknown, "unknown", "known"),
			Severity: SevWarning,
			Message:  fmt.Sprintf("%s position %s", sv.Name, boolState(unknown, "unknown", "known")),
			Params:   map[string]any{"sv": sv.Name},
		}
	})
}

// detectXSig checks cross-signal broadcast agreement. One physical Galileo SV
// broadcasts the same IODnav-scoped CED on E1-B I/NAV (E##@0) and E5a F/NAV (E##@3), so the two
// independently-decoded, independently-propagated positions must agree — a
// disagreement under one IODnav means the two signals carried DIFFERENT element
// bits, a signal-selective fault or spoof no single-signal detector can see.
// Comparison preconditions (all skips leave the machine holding, regression fix rule):
// both entries carry a fresh position, the same POSITION-producing IODnav
// (PosIOD, stamped with the position in Propagate — a changeover skew where one
// signal cuts over first is designed behavior, not divergence; the served
// current-data-set `iod` would mislabel a pre-changeover position in the window
// between apply and the next Propagate tick), and the identical propagation
// epoch (PosAtUnixNs — sub-second epoch skew reads as km of fake divergence). Galileo-only: see XSigDivergenceMeters for
// why the tight bound is unsound for GPS LNAV-vs-CNAV / BDS D1-vs-B-CNAV2
// (independent curve fits). Subject is the physical SV name ("E14" — no @sig,
// deliberately outside the satellite×signal subject space); warning severity in
// v1 — a single-collector observation (one receiver may have a decode fault),
// so it corroborates rather than convicts, per the DEFENSE-PNT single-metric
// rule. It also counts toward plausibility-gate family.
func (d *Detector) detectXSig(svs map[string]state.FeedSV, emit emitFunc) {
	groups := map[string][]state.FeedSV{}
	for _, sv := range svs {
		if sv.GnssID != 2 { // Galileo only (see XSigDivergenceMeters)
			continue
		}
		if sv.XM == nil || sv.YM == nil || sv.ZM == nil || sv.PosIOD == nil || sv.PosAtUnixNs == 0 {
			continue
		}
		groups[sv.Name] = append(groups[sv.Name], sv)
	}
	for name, g := range groups {
		if len(g) < 2 {
			continue
		}
		// Deterministic pair reporting regardless of map iteration order.
		sort.Slice(g, func(i, j int) bool { return g[i].SigID < g[j].SigID })
		compared := false
		worst := 0.0
		var sigA, sigB, iod int
		for i := 0; i < len(g); i++ {
			for j := i + 1; j < len(g); j++ {
				a, b := g[i], g[j]
				if *a.PosIOD != *b.PosIOD || a.PosAtUnixNs != b.PosAtUnixNs {
					continue
				}
				dx, dy, dz := *a.XM-*b.XM, *a.YM-*b.YM, *a.ZM-*b.ZM
				dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
				if !compared || dist > worst {
					worst, sigA, sigB, iod = dist, a.SigID, b.SigID, *a.PosIOD
				}
				compared = true
			}
		}
		if !compared {
			continue // no same-IODnav same-epoch pair: nothing comparable this tick
		}
		divergent := worst > XSigDivergenceMeters
		sev := SevInfo
		if divergent {
			sev = SevWarning
		}
		emit(name, "xsig", boolState(divergent, "divergent", "ok"), func(old string) Event {
			return Event{
				Type: "xsig_divergence", OldValue: old, NewValue: boolState(divergent, "divergent", "ok"),
				Severity: sev,
				Message: fmt.Sprintf("%s signals %d/%d %s (Δ %.3f m, IODnav %d)",
					name, sigA, sigB, map[bool]string{true: "broadcast diverging ephemerides", false: "agree"}[divergent], worst, iod),
				Params: map[string]any{"sv": name, "sig_a": sigA, "sig_b": sigB,
					"distance_m": worst, "iod": iod},
			}
		})
	}
}

// detectSBAS runs the silence and augmentation-health classifiers for one SBAS
// PRN. The entry comes from state.SBASDetect — the UNFILTERED store view
//  — so a dark PRN stays observable here after the served feed drops it.
func (d *Detector) detectSBAS(prn string, s state.SBASEntry, emit emitFunc) {
	subject := "S" + prn

	// Silence (sbas_lost, regression fix): the SBAS mirror of observation_lost, with no
	// visibility caveat — a GEO never sets, so a PRN unseen past the feed's own
	// staleness boundary is a real regional-augmentation outage (or a station-side
	// loss the station detectors corroborate), never rise/set noise.
	silent := float64(s.LastSeenS) > SBASSilentThreshold
	emit(subject, "sbas_silence", boolState(silent, "silent", "seen"), func(old string) Event {
		return Event{
			Type: "sbas_lost", OldValue: old, NewValue: boolState(silent, "silent", "seen"),
			Severity: SevWarning,
			Message:  fmt.Sprintf("SBAS %s (%s) unseen %ds", prn, s.Provider, s.LastSeenS),
			Params:   map[string]any{"prn": prn, "provider": s.Provider, "last_seen_s": s.LastSeenS},
		}
	})
	// Past the latch horizon, the stored health is the regression fix MT0 latch decayed
	// for lack of input, not a current observation — hold the health machine
	// rather than classify stale data (the regression fix unknown-is-not-ok rule; in
	// particular a do-not-use latch must not "recover" to OK just because the
	// MT0s stopped along with everything else when the PRN went dark). Note this
	// window (60 s) is deliberately tighter than SBASSilentThreshold: between
	// them, both machines hold and only the climbing last_seen_s tells the story.
	if float64(s.LastSeenS) > SBASHealthCurrentWindow {
		return
	}

	band := "ok"
	sev := SevInfo
	if s.HealthCode == 3 { // do-not-use
		band, sev = "do_not_use", SevCritical
	}
	emit(subject, "sbas_health", band, func(old string) Event {
		return Event{
			Type: "sbas_health", OldValue: old, NewValue: band, Severity: sev,
			Message: fmt.Sprintf("SBAS %s (%s) %s", prn, s.Provider, band),
			Params:  map[string]any{"prn": prn, "provider": s.Provider, "health_code": s.HealthCode},
		}
	})
}

// healthEvent maps a constellation to its health event type and severity: QZSS and
// NavIC carry their own types (docs/INTEGRITY.md §5); the rest use health_change.
// severity follows health_issue_level (2 error → critical, 1 warning →
// warning) for every constellation — the previous unconditional SevCritical on the
// default arm meant a GPS SV flagging a routine signal-component code (an ICD
// §6.4.6.3 "marginal", e.g. an L2-only issue while L1 C/A is fine) manufactured a
// critical alert, training operators to ignore the event type.
func healthEvent(gnssID, issueLevel int) (string, int) {
	sev := SevWarning
	if issueLevel >= 2 {
		sev = SevCritical
	}
	switch gnssID {
	case 5: // QZSS
		return "qzss_health", sev
	case 7: // NavIC
		return "navic_health", sev
	default:
		return "health_change", sev
	}
}

// discoBand classifies a discontinuity magnitude into ok/warn/crit and returns the
// event severity for the band.
func discoBand(v, warn, severe float64) (string, int) {
	switch {
	case v >= severe:
		return "crit", SevCritical
	case v >= warn:
		return "warn", SevWarning
	default:
		return "ok", SevInfo
	}
}

// sisaBand classifies accuracy as ok/degraded with real hysteresis : it
// enters degraded at SISAAlertThreshold but only clears back to ok below the
// lower SISAExitThreshold. Without this, a quantized URA/SISA value that
// legitimately dwells on both sides of a plain threshold for minutes at a time
// (longer than the debounce window) produces a confirmed sisa_change pair on
// every dwell; the asymmetric band absorbs that dithering. subject looks up the
// last CONFIRMED band for this SV so the hysteresis is anchored to the
// machine's actual current state, not a provisional/pending one.
func (d *Detector) sisaBand(subject string, m float64) string {
	if prev, ok := d.currentBand(subject, "sisa"); ok && prev == "degraded" {
		if m < SISAExitThreshold {
			return "ok"
		}
		return "degraded"
	}
	if m >= SISAAlertThreshold {
		return "degraded"
	}
	return "ok"
}

// currentBand looks up the last CONFIRMED classification for one (subject, metric)
// state machine, without mutating it. The caller must hold d.mu (true for every
// detectSV/detectStationRF/detectCapability call, which run inside d.run).
func (d *Detector) currentBand(subject, metric string) (string, bool) {
	m := d.machines[subject+"\x00"+metric]
	if m == nil {
		return "", false
	}
	return m.current, true
}

func boolState(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

// derefInt renders an optional feed int for event params: the value, or nil when
// the field was absent (never a fabricated zero).
func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// integrityFlagState renders a raised-flag combination as a stable state string
// ("dif", "dif+sif", …, in fixed DIF/SIF/AIF order), or "" when all are clear.
func integrityFlagState(dif, sif, aif bool) string {
	var s string
	add := func(on bool, name string) {
		if !on {
			return
		}
		if s != "" {
			s += "+"
		}
		s += name
	}
	add(dif, "dif")
	add(sif, "sif")
	add(aif, "aif")
	return s
}
