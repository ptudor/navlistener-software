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
)

// Kepler-family broadcast almanacs (GPS and QZSS LNAV pages). Every satellite
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
}

// keplerAlmanacValidity bounds |t − toa| for accepting and serving an almanac.
// GPS keeps t within 3.5 days of toa while a set is transmitted
// (IS-GPS-200N §20.3.3.5.2.2). QZSS's validity period of 144 h is twice
// |t − toa| (QZSS-PNT-006 Table 4.1.1-2). Inside half a week the
// time-of-week wrap used by propagation is unambiguous, so the GPS bound is
// strict.
func keplerAlmanacValidity(g gnss.GNSSID) time.Duration {
	if g == gnss.QZSS {
		return 72 * time.Hour
	}
	return 84 * time.Hour
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
	age := gnsstime.EphAge(towFor(g, stamp), a.Toa)
	if math.Abs(age) >= keplerAlmanacValidity(g).Seconds() {
		return
	}
	toa := stamp.Add(-time.Duration(age * float64(time.Second)))
	key := almanacKey{G: g, Sv: a.SVID}
	s.almMu.Lock()
	defer s.almMu.Unlock()
	if prev, ok := s.almanacs[key]; ok && (prev.toa.After(toa) || (prev.toa.Equal(toa) && prev.stamp.After(stamp))) {
		return
	}
	s.almanacs[key] = keplerAlmanac{eph: eph, toa: toa, stamp: stamp}
}

// almanacPosition is one satellite's almanac-propagated position.
type almanacPosition struct {
	name        string
	g           gnss.GNSSID
	pos         gnss.ECEF
	inclination float64 // rad
	t0e         int     // the almanac's own reference: toa (s of week) or GLONASS t_λ (s of day)
}

// keplerAlmanacPositions propagates every GPS and QZSS almanac still inside its
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
		pos, err := kepler.Propagate(a.eph, towFor(a.eph.ID, now))
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
// now: GPS and QZSS from LNAV pages, GLONASS from strings 6–15.
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
