package detect

import (
	"fmt"
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
}

// machine is one metric's state: the confirmed current classification, a pending
// provisional one, and when the pending one was first seen.
type machine struct {
	current     string
	provisional string
	since       time.Time
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
func (d *Detector) observe(subject, metric, newState string, now time.Time) (changed bool, old string) {
	key := subject + "\x00" + metric
	m := d.machines[key]
	if m == nil {
		d.machines[key] = &machine{current: newState}
		return false, "" // seed; first sighting is not a transition
	}
	switch {
	case newState == m.current:
		m.provisional = "" // pending change reverted
		return false, ""
	case newState == m.provisional:
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
// model (the same one the feeds serve).
func (d *Detector) Tick(now time.Time, svs map[string]state.FeedSV, sbas map[string]state.SBASEntry) []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.run(now, func(emit emitFunc) {
		for name, sv := range svs {
			d.detectSV(name, sv, now, emit)
		}
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
	return d.run(now, func(emit emitFunc) {
		for id, rf := range stations {
			d.detectStationRF(id, rf, emit)
		}
	})
}

// run collects the events a classifier set produces, deterministically ordered. The
// caller holds d.mu (observe mutates the shared machines).
func (d *Detector) run(now time.Time, classify func(emit emitFunc)) []Event {
	var events []Event
	emit := func(subject, metric, newState string, ev func(old string) Event) {
		if changed, old := d.observe(subject, metric, newState, now); changed {
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
func (d *Detector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.machines = map[string]*machine{}
}

// emitFunc is the closure Tick passes to the per-subject detectors.
type emitFunc func(subject, metric, newState string, ev func(old string) Event)

// detectSV runs every SV-level classifier for one satellite×signal.
func (d *Detector) detectSV(name string, sv state.FeedSV, now time.Time, emit emitFunc) {
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

	// Silence (observation lost).
	silent := float64(sv.LastSeenS) > SilentThreshold
	emit(name, "silence", boolState(silent, "silent", "seen"), func(old string) Event {
		return Event{
			Type: "observation_lost", OldValue: old, NewValue: boolState(silent, "silent", "seen"),
			Severity: SevWarning,
			Message:  fmt.Sprintf("%s unseen %ds", sv.Name, sv.LastSeenS),
			Params:   map[string]any{"sv": sv.Name, "last_seen_s": sv.LastSeenS},
		}
	})

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

// detectSBAS runs the augmentation-health classifier for one SBAS PRN.
func (d *Detector) detectSBAS(prn string, s state.SBASEntry, emit emitFunc) {
	band := "ok"
	sev := SevInfo
	if s.HealthCode == 3 { // do-not-use
		band, sev = "do_not_use", SevCritical
	}
	subject := "S" + prn
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
