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
	IOD              *int     `json:"iod,omitempty"`
	OrbitDiscoM      *float64 `json:"orbit_disco_m,omitempty"`
	OrbitDiscoAgeS   *float64 `json:"orbit_disco_age_s,omitempty"`
	TimeDiscoNs      *float64 `json:"time_disco_ns,omitempty"`
	AODC             *int     `json:"aodc,omitempty"`
	AODE             *int     `json:"aode,omitempty"`
	Af0              *float64 `json:"af0,omitempty"`
	Af1              *float64 `json:"af1,omitempty"`
	Af2              *float64 `json:"af2,omitempty"`
	XM               *float64 `json:"x_m,omitempty"`
	YM               *float64 `json:"y_m,omitempty"`
	ZM               *float64 `json:"z_m,omitempty"`
	Tow              *int     `json:"tow,omitempty"`
	Wn               *int     `json:"wn,omitempty"`
	LastSeenS        int      `json:"last_seen_s"`

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

// feedSV projects one svState to a FeedSV. The caller holds the shard lock.
func (st *svState) feedSV(now time.Time) FeedSV {
	g := st.key.G
	// health_code 0 ("unknown") until this SV's own health bits have
	// actually been decoded -- an iono-only SV, or a Galileo SV whose
	// word 5 hasn't arrived yet, otherwise served a false "OK" from st.health's
	// zero value.
	var code, level int
	if st.haveHealth {
		code, level = healthFor(g, st.health)
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
	if m, ok := sisaFor(st.accKind, st.accIdx); ok && finite(m) {
		e.SISAValid, e.SISAM = true, &m
	}
	if st.haveAOD {
		aodc, aode := st.aodc, st.aode
		e.AODC, e.AODE = &aodc, &aode
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
			age := gnsstime.EphAgeDay(gloTOD(now), st.gloEph.Tb) / 60.0
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
		if finite(st.clk.Af0) && finite(st.clk.Af1) && finite(st.clk.Af2) {
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
		// forever.
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
		if ent, seen := out[name]; seen {
			// the observed entry keeps its precise ECEF/epoch, but its
			// always-present inclination/t0e metadata comes from the fresh
			// broadcast almanac instead of fabricated zeros. lambda_na remains
			// intentionally absent on observed entries.
			if ent.Observed {
				ent.InclinationRad = gloMeanInclination + a.Alm.DeltaI
				ent.T0e = int(a.Alm.Tlambda)
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
		}
	}
}

// sbasStaleAfter bounds how long an SBAS PRN may go unseen before it is dropped from the
// feed, mirroring rfStaleAfter: a live SBAS GEO broadcasts continuously (message
// type 1 every few seconds), so this is generous margin over normal operation while still
// catching a PRN whose station has gone dark or been decommissioned rather than serving
// its last-known health with an ever-growing last_seen_s forever.
const sbasStaleAfter = rfStaleAfter

// FeedSBAS builds the sbas augmentation-health feed as of now (docs/OUTPUT.md §1.5).
func (s *Store) FeedSBAS(now time.Time) map[string]SBASEntry {
	out := make(map[string]SBASEntry)
	s.sbasMu.Lock()
	defer s.sbasMu.Unlock()
	for prn, st := range s.sbas {
		if now.Sub(st.lastSeen) > sbasStaleAfter {
			continue
		}
		code := 1 // OK
		if st.doNotUse {
			code = 3 // do-not-use
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

// healthFor maps a constellation's raw broadcast health bits to the frozen
// health_code / health_issue_level enums (docs/OUTPUT.md §2.2): health_code 0
// unknown · 1 OK · 2 not-ok · 3 do-not-use; issue level 0 none · 1 warning · 2
// error. The bit semantics differ per constellation (docs/CONSTELLATIONS.md §2).
func healthFor(g gnss.GNSSID, raw int) (code, level int) {
	switch g {
	case gnss.Galileo:
		// SHS (signal health status), 2 bits: 0 OK, 1 out-of-service (do-not-use),
		// 2 will-be-in-test, 3 in-test.
		switch raw {
		case 0:
			return 1, 0
		case 1:
			return 3, 2
		default:
			return 2, 1
		}
	case gnss.BeiDou:
		// D1 SatH1 / B-CNAV2 HS: 0 healthy, non-zero unhealthy.
		if raw == 0 {
			return 1, 0
		}
		return 2, 2
	case gnss.GLONASS:
		// frame.DecodeGLONASSString stores the RAW 3-bit Bn field
		// (r.Bits(5,3)), not just its MSB -- the two low-order bits carry other
		// GLONASS ICD Ed. 5.1 flags, not overall SV health. Only bit 2 (value 4,
		// the MSB) is the malfunction indicator, so mask to it before the zero
		// test: without the mask, a benign low bit alone (raw 1 or 2) would flag
		// a healthy SV as not-ok and fire a spurious health_change/critical event.
		if raw&gloBnMalfunctionBit == 0 {
			return 1, 0
		}
		return 2, 2
	default: // GPS / QZSS / NavIC: 6-bit health, 0 = all signals OK.
		if raw == 0 {
			return 1, 0
		}
		if raw == 0x3F {
			return 3, 2 // all-ones: SV shall not be used
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
	default:
		return 0, false
	}
}

// weekFor returns the full (rollover-disambiguated) week number for the SV's time
// system at now: GPS week for GPS/Galileo/QZSS, BDT week (GPS week − 1356) for
// BeiDou. GLONASS has no week number.
func weekFor(g gnss.GNSSID, now time.Time) (int, bool) {
	gps := now.Unix() - gpsEpochUnix + gpsUTCOffset
	switch g {
	case gnss.GLONASS:
		return 0, false
	case gnss.BeiDou:
		// the BDT week must come from the same BDT-shifted seconds
		// (GPST − 14 s, matching towFor's shift) as the tow, not from unshifted
		// GPS seconds — otherwise, during the 14 s each week where GPS tow ∈
		// [0, 14), the unshifted week has already rolled over while the shifted
		// tow still reports the tail of the previous BDT week, and a consumer
		// reconstructing absolute BDT time from (wn, tow) is a full week off.
		return int((gps-14)/weekSeconds) - 1356, true // BDT epoch is 2006-01-01, 1356 weeks after GPS
	default:
		return int(gps / weekSeconds), true
	}
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
