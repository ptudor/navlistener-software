package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptudor/navlistener/internal/integrity"
)

func loadBody(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "navlistener.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

const receptionSite = `
[[reception.station]]
observer = "roof"
position = [37.4219, -122.0841, 12.5]
signals = ["0:0"]
`

func TestIntegrityStationsResolved(t *testing.T) {
	cfg, err := loadBody(t, receptionSite+`
[[integrity.station]]
observer = "van"
mode = "mobile"
max_speed_mps = 35

[[integrity.station]]
observer = "mast"
mode = "fixed"
position = [-33.86, 151.21, 40]

[[integrity.station]]
observer = "shed"
mode = "fixed"
`)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.IntegrityStations()
	want := map[string]integrity.StationProfile{
		"roof": {Mode: integrity.ModeFixed, Position: &integrity.Surveyed{LatDeg: 37.4219, LonDeg: -122.0841, HeightM: 12.5}},
		"van":  {Mode: integrity.ModeMobile, MaxSpeedMPS: 35},
		"mast": {Mode: integrity.ModeFixed, Position: &integrity.Surveyed{LatDeg: -33.86, LonDeg: 151.21, HeightM: 40}},
		"shed": {Mode: integrity.ModeFixed},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d installations, want %d: %+v", len(got), len(want), got)
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok || g.Mode != w.Mode || g.MaxSpeedMPS != w.MaxSpeedMPS || (g.Position == nil) != (w.Position == nil) ||
			(g.Position != nil && *g.Position != *w.Position) {
			t.Errorf("%s = %+v, want %+v", id, g, w)
		}
	}
	// The accessor returns a copy.
	got["roof"].Position.LatDeg = 0
	if cfg.IntegrityStations()["roof"].Position.LatDeg != 37.4219 {
		t.Fatal("IntegrityStations exposes the resolved configuration")
	}
}

func TestIntegrityStationSharesReceptionPosition(t *testing.T) {
	cfg, err := loadBody(t, receptionSite+`
[[integrity.station]]
observer = "roof"
mode = "fixed"
position = [37.4219, -122.0841, 12.5]
`)
	if err != nil {
		t.Fatalf("identical positions rejected: %v", err)
	}
	if p := cfg.IntegrityStations()["roof"].Position; p == nil || p.HeightM != 12.5 {
		t.Fatalf("roof = %+v", p)
	}
}

func TestIntegrityStationRejects(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"mode":           {"[[integrity.station]]\nobserver = \"a\"\nmode = \"airborne\"\n", "mode must be"},
		"missing mode":   {"[[integrity.station]]\nobserver = \"a\"\n", "mode must be"},
		"observer":       {"[[integrity.station]]\nobserver = \"a b\"\nmode = \"fixed\"\n", "invalid observer"},
		"duplicate":      {"[[integrity.station]]\nobserver = \"a\"\nmode = \"fixed\"\n[[integrity.station]]\nobserver = \"a\"\nmode = \"fixed\"\n", "duplicate"},
		"fixed speed":    {"[[integrity.station]]\nobserver = \"a\"\nmode = \"fixed\"\nmax_speed_mps = 5\n", "only to a mobile"},
		"mobile speed":   {"[[integrity.station]]\nobserver = \"a\"\nmode = \"mobile\"\n", "needs max_speed_mps"},
		"mobile survey":  {"[[integrity.station]]\nobserver = \"a\"\nmode = \"mobile\"\nmax_speed_mps = 5\nposition = [1, 2, 3]\n", "no surveyed position"},
		"short position": {"[[integrity.station]]\nobserver = \"a\"\nmode = \"fixed\"\nposition = [1, 2]\n", "requires latitude"},
		"latitude":       {"[[integrity.station]]\nobserver = \"a\"\nmode = \"fixed\"\nposition = [91, 2, 3]\n", "out of range"},
		"two positions":  {receptionSite + "[[integrity.station]]\nobserver = \"roof\"\nmode = \"fixed\"\nposition = [37.4219, -122.0841, 13]\n", "in one place"},
		"mobile site":    {receptionSite + "[[integrity.station]]\nobserver = \"roof\"\nmode = \"mobile\"\nmax_speed_mps = 5\n", "fixed site"},
		"unknown field":  {"[[integrity.station]]\nobserver = \"a\"\nmode = \"fixed\"\nspeed = 5\n", "strict mode"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadBody(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}
