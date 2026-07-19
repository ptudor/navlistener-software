package state

import (
	"fmt"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/accuracy"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/gnss/geo"
	"github.com/ptudor/gnss/glonass"
	"github.com/ptudor/gnss/gnsstime"
	"github.com/ptudor/gnss/physconst"
)

// The read-model that backs the native v2 feeds (docs/OUTPUT.md §1). These types
// are built from the live SV state under the shard locks and handed to the serve
// package, which wraps them in the response envelope. Optional fields are pointers
// so an unknown/not-yet-computable value is absent from the JSON rather than a
// sentinel (the "absent = unknown" contract, §1.1) — never a magic number.

// FeedSV is one satellite×signal entry in the svs feed (docs/OUTPUT.md §1.1).
type FeedSV struct {
	FullName         string   `json:"full_name"`
	Name             string   `json:"name"`
	GnssID           int      `json:"gnssid"`
	SvID             int      `json:"svid"`
	SigID            int      `json:"sigid"`
	HealthCode       int      `json:"health_code"`
	HealthIssueLevel int      `json:"health_issue_level"`
	HealthSubcode    int      `json:"health_subcode"`
	EphAgeM          *float64 `json:"eph_age_m,omitempty"`
	SISAValid        bool     `json:"sisa_valid"`
	SISAM            *float64 `json:"sisa_m,omitempty"`
	// AccIndex is the raw broadcast accuracy index (URA / URA_ED / SISA per the
	// constellation's table), present whenever an accuracy field has been decoded
	// — including the "no accuracy prediction" sentinels (GPS/QZSS URA 15, CNAV
	// URA_ED 15/−16, Galileo SISA 255), which map to no sisa_m value. 	// without the raw index, "index says do-not-trust" (IS-GPS-200N
	// §20.3.3.3.1.3: use at own risk) was indistinguishable from "no accuracy
	// field decoded yet", and the sentinel produced zero integrity signal.
	AccIndex *int `json:"acc_index,omitempty"`
	// AccIndexRawOnly (regression fix, detector-facing — not part of the JSON
	// contract): true when AccIndex is a raw index for which NO metres table
	// exists at all (BeiDou B-CNAV2 SISAI, packed oe<<11|ocb<<6|oc1<<3|oc2 —
	// the accSISAIRaw doc), as opposed to a table-backed index currently at a
	// "no accuracy prediction" sentinel. The sisa classifier uses it to track
	// raw-index CHANGES instead of collapsing every value into one
	// "no_accuracy" state.
	AccIndexRawOnly bool `json:"-"`
	// Alert is the GPS/QZSS broadcast URA-alert flag (regression fix/regression fix, IS-GPS-200N
	// §20.3.3.2 HOW bit 18 / §6.4.6.3 CNAV bit 38): true = the SV itself declares
	// its URA may be worse than broadcast — use at own risk. Absent until decoded
	// (and for constellations without the flag).
	Alert *bool `json:"alert,omitempty"`
	// WnMismatch : the broadcast week number (LNAV 10-bit / CNAV 13-bit,
	// rollover-disambiguated) disagrees with the collector wall-clock week — an
	// upload error, SV time fault, or replayed/spoofed signal. Absent until a
	// broadcast WN has been decoded for this entry.
	WnMismatch *bool `json:"wn_mismatch,omitempty"`
	// GpsOffsetNs and the a0g/a1g/t0g/wn0g quartet are the broadcast
	// system→GPS time offset (regression fix; Galileo GGTO today, docs/OUTPUT.md §1.1
	// reserves the same fields for the other constellations' offsets).
	// GpsOffsetNs is Eq. 24's Δt_systems = t_system − t_GPS evaluated at the
	// feed instant (GAL-OS-SIS-ICD-2.2 §5.1.8 — note the sign: positive means
	// the system's time scale is AHEAD of GPS); the quartet is the raw
	// broadcast polynomial (a0g s, a1g s/s, t0g s, wn0g truncated week) so a
	// consumer can re-evaluate at its own epoch. All absent until decoded, and
	// absent again after the §5.1.8 all-ones broadcast withdrawal — "absent =
	// unknown", never a stale offset.
	GpsOffsetNs *float64 `json:"gps_offset_ns,omitempty"`
	A0G         *float64 `json:"a0g,omitempty"`
	A1G         *float64 `json:"a1g,omitempty"`
	T0G         *int     `json:"t0g,omitempty"`
	WN0G        *int     `json:"wn0g,omitempty"`
	// KlobAlpha/KlobBeta and Bdgim  are the SV's raw broadcast
	// ionosphere coefficient sets, served so they are queryable and (via the
	// gnss_snapshots feed dumps) replayable — a broadcast iono-coefficient
	// anomaly is a known spoofing tell, and evaluator will need
	// them. Today: BeiDou B1I D1 subframe-1 α/β (B1I §5.2.4.7 Table 5-5 — the
	// BDS model, NOT interchangeable with GPS's Klobuchar) on C##@0 entries,
	// and B-CNAV2 MT30's BDGIM α1..α9 (B2a Table 7-10, TECu) on C##@8
	// entries. No evaluated delay is served from these yet — model evaluation
	// is regression fix, and iono_model_m stays absent until it lands.
	KlobAlpha *[4]float64 `json:"klob_alpha,omitempty"`
	KlobBeta  *[4]float64 `json:"klob_beta,omitempty"`
	Bdgim     *[9]float64 `json:"bdgim,omitempty"`
	// Dif/Sif/Aif and Sismai (regression fix, BeiDou B-CNAV2 entries) are the B2a
	// signal's broadcast real-time integrity flags, refreshed ~every 3 s
	// (BDS-SIS-ICD-B2a v1.0 Table 7-23): dif=true — the broadcast message
	// parameters exceed their predictive accuracy; sif=true — the signal is
	// abnormal; aif=true — the SISMAI value is invalid. Sismai is the raw
	// 4-bit monitoring-accuracy index (§7.17 — numeric semantics deferred by
	// the ICD, served raw). Absent until the flag block has decoded.
	Dif    *bool `json:"dif,omitempty"`
	Sif    *bool `json:"sif,omitempty"`
	Aif    *bool `json:"aif,omitempty"`
	Sismai *int  `json:"sismai,omitempty"`
	// UtcOffsetNs and the leap-schedule quartet are the SV's broadcast
	// system→UTC offset (regression fix; BeiDou B-CNAV2 MT34's BDT-UTC set today,
	// docs/OUTPUT.md §1.1 reserves the same fields for the other
	// constellations' UTC sets). UtcOffsetNs is BDS-SIS-ICD-B2a v1.0 Eq. 7-25's
	// ΔtUTC = ΔtLS + A0UTC + A1UTC·dt + A2UTC·dt² evaluated at the feed
	// instant (positive = the system's time scale is AHEAD of UTC), with the
	// Eq. 7-29 arm's ΔtLSF substituted once the WNLSF/DN leap event is in the
	// past. DtLS/DtLSF/WnLSF/Dn are the raw broadcast leap schedule so a
	// consumer can handle a pending leap itself. LeapMismatch is the regression fix
	// cross-check: the broadcast current leap count disagrees with this
	// collector's configured GPS−UTC count — a stale config (or a leap event
	// the config missed) that would silently shift every UTC conversion.
	// All absent until an SV's UTC set has decoded — "absent = unknown".
	UtcOffsetNs  *float64 `json:"utc_offset_ns,omitempty"`
	DtLS         *int     `json:"dt_ls,omitempty"`
	DtLSF        *int     `json:"dt_lsf,omitempty"`
	WnLSF        *int     `json:"wn_lsf,omitempty"`
	Dn           *int     `json:"dn,omitempty"`
	LeapMismatch *bool    `json:"leap_mismatch,omitempty"`
	// Osnma (regression fix, Galileo E1-B entries only — docs/OUTPUT.md §1.1): true =
	// the SV's 40-bit I/NAV OSNMA field has carried live (nonzero) data within
	// the last osnmaLiveWindow; false = the field is observed but all-zeros
	// (the SV is outside the OSNMA-distributing subset, GAL-OSNMA-SIS-ICD §2);
	// absent = no nominal page observed / not Galileo. Presence only
	// (INTEGRITY.md §7 v1) — true does NOT mean the data authenticated.
	Osnma          *bool    `json:"osnma,omitempty"`
	IOD            *int     `json:"iod,omitempty"`
	OrbitDiscoM    *float64 `json:"orbit_disco_m,omitempty"`
	OrbitDiscoAgeS *float64 `json:"orbit_disco_age_s,omitempty"`
	TimeDiscoNs    *float64 `json:"time_disco_ns,omitempty"`
	AODC           *int     `json:"aodc,omitempty"`
	AODE           *int     `json:"aode,omitempty"`
	Af0            *float64 `json:"af0,omitempty"`
	Af1            *float64 `json:"af1,omitempty"`
	Af2            *float64 `json:"af2,omitempty"`
	// FreqCh (regression fix, GLONASS-only) is the FDMA frequency channel k ∈ [−7, +6]
	// the tracked signal was received on (receiver freqId − 7, validated at the
	// boundary per regression fix). docs/CONSTELLATIONS.md §5 calls the channel "the"
	// identifier for FDMA satellites; serving it lets consumers cross-check a
	// tracked signal's channel against the broadcast almanac's HnA-derived
	// channel for the slot (docs/OUTPUT.md §1.4 freq_ch) — a mismatch means
	// mis-identification or spoofing (the DEFENSE-PNT cross-check family).
	FreqCh    *int     `json:"freq_ch,omitempty"`
	XM        *float64 `json:"x_m,omitempty"`
	YM        *float64 `json:"y_m,omitempty"`
	ZM        *float64 `json:"z_m,omitempty"`
	// PosAtUnixNs (regression fix, detector-facing — not part of the JSON contract) is
	// the exact propagation epoch (svState.posAt) that produced x_m/y_m/z_m.
	// The cross-signal agreement check must compare two signals' positions ONLY
	// when both were propagated at the identical instant: Propagate uses one
	// `now` per tick, but an entry whose propagation failed a tick retains its
	// previous epoch, and an SV moves ~3–4 km/s — a sub-second epoch skew reads
	// as kilometres of fake divergence. The served integer tow cannot make that
	// distinction under a sub-second propagate_interval; this can. Zero = no
	// fresh position.
	PosAtUnixNs int64 `json:"-"`
	Tow         *int  `json:"tow,omitempty"`
	Wn          *int  `json:"wn,omitempty"`
	LastSeenS int      `json:"last_seen_s"`
	// Conf (regression fix, docs/OUTPUT.md §1.1 / INTEGRITY §6) is the corroboration
	// count: distinct sources that delivered a structurally-decoded nav frame
	// for THIS satellite×signal within the fresh-receiver window. Always
	// present — 0 is a known value ("no current nav corroboration", e.g. an
	// iono-only RAWX entry), not an unknown. Until the §6 broadcast-agreement
	// divergence detector lands (P7), this is the served honesty floor:
	// consumers can tell a fleet-corroborated state (conf ≥ 2) from a
	// single receiver's testimony (conf 1).
	Conf int `json:"conf"`

	Perrecv map[string]*FeedPerRecv `json:"perrecv,omitempty"`
}

// FeedPerRecv is one observer's per-SV reception entry (docs/OUTPUT.md §1.1
// perrecv). This build carries the measured-iono fields (docs/MATH.md §7.4);
// look angles, C/N0, and delta-Hz join it with the observer-geometry pass.
type FeedPerRecv struct {
	IonoDelayM    *float64 `json:"iono_delay_m,omitempty"`
	IonoPairSigID *int     `json:"iono_pair_sigid,omitempty"`
	IonoCal       int      `json:"iono_cal"` // 0 = uncalibrated (receiver DCB not separated)
}

// GlobalFeed is the system-wide counters view (docs/OUTPUT.md §1.2). Per-
// constellation SV/signal counts plus live totals and the leap-second count. The
// broadcast time-system offsets (gps_utc_offset_ns, …) are transcribed values, not
// computed by us; they are populated once the UTC-parameter decode lands, so they
// are omitted here rather than emitted as a guessed zero.
type GlobalFeed struct {
	LastSeen           int64          `json:"last_seen"`
	LeapSeconds        int            `json:"leap_seconds"`
	TotalLiveSVs       int            `json:"total_live_svs"`
	TotalLiveSignals   int            `json:"total_live_signals"`
	TotalLiveReceivers int            `json:"total_live_receivers"`
	Counts             map[string]int `json:"-"` // flattened into <c>_svs / <c>_sigs by the marshaller
}

// AlmanacEntry is one coarse-orbit entry (docs/OUTPUT.md §1.4). This build fills it
// from the precise broadcast ephemeris for every currently-observed SV (eph_source
// 0, observed true); almanac-only SVs appear once the almanac-subframe decode and
// the TLE fill land.
type AlmanacEntry struct {
	Name           string  `json:"name"`
	GnssID         int     `json:"gnssid"`
	Observed       bool    `json:"observed"`
	EcefXM         float64 `json:"ecef_x_m"`
	EcefYM         float64 `json:"ecef_y_m"`
	EcefZM         float64 `json:"ecef_z_m"`
	LatDeg         float64 `json:"lat_deg"`
	LonDeg         float64 `json:"lon_deg"`
	InclinationRad float64 `json:"inclination_rad"`
	T0e            int     `json:"t0e"`
	T              int     `json:"t"`
	EphSource      int     `json:"eph_source"`

	// GLONASS-only ascending-node longitude and its epoch (docs/OUTPUT.md §1.4),
	// absent for other constellations.
	LambdaNA  *float64 `json:"lambda_na,omitempty"`
	TLambdaNA *float64 `json:"t_lambda_na,omitempty"`

	// Operable (regression fix, GLONASS-only) is the almanac CnA ground-segment health
	// flag, re-normalized so true = operable. The broadcast polarity is INVERTED
	// vs Bn/ℓn: Cn = 0 indicates malfunction, Cn = 1 operable (GLO-ICD-5.1
	// §5.3), and ICD Table 5.1 requires Cn analyzed jointly with Bn(ℓn) for the
	// usability decision. For a slot out of ephemeris view the almanac is its
	// only presence in the feeds and Cn its only broadcast health indicator —
	// before this field, a slot ground control had already flagged inoperable
	// (the flag reaches every SV's almanac within ~16 h, §5.3) served as a
	// normal coarse-orbit entry, indistinguishable from a healthy one. An
	// inoperable slot's entry is deliberately still served: consumers want the
	// position of an unhealthy SV; it is the flag that was missing.
	Operable *bool `json:"operable,omitempty"`

	// FreqCh (regression fix, GLONASS-only): the slot's FDMA channel k from the
	// broadcast almanac word HnA (Table 4.10 mapping, validated per regression fix) —
	// the almanac side of the eph-vs-almanac channel cross-check (see
	// FeedSV.FreqCh).
	FreqCh *int `json:"freq_ch,omitempty"`
}

// SBASEntry is one augmentation-system health entry (docs/OUTPUT.md §1.5).
type SBASEntry struct {
	Provider   string `json:"provider"`
	HealthCode int    `json:"health_code"`
	LastSeen   int64  `json:"last_seen"`
	LastSeenS  int    `json:"last_seen_s"`
	LastType   int    `json:"last_type"`
	LastType0  *int64 `json:"last_type_0,omitempty"`
	LastType0S *int   `json:"last_type_0_s,omitempty"`
}

// FeedSVs builds the svs feed as of now (docs/OUTPUT.md §1.1).
func (s *Store) FeedSVs(now time.Time) map[string]FeedSV {
	out := make(map[string]FeedSV)
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, st := range sh.m {
			if !st.haveEph && !st.haveGloEph && len(st.ionoBySource) == 0 {
				continue // neither ephemeris nor observables yet: nothing to publish
			}
			out[st.key.Name()] = st.feedSV(now)
		}
		sh.mu.Unlock()
	}
	return out
}

// freshReceiverWindow bounds how recently a source must have delivered a decoded
// nav frame for its corroboration vote to count toward conf. Mirrors
// detect.FreshReceiverThreshold ("a receiver's vote only counts if it saw the SV
// this recently", docs/INTEGRITY.md §2) — duplicated as a local constant because
// detect depends on state, not the reverse (the liveReceiverWindow precedent);
// keep the two in sync if the operating point moves.
const freshReceiverWindow = 60 * time.Second

// feedSV projects one svState to a FeedSV. The caller holds the shard lock.
func (st *svState) feedSV(now time.Time) FeedSV {
	g := st.key.G
	// health_code 0 ("unknown") until this SV's own health bits have
	// actually been decoded -- an iono-only SV, or a Galileo SV whose
	// word 5 hasn't arrived yet, otherwise served a false "OK" from st.health's
	// zero value.
	var code, level int
	if st.haveHealth {
		code, level = healthFor(g, st.key.Sig, st.health)
	}
	e := FeedSV{
		FullName:         fullName(g, st.key.Sv, st.key.Sig),
		Name:             fmt.Sprintf("%c%02d", g.Letter(), st.key.Sv),
		GnssID:           int(g),
		SvID:             st.key.Sv,
		SigID:            st.key.Sig,
		HealthCode:       code,
		HealthIssueLevel: level,
		HealthSubcode:    st.health,
		LastSeenS:        int(now.Sub(st.lastSeen).Seconds()),
	}
	// count the sources corroborating this entry within the fresh
	// window; prune the rest so the map stays bounded by the CURRENT fleet (a
	// renamed source ages out here rather than lingering). Mutating under the
	// shard lock the caller holds.
	for src, at := range st.seenBy {
		if now.Sub(at) <= freshReceiverWindow {
			e.Conf++
		} else {
			delete(st.seenBy, src)
		}
	}
	// posFresh gates both the position and (for Kepler-family below) its
	// paired tow/wn on the same stored propagation epoch, so the two can never
	// disagree the way "position from the last tick, tow recomputed at feed-build
	// time" did -- and a position frozen by a repeatedly-failing propagate tick
	// (havePos stays true; posAt stops advancing) is dropped once it's stale,
	// rather than served forever alongside an ever-fresher-looking tow.
	posFresh := st.havePos && finiteECEF(st.pos) && !st.posAt.IsZero() && now.Sub(st.posAt) <= posStaleBound
	if posFresh {
		x, y, z := st.pos.X, st.pos.Y, st.pos.Z
		e.XM, e.YM, e.ZM = &x, &y, &z
		e.PosAtUnixNs = st.posAt.UnixNano() // exact epoch for the cross-signal compare
	}
	if st.orbitDiscoValid && finite(st.orbitDisco) {
		v := st.orbitDisco
		e.OrbitDiscoM = &v
		age := now.Sub(st.discoAt).Seconds()
		e.OrbitDiscoAgeS = &age
	}
	if st.timeDiscoValid && finite(st.timeDiscoNs) {
		v := st.timeDiscoNs
		e.TimeDiscoNs = &v
	}
	if st.accKind != accNone {
		idx := st.accIdx
		e.AccIndex = &idx // serve the raw index even when it maps to no metres value
		e.AccIndexRawOnly = st.accKind == accSISAIRaw
	}
	if st.haveAlert {
		a := st.alert
		e.Alert = &a
	}
	if st.haveWN {
		w := st.wnMismatch
		e.WnMismatch = &w
	}
	if m, ok := sisaFor(st.accKind, st.accIdx); ok && finite(m) {
		e.SISAValid, e.SISAM = true, &m
	}
	if st.haveAOD {
		aodc, aode := st.aodc, st.aode
		e.AODC, e.AODE = &aodc, &aode
	}
	if st.haveOSNMA {
		on := !st.osnmaLastLive.IsZero() && now.Sub(st.osnmaLastLive) <= osnmaLiveWindow
		e.Osnma = &on
	}
	if st.ggto != nil {
		a0, a1 := st.ggto.a0g, st.ggto.a1g
		t0, w0 := int(st.ggto.t0g), st.ggto.wn0g
		e.A0G, e.A1G, e.T0G, e.WN0G = &a0, &a1, &t0, &w0
		// Evaluate Eq. 24 at the feed instant on the continuous GPS-seconds
		// axis: dt = t_now − t_ref is axis-invariant, and the ±31-week §5.1.8
		// broadcast bound makes the mod-64 nearest-cycle disambiguation of the
		// 6-bit WN0G exact. Worst case |dt| ≈ 31 weeks with A1G
		// bounded by its 12-bit ×2⁻⁵¹ coding keeps the rate term ≤ ~17 µs —
		// no wrap handling needed beyond the week disambiguation.
		ref := gnsstime.GNSSTime{
			Sys:  gnsstime.SysGalileo,
			Week: gnsstime.DisambiguateWeek(gnsstime.SysGalileo, st.ggto.wn0g, 6, float64(now.Unix()), float64(gpsUTCOffset)),
			TOW:  st.ggto.t0g,
		}
		if refGPS, ok := ref.GPSSeconds(); ok {
			// the wall-clock side of dt reduces through the same
			// epoch table as the reference (SysGPS offset is 0 by definition).
			gpsNow, _ := gnsstime.SystemSeconds(gnsstime.SysGPS, float64(now.Unix()), float64(gpsUTCOffset))
			dt := gpsNow - refGPS
			if off := (a0 + a1*dt) * 1e9; finite(off) {
				e.GpsOffsetNs = &off
			}
		}
	}
	if st.haveBdsFlags {
		dif, sif, aif, sismai := st.bdsDIF, st.bdsSIF, st.bdsAIF, st.bdsSISMAI
		e.Dif, e.Sif, e.Aif, e.Sismai = &dif, &sif, &aif, &sismai
	}
	if st.haveBdsKlob {
		a, b := st.bdsKlobAlpha, st.bdsKlobBeta
		e.KlobAlpha, e.KlobBeta = &a, &b
	}
	if st.haveBDGIM {
		g := st.bdgim
		e.Bdgim = &g
	}
	if st.bdtUTC != nil {
		u := st.bdtUTC
		// evaluate Eq. 7-25 (BDS-SIS-ICD-B2a v1.0 §7.12.2) at the feed
		// instant on the continuous BDT axis. BDT = GPST − 14 s exactly (both
		// leap-free scales; BDT epoch 2006-01-01 is 1356 GPS weeks after the
		// GPS epoch), so BDT seconds since the BDT epoch come from gnsstime's
		// SystemSeconds — the same audited epoch constant weekFor/towFor now
		// reduce through, not a third local (gps−14)−1356·604800
		// fold. WNot is the full 13-bit BDT week (no truncation to
		// disambiguate, unlike the GGTO's 6-bit WN0G). The reference
		// (WNot, tot) makes dt exact across week boundaries — no half-week
		// wrap involved (the regression fix deviation does not apply here).
		bdt, _ := gnsstime.SystemSeconds(gnsstime.SysBeiDou, float64(now.Unix()), float64(gpsUTCOffset)) // SysBeiDou cannot fail
		dt := bdt - (float64(u.WNot)*gnsstime.WeekSeconds + u.Tot)
		// Leap-arm choice (§7.12.2 cases 1/3, mirroring B1I §5.2.4.18 and
		// IS-GPS-200 §20.3.3.5.2.4): ΔtLS before the WNLSF/DN event, ΔtLSF
		// after. The event instant is the END of day DN of week WNLSF — DN is
		// 0–6 (Table 7-20, ruling out GPS's 1-based 1–7 reading), so day DN
		// spans [DN·86400, (DN+1)·86400) of the week; B2a §7.12.2 defines the
		// case-2 accommodation span as "six hours prior to the leap second
		// time ... six hours after", whose upper edge B1I prints as "DN+5/4"
		// (days) — together fixing the event at (DN+1)·86400. (B1I's printed
		// LOWER edge is the asymmetric "DN+2/3", where IS-GPS-200 has DN+3/4;
		// an apparent ICD typo, and either way it does not move the event
		// instant this boundary hangs on.) Within ±6 h of a real leap the
		// served value follows this instant rather than the case-2 day-wrap
		// presentation, which affects tUTC's modulo form, not the offset
		// magnitude served here.
		leap := u.DtLS
		if bdt >= float64(u.WNLSF)*weekSeconds+float64(u.DN+1)*86400 {
			leap = u.DtLSF
		}
		if off := (leap + u.A0 + u.A1*dt + u.A2*dt*dt) * 1e9; finite(off) {
			e.UtcOffsetNs = &off
		}
		dtLS, dtLSF, wnlsf, dn := int(u.DtLS), int(u.DtLSF), u.WNLSF, u.DN
		e.DtLS, e.DtLSF, e.WnLSF, e.Dn = &dtLS, &dtLSF, &wnlsf, &dn
		// regression fix cross-check: the broadcast BDT-UTC leap count and this
		// collector's configured GPS−UTC count describe the same physical leap
		// history, offset by the fixed 14 s BDT-GPST alignment (BDT epoch
		// 2006-01-01, when GPS−UTC was 14 s) — so broadcast + 14 must equal
		// gpsUTCOffset. Compare the currently-applicable arm so the check
		// doesn't false-fire in the window where ΔtLS is already the past
		// count of a superseded event.
		mm := int64(leap)+14 != gpsUTCOffset
		e.LeapMismatch = &mm
	}
	for src, tr := range st.ionoBySource {
		// a receiver may carry several matured secondary arcs (e.g. both
		// L2C and L5); the feed still serves one iono_pair_sigid per receiver, so
		// pick whichever secondary paired most recently.
		var best *secTrack
		var bestSig int
		for sigID, secT := range tr.secs {
			if !secT.hasDelay || !finite(secT.delayM) || secT.delayAt.IsZero() || now.Sub(secT.delayAt) > ionoDelayTTL {
				continue
			}
			// Ties happen in the realistic shape: two secondaries maturing in the
			// same RAWX epoch share both delayAt (one Recv per RXM-RAWX message)
			// and lastEpochS (both equal the primary's epoch), so break the last
			// tie on sigID (lower = more primary) to keep the pick deterministic.
			if best == nil || secT.delayAt.After(best.delayAt) ||
				(secT.delayAt.Equal(best.delayAt) && (secT.lastEpochS > best.lastEpochS ||
					(secT.lastEpochS == best.lastEpochS && sigID < bestSig))) {
				best, bestSig = secT, sigID
			}
		}
		if best == nil {
			continue
		}
		if e.Perrecv == nil {
			e.Perrecv = map[string]*FeedPerRecv{}
		}
		d, ps := best.delayM, bestSig
		e.Perrecv[src] = &FeedPerRecv{IonoDelayM: &d, IonoPairSigID: &ps}
	}

	if g == gnss.GLONASS {
		if st.haveGloEph {
			k := st.gloEph.FreqCh // boundary-validated, k = freqId − 7
			e.FreqCh = &k
			// Inside the serving cap the ICD-defined day-wrapped age is served
			// unchanged; it is legitimately NEGATIVE for roughly the first half of
			// each tb interval, because the immediate data are referred to the
			// MIDDLE of the interval (GLO-ICD-5.1 §4.4) — documented in
			// docs/OUTPUT.md §1.1, not a bug.
			age := gnsstime.EphAgeDay(gloTOD(now), st.gloEph.Tb) / 60.0
			// regression fix (the regression fix discipline at GLONASS's ±12 h horizon): the
			// EphAgeDay wrap saturates at +720 min and then goes negative, so a
			// worsening SV's served age would lie and flip the eph_aged detector
			// back to "fresh". Past the serving cap switch to the wall-clock age
			// since apply — a lower bound on the true broadcast age, monotone,
			// cannot wrap — so eph_aged latches correctly with no further change.
			if wall := now.Sub(st.gloEphAt); !st.gloEphAt.IsZero() && wall > gloPropagateMaxEphAge {
				age = wall.Minutes()
			} else if age < gloServeMinTk.Minutes() {
				// regression fix follow-up, frozen-tb regime: reception is live (the wall
				// switch above did not fire) but the day-wrapped age has left the
				// legitimate window — past +12 h it re-wraps NEGATIVE, which would
				// un-fire eph_aged on a worsening SV. The true broadcast age is
				// unknowable here without extra state, but it is ≥ half a day, so
				// clamp to the wrap ceiling (+720 min) — monotone enough to keep
				// eph_aged latched, and honest as a lower bound.
				age = 720
			}
			if finite(age) {
				e.EphAgeM = &age
			}
		}
		return e
	}

	// Kepler-family: clock polynomial, time-of-week/week, ephemeris age. an
	// SV present only via RAWX observables (st.haveEph == false) has no clock/toe
	// to report -- af0/af1/af2 = 0 and eph_age_m computed against Toe = 0 are
	// fabricated sentinels, not measurements, and the half-week-wrapped age against
	// a zero Toe can debounce-confirm a false eph_aged event. Gate the whole block
	// on haveEph; the entry itself (and its per-receiver iono, above) is still
	// published either way.
	if st.haveEph {
		// iod is a decoded issue-of-data; for an eph-less SV (iono-only RAWX) a
		// served iod:0 is a fabricated value indistinguishable from a genuine IODE 0, so
		// gate it on haveEph like the clock fields ("absent = unknown"). GLONASS (haveGloEph,
		// no issue-of-data) correctly never reaches this block.
		iod := st.iod
		e.IOD = &iod
		// regression fix (regression fix/regression fix discipline for the clock): the CNAV and B-CNAV2
		// families can assemble an ephemeris before any clock message has decoded
		// — st.clk's zero af0/af1/af2 would serve as fabricated sentinels without
		// the haveClk gate ("absent = unknown", docs/OUTPUT.md §1.1).
		if st.haveClk && finite(st.clk.Af0) && finite(st.clk.Af1) && finite(st.clk.Af2) {
			af0, af1, af2 := st.clk.Af0, st.clk.Af1, st.clk.Af2
			e.Af0, e.Af1, e.Af2 = &af0, &af1, &af2
		}
		// tow/wn are "time-of-week of the solution" (docs/OUTPUT.md §1.1) --
		// the solution is x_m/y_m/z_m, so tow/wn must be the epoch that produced
		// that position (st.posAt), not "now" at feed-build time. Gated on posFresh
		// (not just haveEph) so tow/wn are never served without the position they
		// describe.
		if posFresh {
			tow := int(towFor(g, st.posAt))
			e.Tow = &tow
			if wn, ok := weekFor(g, st.posAt); ok {
				e.Wn = &wn
			}
		}
		age := gnsstime.EphAgeMinutes(towFor(g, now), st.eph.Toe)
		// the SOW-based age wraps to ±half-week, so past the serving cap a
		// week-stale ephemeris would read near-zero here and the eph_aged detector
		// would consume that lie even though Propagate has stopped serving its
		// position. Beyond propagateMaxEphAge switch to the wall-clock age since
		// apply (a lower bound on the true broadcast age — it omits the sub-fit-
		// interval toe→apply offset), which is monotonic, cannot wrap, and keeps
		// eph_aged latched. Inside the cap the ICD-defined SOW age (which can be
		// legitimately negative before toe) is served unchanged, per the §1.1
		// contract.
		if wall := now.Sub(st.ephAt); !st.ephAt.IsZero() && wall > propagateMaxEphAge {
			age = wall.Minutes()
		}
		if finite(age) {
			e.EphAgeM = &age
		}
	}
	return e
}

// FeedGlobal builds the global counters feed as of now (docs/OUTPUT.md §1.2).
func (s *Store) FeedGlobal(now time.Time) GlobalFeed {
	g := GlobalFeed{LeapSeconds: int(gpsUTCOffset), Counts: map[string]int{}}
	svs := map[gnss.GNSSID]map[int]bool{} // distinct SVIDs per constellation
	var last time.Time
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, st := range sh.m {
			if !st.haveEph && !st.haveGloEph {
				continue
			}
			g.TotalLiveSignals++
			g.Counts[st.key.G.String()+"_sigs"]++
			if svs[st.key.G] == nil {
				svs[st.key.G] = map[int]bool{}
			}
			svs[st.key.G][st.key.Sv] = true
			if st.lastSeen.After(last) {
				last = st.lastSeen
			}
		}
		sh.mu.Unlock()
	}
	for id, set := range svs {
		g.Counts[id.String()+"_svs"] = len(set)
		g.TotalLiveSVs += len(set)
	}
	// SBAS nav state lives in s.sbas (never the shards), so its counts were
	// structurally absent even with live WAAS/EGNOS reception. Count fresh PRNs — one SV and
	// one signal each. (total_live_* stays shard-derived; the finding scopes this to the
	// per-constellation counts, not the live totals.)
	s.sbasMu.Lock()
	sbasN := 0
	for _, st := range s.sbas {
		if now.Sub(st.lastSeen) <= sbasStaleAfter {
			sbasN++
			if st.lastSeen.After(last) {
				last = st.lastSeen
			}
		}
	}
	s.sbasMu.Unlock()
	g.Counts[gnss.SBAS.String()+"_svs"] = sbasN
	g.Counts[gnss.SBAS.String()+"_sigs"] = sbasN
	// zero-fill every constellation pair so a count of 0 is an explicit value, not an
	// absent-vs-0 ambiguity for a count (IMES=4 is never emitted, docs/CONSTELLATIONS §0).
	for _, id := range []gnss.GNSSID{gnss.GPS, gnss.SBAS, gnss.Galileo, gnss.BeiDou, gnss.QZSS, gnss.GLONASS, gnss.NavIC} {
		if _, ok := g.Counts[id.String()+"_svs"]; !ok {
			g.Counts[id.String()+"_svs"] = 0
		}
		if _, ok := g.Counts[id.String()+"_sigs"]; !ok {
			g.Counts[id.String()+"_sigs"] = 0
		}
	}
	if !last.IsZero() {
		g.LastSeen = last.Unix()
	}
	g.TotalLiveReceivers = s.countLiveReceivers(now)
	return g
}

// liveReceiverWindow bounds how recently a station must have produced a nav frame
// or RF-telemetry sample to count toward total_live_receivers. Mirrors
// detect.ObserverOfflineThreshold ("an observer unseen this long is offline",
// docs/INTEGRITY.md §2) — duplicated as a local constant rather than imported
// because detect depends on state, not the reverse; keep the two in sync if the
// operating point moves.
const liveReceiverWindow = 300 * time.Second

// LiveReceivers is the exported live-station count for the detector's
// fleet-footprint gate (regression fix, detect.SilenceMinReceivers): the same number
// FeedGlobal serves as total_live_receivers, so the events channel and the
// global feed can never disagree about the fleet size a suppression decision
// was based on.
func (s *Store) LiveReceivers(now time.Time) int { return s.countLiveReceivers(now) }

// StationLastSeen is the station_offline classifier's read model (regression fix, the
// regression fix wiring): seconds since each known station last produced a decoded nav
// frame or RF-telemetry sample — the same caps ∪ rf union countLiveReceivers
// uses, but per-station and UNFILTERED, because the offline detector must keep
// observing a station after it goes dark (the filtered FeedStationRF drops a
// station at rfStaleAfter, which is why station_offline could never fire from
// it). The capability map is never evicted (durability is its design), so a
// long-dead station keeps its climbing age and the machine can hold "offline"
// indefinitely rather than freezing mid-state when the rf entry is evicted
//.
func (s *Store) StationLastSeen(now time.Time) map[string]int {
	last := map[string]time.Time{}
	s.capMu.Lock()
	for id, st := range s.caps {
		if t, ok := last[id]; !ok || st.lastSeen.After(t) {
			last[id] = st.lastSeen
		}
	}
	s.capMu.Unlock()
	s.rfMu.Lock()
	for id, st := range s.rf {
		if t, ok := last[id]; !ok || st.lastSeen.After(t) {
			last[id] = st.lastSeen
		}
	}
	s.rfMu.Unlock()
	out := make(map[string]int, len(last))
	for id, t := range last {
		out[id] = int(now.Sub(t).Seconds())
	}
	return out
}

// countLiveReceivers counts distinct stations seen (via either a decoded nav
// frame or RF telemetry) within liveReceiverWindow of now — the union of
// s.caps and s.rf, since a station can report one, the other, or both.
func (s *Store) countLiveReceivers(now time.Time) int {
	live := map[string]bool{}
	s.capMu.Lock()
	for id, st := range s.caps {
		if now.Sub(st.lastSeen) <= liveReceiverWindow {
			live[id] = true
		}
	}
	s.capMu.Unlock()
	s.rfMu.Lock()
	for id, st := range s.rf {
		if now.Sub(st.lastSeen) <= liveReceiverWindow {
			live[id] = true
		}
	}
	s.rfMu.Unlock()
	return len(live)
}

// FeedAlmanac builds the almanac feed as of now (docs/OUTPUT.md §1.4), one coarse
// entry per currently-observed SV derived from its precise broadcast ephemeris.
func (s *Store) FeedAlmanac(now time.Time) map[string]AlmanacEntry {
	out := make(map[string]AlmanacEntry)
	// bestSig tracks the winning signal's SigID per SV name : one entry per
	// SV, and the pick must be deterministic (lowest SigID — the primary signal)
	// rather than dependent on Go's randomized map/shard iteration order, so
	// replays/backfills reproduce the same almanac.
	bestSig := map[string]int{}
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, st := range sh.m {
			if !st.havePos || !finiteECEF(st.pos) || st.posAt.IsZero() || now.Sub(st.posAt) > posStaleBound {
				continue
			}
			name := fmt.Sprintf("%c%02d", st.key.G.Letter(), st.key.Sv)
			if prev, seen := bestSig[name]; seen && st.key.Sig >= prev {
				continue // a lower-SigID (more primary) signal already won this SV
			}
			bestSig[name] = st.key.Sig
			ent := AlmanacEntry{
				Name:      name,
				GnssID:    int(st.key.G),
				Observed:  true,
				EcefXM:    st.pos.X,
				EcefYM:    st.pos.Y,
				EcefZM:    st.pos.Z,
				T:         int(st.posAt.Unix()),
				EphSource: 0,
			}
			ell := physconst.WGS84
			if p, ok := physconst.For(st.key.G); ok {
				ell = p.Datum
			}
			gd := geo.ECEFToGeodetic(st.pos, ell)
			ent.LatDeg, ent.LonDeg = geo.Deg(gd.Lat), geo.Deg(gd.Lon)
			if st.haveEph {
				ent.InclinationRad = st.eph.I0
				ent.T0e = int(st.eph.Toe)
			}
			out[name] = ent
		}
		sh.mu.Unlock()
	}
	s.addGlonassAlmanac(out, now)
	return out
}

// gloMeanInclination is the GLONASS mean orbital inclination (63°, ICD Ed. 5.1
// §A.3.2.1); the almanac broadcasts Δi as a correction to it.
var gloMeanInclination = 63.0 * physconst.Pi / 180.0

// addGlonassAlmanac adds an almanac entry for every GLONASS slot that is not already
// observed (out-of-view SVs the ephemeris store cannot carry). Each is propagated to now
// with the analytic almanac propagator (docs/MATH.md §3.1). the propagation TARGET
// day is the actual current MT calendar day (gloNTDay), not the broadcast NA — NA is only
// the day each almanac's elements are referenced to (stored per-entry in Alm.NA), and it
// lags the calendar, so propagating at NA would evaluate every out-of-view SV's position a
// day (or more) in the past, off by tens of thousands of km along-track.
func (s *Store) addGlonassAlmanac(out map[string]AlmanacEntry, now time.Time) {
	s.gloAlmMu.Lock()
	na := s.gloNA
	alms := make([]frame.GLONASSAlmanacEntry, 0, len(s.gloAlmanac))
	for _, slot := range s.gloAlmanac {
		// a slot the constellation's ground control has actually retired
		// stops being rebroadcast by any satellite within a few ~2.5h cycles; without
		// this cutoff a decommissioned slot's last-ever almanac would be propagated
		// out to an ever-more-speculative "ghost" position at the current day number
		// forever. (Feed filter; the RAM entry is evicted by ExpireStations, regression fix.)
		if now.Sub(slot.lastSeen) > gloAlmanacStaleAfter {
			continue
		}
		alms = append(alms, slot.entry)
	}
	s.gloAlmMu.Unlock()
	if na == 0 {
		return // no day-number anchor yet; the almanac time base is unknown
	}

	ti := gloTOD(now)
	n0 := gloNTDay(now) // actual current MT day; NA (per-entry Alm.NA) is only the reference day
	ell := physconst.WGS84
	if p, ok := physconst.For(gnss.GLONASS); ok {
		ell = p.Datum
	}
	for _, a := range alms {
		name := fmt.Sprintf("R%02d", a.Alm.Slot)
		// surface the CnA ground-segment health flag (see the Operable
		// field doc — broadcast polarity inverted, Cn=1 operable) on every GLONASS
		// entry the almanac covers, observed ones included: Cn is ground-segment
		// truth about the slot regardless of local visibility.
		operable := a.Cn == 1
		freqCh := a.Alm.FreqCh // HnA-derived, validated per regression fix
		if ent, seen := out[name]; seen {
			// the observed entry keeps its precise ECEF/epoch, but its
			// always-present inclination/t0e metadata comes from the fresh
			// broadcast almanac instead of fabricated zeros. lambda_na remains
			// intentionally absent on observed entries.
			if ent.Observed {
				ent.InclinationRad = gloMeanInclination + a.Alm.DeltaI
				ent.T0e = int(a.Alm.Tlambda)
				ent.Operable = &operable
				ent.FreqCh = &freqCh
				out[name] = ent
			}
			continue // observed → its precise broadcast-ephemeris entry wins
		}
		pos, err := glonass.PropagateAlmanacECEF(a.Alm, n0, ti)
		if err != nil || !finiteECEF(pos) {
			continue
		}
		gd := geo.ECEFToGeodetic(pos, ell)
		lambda, tLambda := a.Alm.Lambda, a.Alm.Tlambda
		out[name] = AlmanacEntry{
			Name:           name,
			GnssID:         int(gnss.GLONASS),
			Observed:       false,
			EcefXM:         pos.X,
			EcefYM:         pos.Y,
			EcefZM:         pos.Z,
			LatDeg:         geo.Deg(gd.Lat),
			LonDeg:         geo.Deg(gd.Lon),
			InclinationRad: gloMeanInclination + a.Alm.DeltaI,
			T0e:            int(a.Alm.Tlambda),
			T:              int(now.Unix()),
			EphSource:      0,
			LambdaNA:       &lambda,
			TLambdaNA:      &tLambda,
			Operable:       &operable,
			FreqCh:         &freqCh,
		}
	}
}

// sbasStaleAfter bounds how long an SBAS PRN may go unseen before it is dropped from the
// feed, mirroring rfStaleAfter: a live SBAS GEO broadcasts continuously (message
// type 1 every few seconds), so this is generous margin over normal operation while still
// catching a PRN whose station has gone dark or been decommissioned rather than serving
// its last-known health with an ever-growing last_seen_s forever. This is the FEED
// filter; the RAM entry itself is evicted later by ExpireStations.
const sbasStaleAfter = rfStaleAfter

// sbasType0Hold is how long one received MT0 ("do not use for safety
// applications") keeps the served health_code latched at 3. MT0 is a
// condition, not a message-by-message state: a system under test interleaves
// MT0 with its normal correction stream — since 2003 typically as "MT0/2", an
// MT2 body broadcast in the MT0 frame slot (EGNOS-SDD-OS §4.1 WARNING) — and a
// DO-229 receiver excludes the GEO on any MT0 sighting rather than re-admitting
// it one message later. Deriving health from the *last* message therefore read
// "OK" for almost every feed build on a test-mode GEO (the exact wrong answer
// this feed exists to report) and, sampled every 15 s under a 60 s debounce,
// made the critical sbas_health event structurally unconfirmable.
//
// 60 s is the DO-229-family MT0 exclusion interval as stated by the vendored
// QZSS-L1S §4.1.2.3: "When receiving MT 0, the receiver should delete all SLAS
// messages of the QZS in the past, and SLAS messages of the QZS for the
// subsequent 60 seconds must also not be used." RTCA DO-229 itself is paywalled
// and not vendored (reference/REFERENCES.md "RTCA-DO-229"); re-verify this
// constant against DO-229's own MT0 rule if that reference is ever acquired.
// In steady test mode MT0 recurs well inside 60 s, so the latch holds
// continuously; a single isolated MT0 ages out after its 60 s exclusion.
const sbasType0Hold = 60 * time.Second

// FeedSBAS builds the sbas augmentation-health feed as of now (docs/OUTPUT.md §1.5).
func (s *Store) FeedSBAS(now time.Time) map[string]SBASEntry {
	return s.buildSBAS(now, false)
}

// SBASDetect is the DETECTOR's view of the SBAS store : identical
// entries to FeedSBAS, but with the sbasStaleAfter filter off, so a PRN that
// has gone dark stays observable (with a climbing last_seen_s) until RAM
// eviction (ExpireStations, regression fix) instead of silently vanishing. The silence
// classifier must run over THIS view: classifying from the already-filtered
// FeedSBAS meant a dark GEO simply disappeared from the map, its sbas_health
// machine froze at the last confirmed state, and no event of any type fired —
// the one constellation whose silence is unambiguous (geostationary, ~1 Hz
// broadcast, no rise/set) was the one without a silence event.
func (s *Store) SBASDetect(now time.Time) map[string]SBASEntry {
	return s.buildSBAS(now, true)
}

// buildSBAS projects the per-PRN SBAS state to entries; includeStale keeps
// entries past sbasStaleAfter (the detector view) instead of dropping them
// (the served feed).
func (s *Store) buildSBAS(now time.Time, includeStale bool) map[string]SBASEntry {
	out := make(map[string]SBASEntry)
	s.sbasMu.Lock()
	defer s.sbasMu.Unlock()
	for prn, st := range s.sbas {
		if !includeStale && now.Sub(st.lastSeen) > sbasStaleAfter {
			continue
		}
		code := 1 // OK
		if st.haveType0 && now.Sub(st.lastType0) < sbasType0Hold {
			code = 3 // do-not-use, latched on MT0 recency 
		}
		ent := SBASEntry{
			Provider:   st.provider,
			HealthCode: code,
			LastSeen:   st.lastSeen.Unix(),
			LastSeenS:  int(now.Sub(st.lastSeen).Seconds()),
			LastType:   st.lastType,
		}
		if st.haveType0 {
			t0 := st.lastType0.Unix()
			t0s := int(now.Sub(st.lastType0).Seconds())
			ent.LastType0, ent.LastType0S = &t0, &t0s
		}
		out[fmt.Sprintf("%d", prn)] = ent
	}
	return out
}

// gloBnMalfunctionBit is the GLONASS Bn health word's MSB (bit 2 of the 3-bit
// field): 1 = malfunctioning, 0 = operable (GLONASS ICD Ed. 5.1). The two
// low-order bits carry other status, not overall SV health.
const gloBnMalfunctionBit = 0x4

// gloLnShift/gloLnMalfunctionBit : the GLONASS health_subcode is OUR
// packed encoding Bn | ℓn<<3 — the raw 3-bit Bn word in the low bits plus the
// GLONASS-M ℓn fast malfunction flag (GLO-ICD-5.1 §4.4; ≤10 s latency vs Bn's
// ≤1 min, §5.3 note) one bit above it, so the served subcode preserves both
// flags and a Bn-vs-ℓn disagreement window is visible to consumers
// (docs/OUTPUT.md §2.2). Either malfunction bit gates usability: ICD Table 5.1
// defines operability over Bn(ℓn) jointly.
const (
	gloLnShift          = 3
	gloLnMalfunctionBit = 1 << gloLnShift
)

// GPS subframe-1 health-word structure (IS-GPS-200N §20.3.3.3.1.4): the MSB is
// the LNAV-data health summary; the 5 LSBs are the signal-component code of
// Table 20-VIII. Two component codes are singled out by §6.4.6.3 as NOT merely
// "marginal" for the C/A signal: 00010 (all signals dead) and 11100 (SV
// temporarily out — "do not use this SV during current pass").
const (
	gpsHealthNavDataBad = 0x20 // MSB set: "some or all LNAV data are bad"
	gpsHealthCompMask   = 0x1F // 5-bit signal-component code (Table 20-VIII)
	gpsCompAllDead      = 0x02 // Table 20-VIII 00010: all signals dead
	gpsCompTempOut      = 0x1C // Table 20-VIII 11100: SV is temporarily out
)

// QZSS subframe-1 health-word structure — deliberately DIFFERENT from GPS
// (QZSS-PNT-006 §4.1.2.3(4), Tables 4.1.2-5-1/-2): the MSB is the health of the
// transmitted L1 signal (L1C/A or L1C/B); the 5 LSBs are per-signal bits
// L1C/A · L2C · L5 · L1C · L1C/B (MSB→LSB). Because L1C/A and L1C/B are
// exclusively transmitted, the UNTRANSMITTED one's bit is 1 in normal
// operation — a QZSS SV broadcasting health 000001 is healthy, not faulty.
const (
	qzssHealthL1   = 0x20 // MSB: transmitted-L1-signal (L1C/A or L1C/B) health
	qzssHealthL1CA = 0x10
	qzssHealthL2C  = 0x08
	qzssHealthL5   = 0x04
	qzssHealthL1C  = 0x02
	qzssHealthL1CB = 0x01
)

// healthFor maps a constellation's raw broadcast health bits to the frozen
// health_code / health_issue_level enums (docs/OUTPUT.md §2.2): health_code 0
// unknown · 1 OK · 2 not-ok · 3 do-not-use; issue level 0 none · 1 warning · 2
// error. The bit semantics differ per constellation AND per signal
// (docs/CONSTELLATIONS.md §2); sig is the entry's u-blox sigId (0 = the
// constellation's primary civil signal).
func healthFor(g gnss.GNSSID, sig, raw int) (code, level int) {
	switch g {
	case gnss.Galileo:
		// SHS (Signal Health Status), 2 bits — GAL-OS-SIS-ICD-2.2 Table 84:
		// 0 = Signal OK, 1 = Signal out of service, 2 = Signal in Extended
		// Operations Mode (EOM), 3 = Signal Component currently in Test.
		// Issue 2.2 REDEFINED SHS=2 — the pre-2.2 gloss was "will be
		// out of service", and mapping from that memory would mark every EOM
		// satellite (a usable, published operational mode) dead on the map. Do
		// NOT promote SHS=2 to do-not-use. The 2/3 → not-ok(2)/warning(1)
		// mapping below is a deliberate conservative choice for both EOM and
		// in-test; a distinct EOM label is a candidate for the next OUTPUT.md
		// §2.2 enum rev, not a unilateral change here. Whether SHS=3 should
		// instead be do-not-use is an open product-enum call (the ICD does not
		// dictate it) — recorded in REVIEW-FABLE5_GALILEO §Could-not-verify.
		switch raw {
		case 0:
			return 1, 0
		case 1:
			return 3, 2
		default:
			return 2, 1
		}
	case gnss.BeiDou:
		// D1 SatH1 / B-CNAV2 HS. HS=1 is BDS-SIS-ICD-B2a v1.0 Table
		// 7-22 — "The satellite is unhealthy or in the test / The satellite
		// does not provide services" — the same do-not-use semantic as GPS's
		// nav-data-bad word and Galileo's SHS=1 ("out of service"), both mapped
		// to 3 above/below; leaving BeiDou at 2 rendered the single most
		// operationally important BDS health state as merely "not-ok" on the
		// map. SatH1 shares the raw==1 arm: it is 1 bit ("0 means broadcasting
		// satellite is good and 1 means not", BDS-SIS-ICD-B1I v3.0 §5.2.4.6 —
		// no explicit "shall not be used" phrasing, but treating a B1I
		// "not good" satellite as do-not-use matches BDS operational practice
		// and keeps the two message families' shared st.health unambiguous).
		// HS=2/3 are Table 7-22 "Reserved": undefined ≠ known-dead, so they
		// stay at not-ok rather than being promoted.
		switch raw {
		case 0:
			return 1, 0
		case 1:
			return 3, 2
		default:
			return 2, 2
		}
	case gnss.GLONASS:
		// raw's low 3 bits are the RAW Bn field (r.Bits(5,3)), not just its
		// MSB -- the two low-order bits carry other GLONASS ICD Ed. 5.1 flags, not
		// overall SV health. Only bit 2 (value 4, the MSB) is the malfunction
		// indicator, so mask to it before the zero test: without the mask, a
		// benign low bit alone (raw 1 or 2) would flag a healthy SV as not-ok and
		// fire a spurious health_change/critical event. bit 3 is the
		// packed GLONASS-M ℓn fast flag (see gloLnMalfunctionBit) — ICD Table 5.1
		// defines operability over Bn(ℓn) jointly, so EITHER malfunction bit set
		// is not-ok.
		if raw&(gloBnMalfunctionBit|gloLnMalfunctionBit) == 0 {
			return 1, 0
		}
		return 2, 2
	case gnss.GPS:
		if sig != 0 {
			// L2C/L5 per-signal entry: raw is the tracked carrier's own 1-bit CNAV
			// health (extracted by cnavCarrierHealth from MT10's 3-bit L1/L2/L5
			// field). IS-GPS-200N §30.3.3.1.1.2: 1 = "all codes and data on this
			// carrier are bad or unavailable" — do-not-use for this entry's signal.
			if raw == 0 {
				return 1, 0
			}
			return 3, 2
		}
		// the subframe-1 word is MSB + component code, not an opaque
		// number. The previous "any nonzero → not-ok/error" conflated the two and
		// fired a critical health_change for e.g. an L2-only component issue on an
		// SV whose L1 C/A (the signal this entry tracks) is fine.
		switch {
		case raw == 0:
			return 1, 0 // "all LNAV data are OK" + "all signals OK"
		case raw&gpsHealthNavDataBad != 0:
			// The LNAV data summary says the CEI data this daemon decodes and
			// serves is bad; IS-GPS-200N §6.4.6.1 item 4 — such signals "should be
			// ignored". (0x3F, the old special case, is subsumed here.)
			return 3, 2
		case raw&gpsHealthCompMask == gpsCompAllDead, raw&gpsHealthCompMask == gpsCompTempOut:
			return 3, 2 // all signals dead / SV temporarily out (§6.4.6.3 carve-outs)
		default:
			// IS-GPS-200N §6.4.6.3: MSB 0 with any other nonzero component code is
			// the ICD's own "marginal" — the C/A signal "may be ignored", not
			// must-not-use. Not-ok at WARNING level, so the detector no longer
			// manufactures criticals for routine component codes.
			return 2, 1
		}
	case gnss.QZSS:
		if sig != 0 {
			// Same CNAV per-carrier bit as GPS (QZSS-PNT-006 §4.3.2 mirrors
			// IS-GPS-200's MT10 health field).
			if raw == 0 {
				return 1, 0
			}
			return 3, 2
		}
		// QZSS-PNT-006 §4.1.2.3(4): see the qzssHealth* constants — this word is
		// NOT GPS-shaped. In particular the exclusive L1C/A / L1C/B pair means
		// exactly one of those two bits is 1 in NORMAL operation, so the previous
		// GPS-style "nonzero → not-ok/critical" misclassified every healthy QZSS
		// SV that broadcast the designed 000001/010000 pattern.
		switch {
		case raw&qzssHealthL1 != 0:
			// The transmitted L1 signal (what this sig-0 entry tracks) is
			// unhealthy: do-not-use.
			return 3, 2
		case raw&(qzssHealthL2C|qzssHealthL5|qzssHealthL1C) != 0:
			return 2, 1 // another PNT signal is flagged; tracked L1 is fine — marginal
		case raw&(qzssHealthL1CA|qzssHealthL1CB) == qzssHealthL1CA|qzssHealthL1CB:
			// Both of the exclusively-transmitted pair flagged while the L1
			// summary reads healthy: inconsistent broadcast — surface as marginal.
			return 2, 1
		default:
			// 0, or exactly one of the L1C/A / L1C/B pair set — the designed
			// normal-operation patterns.
			return 1, 0
		}
	default:
		// NavIC — UNREACHABLE placeholder : the decoder is a tracked
		// stub (regression fix, gnss/frame/navic.go), nothing ever creates a NavIC
		// svState, and this GPS-flavored opaque mapping was never ICD-derived.
		// The IRNSS SPS ICD does NOT define a 6-bit health word: NavIC health
		// is two ONE-BIT flags — "L5 flag" and "S flag", 1 = "Some or all
		// navigation data on [that] SPS signal are bad" (NAVIC-SPS-L5S
		// §6.2.1.6 Table 24). When the decoder lands, replace this arm with a
		// per-signal Table 24 mapping (the CNAV per-carrier arms above are the
		// shape to follow); do not let this placeholder go live as-is.
		if raw == 0 {
			return 1, 0
		}
		if raw == 0x3F {
			return 3, 2
		}
		return 2, 2
	}
}

// sisaFor decodes a stored accuracy index to metres via the constellation's table.
func sisaFor(kind uint8, idx int) (float64, bool) {
	switch kind {
	case accURA:
		return accuracy.URAMeters(idx)
	case accSISA:
		return accuracy.GalileoSISA(idx)
	case accURAED:
		return accuracy.URAEDMeters(idx) // CNAV signed URA_ED 
	default:
		// accSISAIRaw lands here BY DESIGN : the B2a ICD v1.0 defers
		// the SISAI index→metres tables to "a future update", so there is
		// nothing lawful to convert with — the raw packed index is served via
		// acc_index and sisa_valid stays false. Do not add a table here
		// without a published ICD revision to cite.
		return 0, false
	}
}

// weekFor returns the full (rollover-disambiguated) week number for the SV's time
// system at now: GPS week for GPS/Galileo/QZSS (the regression fix serving convention —
// see timeSysFor), BDT week (GPS week − 1356) for BeiDou. GLONASS has no week
// number. reduced via gnsstime's single epoch table, so the regression fix
// guarantee — the BDT week and towFor's BDT tow derive from the SAME shifted
// seconds, never disagreeing across the 14 s window each week where GPS tow ∈
// [0, 14) — now holds structurally: both go through SystemSeconds(SysBeiDou)
// and the one audited epoch constant, instead of two hand-kept literals.
func weekFor(g gnss.GNSSID, now time.Time) (int, bool) {
	if g == gnss.GLONASS {
		return 0, false
	}
	return gnsstime.WeekAt(timeSysFor(g), float64(now.Unix()), float64(gpsUTCOffset))
}

// fullName renders a human label like "GPS-5" or "QZSS-3", with a signal suffix for
// non-primary signals (docs/OUTPUT.md §1.1).
func fullName(g gnss.GNSSID, svid, sig int) string {
	var name string
	switch g {
	case gnss.GPS:
		name = "GPS"
	case gnss.Galileo:
		name = "Galileo"
	case gnss.BeiDou:
		name = "BeiDou"
	case gnss.QZSS:
		name = "QZSS"
	case gnss.NavIC:
		name = "NavIC"
	case gnss.GLONASS:
		name = "GLONASS"
	case gnss.SBAS:
		name = "SBAS"
	default:
		name = "SV"
	}
	if sig != 0 {
		return fmt.Sprintf("%s-%d (sig %d)", name, svid, sig)
	}
	return fmt.Sprintf("%s-%d", name, svid)
}
