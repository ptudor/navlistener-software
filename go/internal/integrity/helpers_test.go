package integrity

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/geo"
	"github.com/ptudor/gnss/physconst"
)

// site is a surveyed antenna used across the tests.
var site = Surveyed{LatDeg: 37.4219, LonDeg: -122.0841, HeightM: 12.5}

func fixedProfile() StationProfile {
	pos := site
	return StationProfile{Mode: ModeFixed, Position: &pos}
}

// offsetLLH returns the geodetic coordinates of a point displaced east/north/up
// metres from the surveyed site.
func offsetLLH(east, north, up float64) (lat, lon, h float64) {
	ref := geo.Geodetic{Lat: geo.Rad(site.LatDeg), Lon: geo.Rad(site.LonDeg), Height: site.HeightM}
	e, n, u := geo.ENUBasis(ref)
	p := geo.GeodeticToECEF(ref, physconst.WGS84).Add(e.Scale(east)).Add(n.Scale(north)).Add(u.Scale(up))
	g := geo.ECEFToGeodetic(p, physconst.WGS84)
	return geo.Deg(g.Lat), geo.Deg(g.Lon), g.Height
}

// solutionAt builds a valid 3D solution displaced east/north/up from the site with
// the given north/east/down velocity, one epoch per second from TOW tow0.
func solutionAt(epoch int, east, north, up, vn, ve, vd float64) Solution {
	lat, lon, h := offsetLLH(east, north, up)
	recv := t0.Add(time.Duration(epoch) * time.Second)
	return Solution{
		Received: recv, TOW: uint32(tow0 + epoch*1000), UTC: recv.Add(-300 * time.Millisecond), UTCValid: true,
		HostStamp: recv, FixType: Fix3D, FixOK: true, NumSV: 12,
		LatDeg: lat, LonDeg: lon, HeightM: h, HAccM: 1.5, VAccM: 2.5,
		VelN: vn, VelE: ve, VelD: vd, SAccMPS: 0.1,
	}
}

const tow0 = 345_600_000 // Wednesday 00:00 GPS time

func mustStation(t *testing.T, sp StationProfile) *Station {
	t.Helper()
	s, err := NewStation(DefaultProfile(), sp)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func checkResult(t *testing.T, a Assessment, name string) Result {
	t.Helper()
	for _, r := range a.Checks {
		if r.Check == name {
			return r
		}
	}
	t.Fatalf("assessment has no %s check: %+v", name, a.Checks)
	return Result{}
}

// ecefOf returns the ECEF position of a solution, for distance assertions.
func ecefOf(s Solution) gnss.ECEF { return s.ecef() }
