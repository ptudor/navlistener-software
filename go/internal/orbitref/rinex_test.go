package orbitref

import (
	"compress/gzip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/gnss"
)

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../../gnss/testdata/BRDC00WRD_R_20240100000_truth.rnx")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Independent ESA SP3 coordinates from the existing truth fixture. This pins
// parsing, SI units, GPS/GST/BDT/UTC epochs and propagation together.
func TestRINEXAgainstPreciseOrbits(t *testing.T) {
	orbits, err := parseRINEX(strings.NewReader(fixture(t)), 18)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		hour      int
		want      gnss.ECEF
		tolerance float64
	}{
		{"G05", 12, gnss.ECEF{X: 14412808.052, Y: 5559840.190, Z: -21791820.530}, 8},
		{"E21", 12, gnss.ECEF{X: 6297622.492, Y: 17467415.185, Z: 23047739.370}, 8},
		{"C11", 12, gnss.ECEF{X: 23058546.809, Y: 15469428.441, Z: -2758626.850}, 10},
		{"J02", 9, gnss.ECEF{X: -31133494.974, Y: 22980554.534, Z: 21800813.561}, 12},
		{"R09", 12, gnss.ECEF{X: 5830691.877, Y: -24858159.659, Z: -936999.923}, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Date(2024, 1, 10, tc.hour, 30, 0, 0, time.UTC).Add(-18 * time.Second)
			p, ok := orbits[tc.name].position(at)
			if !ok || p.Sub(tc.want).Norm() > tc.tolerance {
				t.Fatalf("position %v ok=%v miss=%.2fm", p, ok, p.Sub(tc.want).Norm())
			}
		})
	}
}

func TestRINEXRejectsBrokenRecords(t *testing.T) {
	good := fixture(t)
	for _, bad := range []string{
		strings.Replace(good, "3.05", "2.11", 1),
		good[:len(good)-100],
		strings.Replace(good, "5.153794351578e+03", "NaN              ", 1),
		strings.Replace(good, "5.153794351578e+03", "                 ", 1),
		strings.Replace(good, "2.296000000000e+03", "1.296000000000e+03", 1),
		strings.Replace(good, "2024 01 10 12", "2024 02 31 12", 1),
	} {
		if _, err := parseRINEX(strings.NewReader(bad), 18); err == nil {
			t.Fatal("accepted corrupt RINEX")
		}
	}
}

func TestOrbitExpiryRetainsRoster(t *testing.T) {
	orbits, err := parseRINEX(strings.NewReader(fixture(t)), 18)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2024, 1, 10, 12, 30, 0, 0, time.UTC)
	c := New("", 18)
	c.data = diskCatalogue{Orbits: orbits, FetchedAt: at}
	// Past every coast limit, identities remain without coordinates.
	snapshot := c.Snapshot(at.Add(80 * time.Hour))
	if snapshot.Status != "delayed" || len(snapshot.Satellites) != 5 {
		t.Fatalf("lost expected roster: %+v", snapshot)
	}
	for _, s := range snapshot.Satellites {
		if s.Position != nil || s.Extrapolated {
			t.Fatalf("expired orbit still located: %s", s.Name)
		}
	}
	if _, ok := orbits["G05"].position(at.Add(7 * 24 * time.Hour)); ok {
		t.Fatal("week-wrapped stale orbit accepted")
	}
	if _, ok := orbits["G05"].extrapolate(at.Add(7 * 24 * time.Hour)); ok {
		t.Fatal("week-wrapped stale orbit extrapolated")
	}
}

func TestDelayedReferenceCoastsOnLastOrbits(t *testing.T) {
	orbits, err := parseRINEX(strings.NewReader(fixture(t)), 18)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2024, 1, 10, 12, 30, 0, 0, time.UTC)
	c := New("", 18)
	c.data = diskCatalogue{Orbits: orbits, FetchedAt: at}
	located := func(now time.Time) map[string]bool {
		out := map[string]bool{}
		for _, s := range c.Snapshot(now).Satellites {
			if (s.Position != nil) != s.Extrapolated && now.Sub(orbits[s.Name].Epoch) > 4*time.Hour {
				t.Fatalf("%s: position %v extrapolated=%v", s.Name, s.Position, s.Extrapolated)
			}
			out[s.Name] = s.Position != nil
		}
		return out
	}
	// Every orbit is past its fit window, but a download outage keeps the
	// last orbits on the map instead of erasing every known gap.
	if got := located(at.Add(40 * time.Hour)); len(got) != 5 || !got["G05"] || !got["E21"] || !got["C11"] || !got["J02"] || !got["R09"] {
		t.Fatalf("delayed reference dropped coastable orbits: %v", got)
	}
	// GLONASS integration stops at two days; Kepler orbits coast to three.
	if got := located(at.Add(60 * time.Hour)); got["R09"] || !got["G05"] || !got["C11"] {
		t.Fatalf("per-system coast limits not applied: %v", got)
	}
	// Inside the fit window a delayed reference still reports a current orbit.
	for _, s := range c.Snapshot(at).Satellites {
		if s.Name == "G05" && (s.Position == nil || s.Extrapolated) {
			t.Fatalf("in-window orbit marked extrapolated: %+v", s)
		}
	}
	// A current reference never extrapolates an orbit it has stopped refreshing.
	later := at.Add(40 * time.Hour)
	c.data.FetchedAt = later
	snapshot := c.Snapshot(later)
	if snapshot.Status != "current" {
		t.Fatalf("status %q", snapshot.Status)
	}
	for _, s := range snapshot.Satellites {
		if s.Position != nil || s.Extrapolated {
			t.Fatalf("current reference extrapolated %s", s.Name)
		}
	}
	// Coasting only runs forward from the orbit epoch.
	if _, ok := orbits["G05"].extrapolate(orbits["G05"].Epoch.Add(-3 * time.Hour)); ok {
		t.Fatal("extrapolated backwards past the fit window")
	}
}

func TestRINEXSkipsGLONASSSlotsAboveTwentyFour(t *testing.T) {
	good := fixture(t)
	record := good[strings.Index(good, "R09 2024"):]
	extra := good + strings.Replace(record, "R09", "R27", 1) + strings.Replace(record, "R09", "R30", 1)
	orbits, err := parseRINEX(strings.NewReader(extra), 18)
	if err != nil {
		t.Fatal(err)
	}
	if len(orbits) != 5 || orbits["R09"].Name != "R09" {
		t.Fatalf("unexpected roster: %d orbits", len(orbits))
	}
	if _, err := parseRINEX(strings.NewReader(good+strings.Replace(record, "R09", "R00", 1)), 18); err == nil {
		t.Fatal("accepted GLONASS slot zero")
	}
}

func TestDownloadedReference(t *testing.T) {
	path := os.Getenv("NAVLISTEN_TEST_RINEX_GZ")
	if path == "" {
		t.Skip("optional downloaded reference validation")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	orbits, err := parseRINEX(z, 18)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[gnss.GNSSID]int{}
	for _, o := range orbits {
		counts[o.GNSS]++
	}
	for _, g := range systems {
		if counts[g] == 0 {
			t.Fatalf("system %d missing", g)
		}
	}
	t.Logf("validated %d reference satellites: %v", len(orbits), counts)
}
