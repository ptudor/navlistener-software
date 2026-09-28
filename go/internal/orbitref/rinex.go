// Package orbitref supplies independent geometry for the monitoring map. These
// orbits are never ingested as receiver observations or used by detectors.
package orbitref

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/glonass"
	"github.com/ptudor/gnss/gnsstime"
	"github.com/ptudor/gnss/kepler"
)

// RINEX 3 navigation layouts: reference/RINEX-3.05, Appendix A. GPS,
// Galileo and QZSS week numbers use the GPS origin; BeiDou uses the BDT origin.
// Epochs are constellation time except GLONASS, whose record epoch is UTC.
type orbit struct {
	Name   string
	GNSS   gnss.GNSSID
	Epoch  time.Time // absolute UTC toe/tb, not the clock epoch
	Kepler kepler.Ephemeris
	GLO    glonass.Ephemeris
}

var systems = map[byte]gnss.GNSSID{'G': gnss.GPS, 'E': gnss.Galileo, 'C': gnss.BeiDou, 'J': gnss.QZSS, 'R': gnss.GLONASS}

func parseRINEX(r io.Reader, leapSeconds int) (map[string]orbit, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024), 4096)
	if !sc.Scan() || len(sc.Text()) < 21 {
		return nil, fmt.Errorf("missing RINEX header")
	}
	first := sc.Text()
	version, err := strconv.ParseFloat(strings.TrimSpace(first[:9]), 64)
	if err != nil || version < 3 || version >= 4 || first[20] != 'N' {
		return nil, fmt.Errorf("expected RINEX 3 navigation data")
	}
	headerDone := false
	for n := 0; n < 1000 && sc.Scan(); n++ {
		if strings.Contains(sc.Text(), "END OF HEADER") {
			headerDone = true
			break
		}
	}
	if !headerDone {
		return nil, fmt.Errorf("unfinished RINEX header")
	}
	out := map[string]orbit{}
	var lines []string
	flush := func() error {
		if len(lines) == 0 {
			return nil
		}
		o, supported, err := parseRecord(lines, leapSeconds)
		if err != nil {
			return err
		}
		if supported {
			if prev, ok := out[o.Name]; !ok || o.Epoch.After(prev.Epoch) {
				out[o.Name] = o
			}
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] != ' ' {
			if err := flush(); err != nil {
				return nil, err
			}
			lines = nil
		}
		lines = append(lines, line)
		if len(lines) > 9 {
			return nil, fmt.Errorf("oversized RINEX record")
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("reference has no supported satellites")
	}
	return out, nil
}

func parseRecord(lines []string, leapSeconds int) (orbit, bool, error) {
	var o orbit
	first := lines[0]
	if len(first) < 23 {
		return o, false, fmt.Errorf("short RINEX epoch")
	}
	g, supported := systems[first[0]]
	if !supported {
		// SBAS and NavIC are explicitly outside this map's supported scope.
		if (first[0] == 'S' && len(lines) == 4) || (first[0] == 'I' && len(lines) == 8) {
			return o, false, nil
		}
		return o, false, fmt.Errorf("unsupported RINEX record %q", first[:3])
	}
	n, err := strconv.Atoi(first[1:3])
	if err != nil || n < 1 || n > map[gnss.GNSSID]int{gnss.GPS: 32, gnss.Galileo: 36, gnss.BeiDou: 63, gnss.QZSS: 10, gnss.GLONASS: 24}[g] {
		// R25..R27 experimental GLONASS slots can appear in reference files;
		// the collector cannot decode them, so do not claim monitoring support.
		if g == gnss.GLONASS && n >= 25 && n <= 27 {
			return o, false, nil
		}
		return o, false, fmt.Errorf("invalid RINEX satellite %q", first[:3])
	}
	if (g != gnss.GLONASS && len(lines) != 8) || (g == gnss.GLONASS && len(lines) != 4 && len(lines) != 5) {
		return o, false, fmt.Errorf("incomplete RINEX record %s", first[:3])
	}
	var year, month, day, hour, minute, second int
	if _, err := fmt.Sscanf(first[3:23], "%d %d %d %d %d %d", &year, &month, &day, &hour, &minute, &second); err != nil {
		return o, false, fmt.Errorf("invalid RINEX epoch: %w", err)
	}
	epoch := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC)
	if year < 1980 || year > 2200 || int(epoch.Month()) != month || epoch.Day() != day || hour > 23 || minute > 59 || second > 59 || hour < 0 || minute < 0 || second < 0 {
		return o, false, fmt.Errorf("invalid RINEX calendar")
	}
	var f []float64
	var present []bool
	for i, line := range lines {
		base, count := 4, 4
		if i == 0 {
			base, count = 23, 3
		}
		for j := 0; j < count; j++ {
			start := base + 19*j
			v, have := 0.0, false
			if start < len(line) {
				end := min(start+19, len(line))
				s := strings.TrimSpace(line[start:end])
				if s != "" {
					have = true
					v, err = strconv.ParseFloat(strings.NewReplacer("D", "e", "d", "e").Replace(s), 64)
					if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
						return o, false, fmt.Errorf("invalid RINEX numeric field in %s", first[:3])
					}
				}
			}
			f, present = append(f, v), append(present, have)
		}
	}
	o = orbit{Name: first[:3], GNSS: g, Epoch: epoch}
	if g == gnss.GLONASS {
		for _, i := range []int{3, 4, 5, 7, 8, 9, 11, 12, 13} {
			if !present[i] {
				return o, false, fmt.Errorf("missing GLONASS state field")
			}
		}
		o.GLO = glonass.Ephemeris{X: f[3], Vx: f[4], Ax: f[5], Y: f[7], Vy: f[8], Ay: f[9], Z: f[11], Vz: f[12], Az: f[13], TodKnown: true}
	} else {
		for _, i := range []int{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 21} {
			if !present[i] {
				return o, false, fmt.Errorf("missing Kepler field in %s", o.Name)
			}
		}
		if f[21] != math.Trunc(f[21]) || f[21] < 0 || f[21] > 12000 || f[11] < 0 || f[11] >= 604800 {
			return o, false, fmt.Errorf("invalid RINEX week/toe")
		}
		origin, offset := time.Date(1980, 1, 6, 0, 0, 0, 0, time.UTC), leapSeconds
		if g == gnss.BeiDou {
			origin, offset = time.Date(2006, 1, 1, 0, 0, 0, 0, time.UTC), leapSeconds-14
		}
		o.Epoch = origin.Add(time.Duration((f[21]*604800 + f[11] - float64(offset)) * float64(time.Second)))
		if math.Abs(o.Epoch.Sub(epoch.Add(-time.Duration(offset)*time.Second)).Hours()) > 24 {
			return o, false, fmt.Errorf("inconsistent RINEX week/epoch in %s", o.Name)
		}
		o.Kepler = kepler.Ephemeris{ID: g, SVID: n, SqrtA: f[10], Ecc: f[8], M0: f[6], DeltaN: f[5], I0: f[15], IDot: f[19], Omega0: f[13], OmegaDot: f[18], Omega: f[17], Cuc: f[7], Cus: f[9], Crc: f[16], Crs: f[4], Cic: f[12], Cis: f[14], Toe: f[11]}
	}
	if _, ok := o.position(o.Epoch); !ok {
		return o, false, fmt.Errorf("invalid orbit for %s", o.Name)
	}
	return o, true, nil
}

func (o orbit) position(at time.Time) (gnss.ECEF, bool) {
	age := at.Sub(o.Epoch)
	var p gnss.ECEF
	var err error
	if o.GNSS == gnss.GLONASS {
		// Match the collector's deliberately short GLONASS fit window.
		if age < -15*time.Minute || age > 30*time.Minute {
			return p, false
		}
		p, err = glonass.Propagate(o.GLO, age.Seconds())
	} else {
		if age < -2*time.Hour || age > 4*time.Hour {
			return p, false
		}
		p, err = kepler.Propagate(o.Kepler, math.Mod(o.Kepler.Toe+age.Seconds()+gnsstime.WeekSeconds, gnsstime.WeekSeconds))
	}
	radius := p.Norm()
	return p, err == nil && !math.IsNaN(radius) && radius > 2e7 && radius < 5e7
}
