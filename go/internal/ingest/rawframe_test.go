package ingest

import (
	"fmt"
	"testing"

	"github.com/ptudor/gnss"
)

// TestNavTypeMatchesConstellationsTable guards NavType must match every
// (gnssId, sigId) -> GNF1 type byte row in docs/CONSTELLATIONS.md §6's u-blox
// table (lines 108-128) and §6.1's byte table (lines 355-377). In particular
// BeiDou sigId 5/6 (B1C, B-CNAV1) must map to 0x32, not fall through to 0x30
// (BdsD1) as it did before this fix.
func TestNavTypeMatchesConstellationsTable(t *testing.T) {
	cases := []struct {
		gnssID gnss.GNSSID
		sigID  int
		want   int
		name   string
	}{
		{gnss.GPS, 0, 0x10, "GpsLnav"},
		{gnss.GPS, 3, 0x11, "GpsCnav"},
		{gnss.GPS, 4, 0x11, "GpsCnav"},
		{gnss.GPS, 6, 0x11, "GpsCnav"},
		{gnss.GPS, 7, 0x11, "GpsCnav"},
		{gnss.Galileo, 0, 0x20, "GalInav"},
		{gnss.Galileo, 1, 0x20, "GalInav"},
		{gnss.Galileo, 3, 0x21, "GalFnav"},
		{gnss.Galileo, 4, 0x21, "GalFnav"},
		{gnss.Galileo, 5, 0x20, "GalInav"},
		{gnss.Galileo, 6, 0x20, "GalInav"},
		{gnss.BeiDou, 0, 0x30, "BdsD1"},
		{gnss.BeiDou, 1, 0x31, "BdsD2"},
		{gnss.BeiDou, 2, 0x30, "BdsD1"},
		{gnss.BeiDou, 3, 0x31, "BdsD2"},
		{gnss.BeiDou, 5, 0x32, "BdsCnav1"},
		{gnss.BeiDou, 6, 0x32, "BdsCnav1"},
		{gnss.BeiDou, 7, 0x33, "BdsCnav2"},
		{gnss.BeiDou, 8, 0x33, "BdsCnav2"},
		{gnss.QZSS, 0, 0x50, "QzsLnav"},
		{gnss.QZSS, 1, 0x53, "QzsL1s"},
		{gnss.QZSS, 4, 0x51, "QzsCnav"},
		{gnss.QZSS, 5, 0x51, "QzsCnav"},
		{gnss.QZSS, 8, 0x51, "QzsCnav"},
		{gnss.QZSS, 9, 0x51, "QzsCnav"},
		{gnss.GLONASS, 0, 0x40, "GloNav"},
		{gnss.GLONASS, 2, 0x40, "GloNav"},
		{gnss.NavIC, 0, 0x60, "NavicNav"},
		{gnss.SBAS, 0, 0x70, "SbasL1"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/sig%d/%s", c.gnssID, c.sigID, c.name), func(t *testing.T) {
			f := &RawFrame{GnssID: c.gnssID, SigID: c.sigID}
			if got := f.NavType(); got != c.want {
				t.Errorf("NavType() = 0x%02x, want 0x%02x (%s)", got, c.want, c.name)
			}
		})
	}
}
