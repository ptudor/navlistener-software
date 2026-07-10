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
	IOD              int      `json:"iod"`
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
	code, level := healthFor(g, st.health)
	e := FeedSV{
		FullName:         fullName(g, st.key.Sv, st.key.Sig),
		Name:             fmt.Sprintf("%c%02d", g.Letter(), st.key.Sv),
		GnssID:           int(g),
		SvID:             st.key.Sv,
		SigID:            st.key.Sig,
		HealthCode:       code,
		HealthIssueLevel: level,
		HealthSubcode:    st.health,
		IOD:              st.iod,
		LastSeenS:        int(now.Sub(st.lastSeen).Seconds()),
	}
	if st.havePos && finiteECEF(st.pos) {
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
		if !tr.hasDelay || !finite(tr.delayM) {
			continue
		}
		if e.Perrecv == nil {
			e.Perrecv = map[string]*FeedPerRecv{}
		}
		d, ps := tr.delayM, tr.pairSig
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

	// Kepler-family: clock polynomial, time-of-week/week, ephemeris age.
	if finite(st.clk.Af0) && finite(st.clk.Af1) && finite(st.clk.Af2) {
		af0, af1, af2 := st.clk.Af0, st.clk.Af1, st.clk.Af2
		e.Af0, e.Af1, e.Af2 = &af0, &af1, &af2
	}
	tow := int(towFor(g, now))
	e.Tow = &tow
	if wn, ok := weekFor(g, now); ok {
		e.Wn = &wn
	}
	age := gnsstime.EphAgeMinutes(towFor(g, now), st.eph.Toe)
	if finite(age) {
		e.EphAgeM = &age
	}
	return e
}

// FeedGlobal builds the global counters feed as of now (docs/OUTPUT.md §1.2).
func (s *Store) FeedGlobal(now time.Time) GlobalFeed {
	g := GlobalFeed{LeapSeconds: gpsUTCOffset, Counts: map[string]int{}}
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
	if !last.IsZero() {
		g.LastSeen = last.Unix()
	}
	return g
}

// FeedAlmanac builds the almanac feed as of now (docs/OUTPUT.md §1.4), one coarse
// entry per currently-observed SV derived from its precise broadcast ephemeris.
func (s *Store) FeedAlmanac(now time.Time) map[string]AlmanacEntry {
	out := make(map[string]AlmanacEntry)
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, st := range sh.m {
			if !st.havePos || !finiteECEF(st.pos) {
				continue
			}
			name := fmt.Sprintf("%c%02d", st.key.G.Letter(), st.key.Sv)
			if _, seen := out[name]; seen {
				continue // one entry per SV; the first signal that has a fix wins
			}
			ent := AlmanacEntry{
				Name:      name,
				GnssID:    int(st.key.G),
				Observed:  true,
				EcefXM:    st.pos.X,
				EcefYM:    st.pos.Y,
				EcefZM:    st.pos.Z,
				T:         int(now.Unix()),
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
// with the analytic almanac propagator (docs/MATH.md §3.1). The current day-number is the
// broadcast NA — kept current by the live stream — so propagating at NA to the current
// GLONASS time-of-day gives the present position without reimplementing GLONASS calendar
// arithmetic.
func (s *Store) addGlonassAlmanac(out map[string]AlmanacEntry, now time.Time) {
	s.gloAlmMu.Lock()
	na := s.gloNA
	alms := make([]frame.GLONASSAlmanacEntry, 0, len(s.gloAlmanac))
	for _, a := range s.gloAlmanac {
		alms = append(alms, a)
	}
	s.gloAlmMu.Unlock()
	if na == 0 {
		return // no day-number anchor yet; the almanac time base is unknown
	}

	ti := gloTOD(now)
	ell := physconst.WGS84
	if p, ok := physconst.For(gnss.GLONASS); ok {
		ell = p.Datum
	}
	for _, a := range alms {
		name := fmt.Sprintf("R%02d", a.Alm.Slot)
		if _, seen := out[name]; seen {
			continue // observed → its precise broadcast-ephemeris entry wins
		}
		pos, err := glonass.PropagateAlmanacECEF(a.Alm, na, ti)
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

// FeedSBAS builds the sbas augmentation-health feed as of now (docs/OUTPUT.md §1.5).
func (s *Store) FeedSBAS(now time.Time) map[string]SBASEntry {
	out := make(map[string]SBASEntry)
	s.sbasMu.Lock()
	defer s.sbasMu.Unlock()
	for prn, st := range s.sbas {
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
		// Bn MSB (bit 2) set ⇒ malfunction.
		if raw == 0 {
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
	gpsWeek := int((now.Unix() - gpsEpochUnix + gpsUTCOffset) / weekSeconds)
	switch g {
	case gnss.GLONASS:
		return 0, false
	case gnss.BeiDou:
		return gpsWeek - 1356, true // BDT epoch is 2006-01-01, 1356 weeks after GPS
	default:
		return gpsWeek, true
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
