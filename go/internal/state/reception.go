package state

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/geo"
	"github.com/ptudor/gnss/glonass"
	"github.com/ptudor/gnss/gnsstime"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
	"github.com/ptudor/navlistener/internal/reception"
)

// ReceptionForecast uses only this audience's state. Each included orbit must
// have a recent navigation witness other than the station being assessed.
// Witness recency does not imply bitwise agreement or authenticated RF.
func (s *Store) ReceptionForecast(site reception.Site, now time.Time) reception.Expectation {
	e := reception.Expectation{ID: 1, Issued: now.Unix(), Latitude: int32(math.Round(site.Position[0] * 1e7)), Longitude: int32(math.Round(site.Position[1] * 1e7)), RadiusM: site.RadiusM, AlarmSeconds: site.AlarmSeconds, ClearSeconds: site.ClearSeconds, MinExpected: site.MinExpected, MinMissing: site.MinMissing, MissingPercent: site.MissingPercent}
	pos := geo.Geodetic{Lat: site.Position[0] * math.Pi / 180, Lon: site.Position[1] * math.Pi / 180, Height: site.Position[2]}
	entries := map[[3]uint8]uint8{}
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, st := range sh.m {
			g, sig := uint8(st.key.G), reception.CanonicalSignal(uint8(st.key.G), uint8(st.key.Sig))
			if !site.Allows(g, sig) {
				continue
			}
			feed := st.feedSV(now, nil)
			if feed.HealthCode != 1 || feed.XM == nil {
				continue
			}
			witness := false
			for id, at := range st.seenBy {
				if id != site.Observer && !at.After(now) && now.Sub(at) <= 60*time.Second {
					witness = true
					break
				}
			}
			if !witness {
				continue
			}
			if !site.PerSignal {
				sig = reception.Satellite
			}
			key := [3]uint8{g, uint8(st.key.Sv), sig}
			for slot := 0; slot < reception.Slots; slot++ {
				visible := true
				// Ten-second samples with a two-degree margin exclude horizon crossings.
				for sec := slot * 60; sec <= (slot+1)*60; sec += 10 {
					at := now.Add(time.Duration(sec) * time.Second)
					xyz, ok := forecastPosition(st, at)
					if !ok {
						visible = false
						break
					}
					_, el := geo.AzEl(xyz, pos, physconst.WGS84)
					if el*180/math.Pi < site.Elevation+2 {
						visible = false
						break
					}
				}
				if visible {
					entries[key] |= 1 << uint(slot)
				}
			}
		}
		sh.mu.Unlock()
	}
	if len(entries) > reception.MaxEntries {
		entries = nil
	} // unavailable rather than silently truncate coverage
	for k, slots := range entries {
		e.Entries = append(e.Entries, reception.Entry{GNSS: k[0], SV: k[1], Signal: k[2], Slots: slots})
	}
	sort.Slice(e.Entries, func(i, j int) bool {
		a, b := e.Entries[i], e.Entries[j]
		if a.GNSS != b.GNSS {
			return a.GNSS < b.GNSS
		}
		if a.SV != b.SV {
			return a.SV < b.SV
		}
		return a.Signal < b.Signal
	})
	b, _ := e.Encode()
	sum := sha256.Sum256(append([]byte(site.Observer+"\x00"), b...))
	e.ID = binary.BigEndian.Uint64(sum[:])
	if e.ID == 0 {
		e.ID = 1
	}
	return e
}

func forecastPosition(st *svState, at time.Time) (gnss.ECEF, bool) {
	if st.key.G == gnss.GLONASS {
		if !st.haveGloEph || st.gloTbAt.IsZero() || at.Sub(st.gloTbAt) > gloPropagateMaxEphAge || (!st.gloEphRecvAt.IsZero() && at.Sub(st.gloEphRecvAt) > gloPropagateMaxEphAge) {
			return gnss.ECEF{}, false
		}
		tk := gnsstime.EphAgeDay(gloTOD(at), st.gloEph.Tb)
		if tk > gloServeMaxTk.Seconds() || tk < gloServeMinTk.Seconds() {
			return gnss.ECEF{}, false
		}
		p, err := glonass.Propagate(st.gloEph, tk)
		return p, err == nil && finiteECEF(p)
	}
	if !st.haveEph || st.ephAt.IsZero() || at.Sub(st.ephAt) > propagateMaxEphAge || (!st.ephRecvAt.IsZero() && at.Sub(st.ephRecvAt) > propagateMaxEphAge) {
		return gnss.ECEF{}, false
	}
	p, err := kepler.Propagate(st.eph, towFor(st.key.G, at))
	return p, err == nil && finiteECEF(p)
}
