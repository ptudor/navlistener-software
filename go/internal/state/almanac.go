package state

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/gnss/glonass"
	"github.com/ptudor/gnss/gnsstime"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// Kepler-family broadcast almanacs (GPS and QZSS LNAV pages, Galileo I/NAV
// word types 7–10, BeiDou B-CNAV2 midi almanacs and D1 almanac pages). Every
// satellite
// broadcasts its whole constellation's almanac, so one station hearing any
// satellite learns where all of them are, including those no station can
// hear. Entries are keyed by the satellite an almanac describes, not the one
// that broadcast it, and give coarse positions to the almanac feed and the
// coverage map wherever no fresh ephemeris position exists.

// almanacKey identifies the satellite an almanac describes.
type almanacKey struct {
	G  gnss.GNSSID
	Sv int
}

// keplerAlmanac is one satellite's newest accepted almanac.
type keplerAlmanac struct {
	eph   kepler.Ephemeris // Toe is toa, in the constellation's seconds of week
	toa   time.Time        // toa resolved to an absolute instant
	stamp time.Time        // reception stamp of the page that delivered it
	// exact marks a toa fixed by a broadcast week number (BeiDou midi
	// almanacs and D1 page 8) rather than resolved from its time of week.
	exact bool
}

// keplerAlmanacValidity bounds |t − toa| for accepting and serving an almanac.
// GPS keeps t within 3.5 days of toa while a set is transmitted
// (IS-GPS-200N §20.3.3.5.2.2). QZSS's validity period of 144 h is twice
// |t − toa| (QZSS-PNT-006 Table 4.1.1-2). The BeiDou almanac algorithm wraps
// t − toa at half a week (BDS-SIS-B2a-1.0 Table 7-15), but almanacs are only
// updated in less than 7 days (BDS-SIS-B1I-3.0 Table 5-1); the midi almanac's
// full week or D1 page 8's truncated week fixes toa, so almanacAt propagates
// across the wrap and BeiDou is served for 7 days. GAL-OS-SIS-ICD-2.2 states
// no almanac validity period, so Galileo uses half a week as an engineering
// bound. Where toa is resolved from a time of week, inside half a week the
// resolution is unambiguous, so those bounds are strict.
func keplerAlmanacValidity(g gnss.GNSSID) time.Duration {
	switch g {
	case gnss.QZSS:
		return 72 * time.Hour
	case gnss.BeiDou:
		return 7 * 24 * time.Hour
	}
	return 84 * time.Hour
}

// almanacAt re-references eph by whole weeks so that now lies within half a
// week of its toe, where the propagator's time-of-week wrap is the true
// elapsed time. An almanac carries no rate or harmonic terms, so the shift is
// exact: over k weeks the mean anomaly advances by n0·kW and, because Ω0 is
// referenced to the start of toa's week, the node by (Ω̇ − Ω̇e)·kW
// (IS-GPS-200N Table 20-IV; BDS-SIS-B2a-1.0 Table 7-15).
func almanacAt(eph kepler.Ephemeris, toa, now time.Time) (kepler.Ephemeris, bool) {
	weeks := math.Round(now.Sub(toa).Seconds() / gnsstime.WeekSeconds)
	if weeks == 0 {
		return eph, true
	}
	p, ok := physconst.For(eph.ID)
	a := eph.SqrtA * eph.SqrtA
	if !ok || !(a > 0) {
		return eph, false
	}
	dt := weeks * gnsstime.WeekSeconds
	eph.M0 = math.Remainder(eph.M0+math.Sqrt(p.Mu/(a*a*a))*dt, 2*math.Pi)
	eph.Omega0 = math.Remainder(eph.Omega0+(eph.OmegaDot-p.OmegaE)*dt, 2*math.Pi)
	return eph, true
}

// applyKeplerAlmanac stores an LNAV almanac page received at stamp (the
// receiver's clock). toa is resolved to the instant nearest stamp with that
// time of week. A page outside its validity window, or one rejected by the ICD
// range checks, is ignored. An older set never replaces a newer one; a page
// for the same toa replaces the stored one unless it was received earlier, so
// a re-upload that keeps toa still takes effect. Called with the transmitting
// SV's shard lock held (ordering shard→alm).
func (s *Store) applyKeplerAlmanac(g gnss.GNSSID, a *frame.LNAVAlmanac, stamp time.Time) {
	eph, err := a.Ephemeris(g)
	if err != nil {
		return
	}
	s.storeAlmanac(eph, stamp, -1)
}

// applyGalileoAlmanac stores a completed Galileo almanac like applyKeplerAlmanac.
// The broadcast WNa, the two least significant bits of t0a's GST week
// (GAL-OS-SIS-ICD-2.2 §5.1.10), must agree with the week t0a resolves to.
func (s *Store) applyGalileoAlmanac(a frame.GalileoAlmanac, stamp time.Time) {
	s.storeAlmanac(a.Ephemeris(), stamp, a.WNa)
}

// applyBeiDouAlmanac stores a B-CNAV2 midi almanac. Its 13-bit BDT week fixes
// toa exactly (BDS-SIS-B2a-1.0 §7.9.2: toa counts from the start of WNa).
func (s *Store) applyBeiDouAlmanac(a *frame.BeiDouMidiAlmanac, stamp time.Time) {
	eph, err := a.Ephemeris()
	if err != nil {
		return
	}
	unix, ok := gnsstime.GNSSTime{Sys: gnsstime.SysBeiDou, Week: a.WN, TOW: a.Toa}.ToUnix(float64(gpsUTCOffset))
	if !ok {
		return
	}
	s.storeAlmanacAt(eph, time.Unix(int64(math.Round(unix)), 0), stamp, true) // toa is whole seconds
}

// bdsAmEpIDFresh bounds how old a satellite's last AmEpID may be when one of
// its expanded pages arrives. Every 30 s D1 frame carries a subframe 4 basic
// page with AmEpID (BDS-SIS-B1I-3.0 §5.2.4.13–14), so a few minutes is
// generous. Engineering bound.
const bdsAmEpIDFresh = 5 * time.Minute

// applyBeiDouD1Almanac stores the almanac on a D1 subframe 4/5 page. Subframe
// 5 page 8 carries the truncated BDT week and toa that anchor the set. Basic
// pages 1–30 are almanacs for every AmEpID; AmEpID "11" is required only for
// expanded pages (Table 5-13). Until page 8 arrives, the conservative fallback
// accepts pages only from an AmEpID "11" transmitter and resolves toa to the
// instant nearest reception. Caller holds the SV's shard lock.
func (s *Store) applyBeiDouD1Almanac(st *svState, sf *frame.BeiDouSubframe, stamp, local time.Time) {
	if sf.HasAmEpID {
		st.bdsAmEpID, st.bdsAmEpIDAt = sf.AmEpID, local
	}
	if sf.HasAlmanacRef {
		week := gnsstime.DisambiguateWeek(gnsstime.SysBeiDou, sf.AlmanacWN, 8,
			float64(stamp.Unix()), float64(gpsUTCOffset))
		unix, ok := gnsstime.GNSSTime{
			Sys: gnsstime.SysBeiDou, Week: week, TOW: sf.AlmanacToa,
		}.ToUnix(float64(gpsUTCOffset))
		if ok {
			ref := time.Unix(int64(math.Round(unix)), 0)
			age := stamp.Sub(ref)
			if age > -keplerAlmanacValidity(gnss.BeiDou) && age < keplerAlmanacValidity(gnss.BeiDou) &&
				(st.bdsAlmRef.IsZero() || ref.After(st.bdsAlmRef) ||
					(ref.Equal(st.bdsAlmRef) && local.After(st.bdsAlmRefAt))) {
				st.bdsAlmRef, st.bdsAlmRefTOW, st.bdsAlmRefAt = ref, sf.AlmanacToa, local
			}
		}
	}
	a := sf.Almanac
	if a == nil {
		return
	}
	resolved := *a
	freshExpanded := !st.bdsAmEpIDAt.IsZero() && local.Sub(st.bdsAmEpIDAt) <= bdsAmEpIDFresh && st.bdsAmEpID == 3
	if resolved.Expanded {
		if !freshExpanded || !resolved.ResolveExpanded(st.bdsAmEpID) {
			return
		}
	}
	eph, err := resolved.Ephemeris()
	if err != nil {
		return
	}
	if !st.bdsAlmRef.IsZero() {
		if resolved.Toa != st.bdsAlmRefTOW {
			return // page belongs to another almanac set
		}
		s.storeAlmanacAt(eph, st.bdsAlmRef, stamp, true)
		return
	}
	if !freshExpanded {
		return // wait for page 8 rather than risk a one-week ambiguity
	}
	s.storeAlmanac(eph, stamp, -1)
}

// storeAlmanac resolves eph's toa (its Toe) to the instant nearest stamp with
// that time of week and stores it under its subject satellite. wna, when not
// -1, is the broadcast two-bit week of toa, which the resolved week must match.
func (s *Store) storeAlmanac(eph kepler.Ephemeris, stamp time.Time, wna int) {
	g := eph.ID
	age := gnsstime.EphAge(towFor(g, stamp), eph.Toe)
	toa := stamp.Add(-time.Duration(age * float64(time.Second)))
	if wna >= 0 {
		week, ok := gnsstime.WeekAt(gnsstime.SysGalileo, float64(toa.Unix()), float64(gpsUTCOffset))
		if !ok || week&3 != wna {
			return
		}
	}
	s.storeAlmanacAt(eph, toa, stamp, false)
}

// storeAlmanacAt stores eph, whose toa is the absolute instant toa, under its
// subject satellite, unless the page that delivered it (received at stamp) lies
// outside the validity window around toa. exact marks a toa fixed by a full
// week number; such an almanac is never replaced by one resolved from a time
// of week, which could be a week off.
func (s *Store) storeAlmanacAt(eph kepler.Ephemeris, toa, stamp time.Time, exact bool) {
	g := eph.ID
	if age := stamp.Sub(toa); age <= -keplerAlmanacValidity(g) || age >= keplerAlmanacValidity(g) {
		return
	}
	key := almanacKey{G: g, Sv: eph.SVID}
	s.almMu.Lock()
	defer s.almMu.Unlock()
	if prev, ok := s.almanacs[key]; ok {
		if prev.exact && !exact {
			return
		}
		if prev.exact == exact && (prev.toa.After(toa) || (prev.toa.Equal(toa) && prev.stamp.After(stamp))) {
			return
		}
	}
	s.almanacs[key] = keplerAlmanac{eph: eph, toa: toa, stamp: stamp, exact: exact}
}

// almanacPosition is one satellite's almanac-propagated position.
type almanacPosition struct {
	name        string
	g           gnss.GNSSID
	pos         gnss.ECEF
	inclination float64 // rad
	t0e         int     // the almanac's own reference: toa (s of week) or GLONASS t_λ (s of day)
}

// keplerAlmanacPositions propagates every GPS, Galileo, QZSS and BeiDou almanac still inside its
// validity window to now, sorted by name.
func (s *Store) keplerAlmanacPositions(now time.Time) []almanacPosition {
	s.almMu.Lock()
	entries := make([]keplerAlmanac, 0, len(s.almanacs))
	for _, a := range s.almanacs {
		if age := now.Sub(a.toa); age > -keplerAlmanacValidity(a.eph.ID) && age < keplerAlmanacValidity(a.eph.ID) {
			entries = append(entries, a)
		}
	}
	s.almMu.Unlock()
	out := make([]almanacPosition, 0, len(entries))
	for _, a := range entries {
		eph, ok := almanacAt(a.eph, a.toa, now)
		if !ok {
			continue
		}
		pos, err := kepler.Propagate(eph, towFor(a.eph.ID, now))
		if err != nil || !finiteECEF(pos) {
			continue
		}
		out = append(out, almanacPosition{
			name:        fmt.Sprintf("%c%02d", a.eph.ID.Letter(), a.eph.SVID),
			g:           a.eph.ID,
			pos:         pos,
			inclination: a.eph.I0,
			t0e:         int(a.eph.Toe),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// expireKeplerAlmanacs drops almanacs that have left their validity window.
func (s *Store) expireKeplerAlmanacs(now time.Time) {
	s.almMu.Lock()
	defer s.almMu.Unlock()
	for key, a := range s.almanacs {
		if age := now.Sub(a.toa); age <= -keplerAlmanacValidity(key.G) || age >= keplerAlmanacValidity(key.G) {
			delete(s.almanacs, key)
		}
	}
}

// glonassAlmanacEntries returns the GLONASS almanac slots still being
// rebroadcast, and whether the frame day number NA that anchors their time base
// is known yet.
func (s *Store) glonassAlmanacEntries(now time.Time) ([]frame.GLONASSAlmanacEntry, bool) {
	s.gloAlmMu.Lock()
	defer s.gloAlmMu.Unlock()
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
	sort.Slice(alms, func(i, j int) bool { return alms[i].Alm.Slot < alms[j].Alm.Slot })
	return alms, s.gloNA != 0
}

// glonassAlmanacPosition propagates one GLONASS almanac slot to now. The
// propagation day is the actual current MT calendar day (gloNTDay), not the
// broadcast NA: NA is only the day each almanac's elements are referenced to
// (stored per-entry in Alm.NA), and it lags the calendar, so propagating at NA
// would evaluate every out-of-view SV's position a day (or more) in the past,
// off by tens of thousands of km along-track.
func glonassAlmanacPosition(a frame.GLONASSAlmanacEntry, now time.Time) (gnss.ECEF, bool) {
	pos, err := glonass.PropagateAlmanacECEF(a.Alm, gloNTDay(now), gloTOD(now))
	return pos, err == nil && finiteECEF(pos)
}

// almanacPositions returns every satellite the decoded almanacs can place at
// now: GPS and QZSS from LNAV pages, Galileo from I/NAV word types 7–10,
// BeiDou from B-CNAV2 midi almanacs and D1 pages, GLONASS from strings 6–15.
func (s *Store) almanacPositions(now time.Time) []almanacPosition {
	out := s.keplerAlmanacPositions(now)
	alms, anchored := s.glonassAlmanacEntries(now)
	if !anchored {
		return out // no day-number anchor yet; the GLONASS almanac time base is unknown
	}
	for _, a := range alms {
		pos, ok := glonassAlmanacPosition(a, now)
		if !ok {
			continue
		}
		out = append(out, almanacPosition{
			name:        fmt.Sprintf("R%02d", a.Alm.Slot),
			g:           gnss.GLONASS,
			pos:         pos,
			inclination: gloMeanInclination + a.Alm.DeltaI,
			t0e:         int(a.Alm.Tlambda),
		})
	}
	return out
}

// galAlmWord is one relay's most recent Galileo almanac word, with its feeder
// stamp (broadcast adjacency) and collector clock (aging out a silent relay).
type galAlmWord struct {
	w         *frame.GalileoINAV
	at, local time.Time
}

// galAlmanacPairWindow bounds the feeder-clock gap between the two words that
// carry one satellite's almanac. GAL-OS-SIS-ICD-2.2 Table 41 sequences them in
// the same or the next 30 s sub-frame; a gap past two sub-frames means a word
// was lost in between. Engineering bound from that sequencing.
const galAlmanacPairWindow = time.Minute

// swapGalAlmanacWord records word as relay's latest almanac word and returns
// the one it replaces. Relays silent for gloAlmPendingStale are dropped and, at
// gloAlmPendingMax, the oldest relay is evicted. Caller holds the SV's shard lock.
func (st *svState) swapGalAlmanacWord(relay gloAlmRelay, word galAlmWord) (galAlmWord, bool) {
	if st.galAlmLast == nil {
		st.galAlmLast = make(map[gloAlmRelay]galAlmWord, 2)
	}
	for k, p := range st.galAlmLast {
		if k != relay && word.local.Sub(p.local) > gloAlmPendingStale {
			delete(st.galAlmLast, k)
		}
	}
	prev, ok := st.galAlmLast[relay]
	if !ok && len(st.galAlmLast) >= gloAlmPendingMax {
		var oldest gloAlmRelay
		var oldestAt time.Time
		first := true
		for k, p := range st.galAlmLast {
			if first || p.local.Before(oldestAt) {
				oldest, oldestAt, first = k, p.local, false
			}
		}
		delete(st.galAlmLast, oldest)
	}
	st.galAlmLast[relay] = word
	return prev, ok
}
