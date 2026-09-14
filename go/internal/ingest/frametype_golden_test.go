package ingest

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ptudor/gnss"
)

// frameTypeGolden is the checked-in (gnssId, sigId) → frame_type matrix: the SINGLE
// authority for a mapping that three implementations must agree on — Go's
// RawFrame.NavType (here), the C feeder's frame_type() (feeder/navfeeder.c), and the
// ESP32's gnf1_frame_type() (esp32/components/gnf1/src/gnf1.c). Before this fixture
// existed the three had drifted pairwise: the C feeder labelled every SBAS sigId 0x70
// where Go reserves L5/DFMC, and the ESP32 applied broad GPS/Galileo/GLONASS/SBAS family
// fallbacks. The byte is forensic metadata (the collector dispatches decode on
// (gnssId, sigId)), but the historian's provenance is a product promise: the same
// unsupported signal must not be persisted under different msg_type values depending on
// which feeder hardware saw it.
//
// Regenerate after an intentional NavType change: UPDATE_GOLDEN=1 go test ./internal/ingest
// -run TestNavTypeMatchesGolden — then re-run the C and ESP32 differential tests, which
// read this same file.
const frameTypeGolden = "../../../testdata/gnf1_frame_type.tsv"

// goldenGnssIDs/goldenSigIDs bound the enumerated matrix: every u-blox constellation id
// (0–7, docs/CONSTELLATIONS.md §0) crossed with sigId 0–15. u-blox sigId is a byte, but no
// assigned signal exceeds 15 on any current receiver; values outside both ranges are
// covered by rule instead of by row (TestNavTypeOutOfMatrixRange).
const (
	goldenGnssIDs = 8
	goldenSigIDs  = 16
)

// frameTypeNames labels the golden rows for a human reader. Names are the registry names in
// docs/CONSTELLATIONS.md §6.1; "-" means "no mapping" (unsupported or deliberately reserved).
var frameTypeNames = map[int]string{
	0x10: "GpsLnav", 0x11: "GpsCnav",
	0x20: "GalInav", 0x21: "GalFnav",
	0x30: "BdsD1", 0x33: "BdsCnav2",
	0x40: "GloNav",
	0x50: "QzsLnav", 0x51: "QzsCnav",
	0x70: "SbasL1",
}

type goldenRow struct {
	gnssID, sigID, frameType int
	name                     string
}

// TestNavTypeMatchesGolden pins Go's NavType — the canonical mapping, and the only one of
// the three with the full citation trail — against the checked-in
// matrix. It fails if NavType changed without the fixture being regenerated, which is the
// event that must force the C feeder and ESP32 mappers to be revisited.
func TestNavTypeMatchesGolden(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		writeFrameTypeGolden(t)
	}
	rows := readFrameTypeGolden(t)

	if len(rows) != goldenGnssIDs*goldenSigIDs {
		t.Fatalf("golden has %d rows, want %d (%d gnssId × %d sigId)",
			len(rows), goldenGnssIDs*goldenSigIDs, goldenGnssIDs, goldenSigIDs)
	}
	seen := make(map[[2]int]bool, len(rows))
	for _, r := range rows {
		key := [2]int{r.gnssID, r.sigID}
		if seen[key] {
			t.Errorf("golden has duplicate row for (gnssId=%d, sigId=%d)", r.gnssID, r.sigID)
		}
		seen[key] = true

		f := &RawFrame{GnssID: gnss.GNSSID(r.gnssID), SigID: r.sigID}
		if got := f.NavType(); got != r.frameType {
			t.Errorf("NavType(gnssId=%d, sigId=%d) = 0x%02x, golden says 0x%02x",
				r.gnssID, r.sigID, got, r.frameType)
		}
		if want := goldenName(r.frameType); r.name != want {
			t.Errorf("golden (gnssId=%d, sigId=%d) name %q, want %q", r.gnssID, r.sigID, r.name, want)
		}
	}
	for g := 0; g < goldenGnssIDs; g++ {
		for s := 0; s < goldenSigIDs; s++ {
			if !seen[[2]int{g, s}] {
				t.Errorf("golden is missing row (gnssId=%d, sigId=%d)", g, s)
			}
		}
	}
}

// TestNavTypeOutOfMatrixRange covers by rule what the matrix cannot cover by row: every
// gnssId above the u-blox range and every sigId above 15 must map to 0 (no forensic label),
// not to a constellation-family default. This is the exact rule the ESP32 mapper broke.
func TestNavTypeOutOfMatrixRange(t *testing.T) {
	for s := goldenSigIDs; s <= 255; s++ {
		for g := 0; g < goldenGnssIDs; g++ {
			f := &RawFrame{GnssID: gnss.GNSSID(g), SigID: s}
			if got := f.NavType(); got != 0 {
				t.Fatalf("NavType(gnssId=%d, sigId=%d) = 0x%02x, want 0 (unassigned signal)", g, s, got)
			}
		}
	}
	for g := goldenGnssIDs; g <= 255; g++ {
		for s := 0; s < goldenSigIDs; s++ {
			f := &RawFrame{GnssID: gnss.GNSSID(g), SigID: s}
			if got := f.NavType(); got != 0 {
				t.Fatalf("NavType(gnssId=%d, sigId=%d) = 0x%02x, want 0 (unassigned constellation)", g, s, got)
			}
		}
	}
}

// TestESP32Gnf1FrameTypeMatchesGolden is the ESP32 leg of the regression fix differential test. The
// gnf1 is pure buffer code with no ESP-IDF dependency and can be tested on the host.
// the system cc and runs it — which means a drifting ESP32 mapper fails `make check` on a
// machine with no ESP-IDF, no board, and no flash. The C source's own assertion that it is
// "byte-for-byte matched to navfeeder.c" is thereby enforced rather than asserted; it was
// false when this test was written (broad GPS/Galileo/GLONASS/SBAS family fallbacks).
func TestESP32Gnf1FrameTypeMatchesGolden(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler; skipping ESP32 gnf1 host test")
	}
	const comp = "../../../esp32/components/gnf1"
	bin := filepath.Join(t.TempDir(), "frame_type_matrix_test")
	build := exec.Command(cc, "-O1", "-Wall", "-Wextra", "-std=c11",
		"-I"+filepath.Join(comp, "include"), "-o", bin,
		filepath.Join(comp, "test", "frame_type_matrix_test.c"),
		filepath.Join(comp, "src", "gnf1.c"))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compiling the esp32 gnf1 host test failed: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, frameTypeGolden).CombinedOutput()
	if err != nil {
		t.Fatalf("esp32 gnf1_frame_type disagrees with %s: %v\n%s", frameTypeGolden, err, out)
	}
	t.Logf("%s", bytes.TrimSpace(out))
}

func goldenName(frameType int) string {
	if n, ok := frameTypeNames[frameType]; ok {
		return n
	}
	return "-"
}

// readFrameTypeGolden parses the fixture: tab-separated gnssId, sigId, frame_type (0xNN),
// name. '#' comments and blank lines are skipped — the same shape the C readers parse.
func readFrameTypeGolden(t *testing.T) []goldenRow {
	t.Helper()
	f, err := os.Open(frameTypeGolden)
	if err != nil {
		t.Fatalf("open golden: %v", err)
	}
	defer f.Close()

	var rows []goldenRow
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, "\t")
		if len(fields) < 4 {
			t.Fatalf("%s:%d: want 4 tab-separated fields, got %d", frameTypeGolden, line, len(fields))
		}
		g, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatalf("%s:%d: gnssId: %v", frameTypeGolden, line, err)
		}
		s, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("%s:%d: sigId: %v", frameTypeGolden, line, err)
		}
		ft, err := strconv.ParseUint(strings.TrimPrefix(fields[2], "0x"), 16, 8)
		if err != nil {
			t.Fatalf("%s:%d: frame_type: %v", frameTypeGolden, line, err)
		}
		rows = append(rows, goldenRow{gnssID: g, sigID: s, frameType: int(ft), name: fields[3]})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return rows
}

// writeFrameTypeGolden regenerates the fixture from NavType (UPDATE_GOLDEN=1). Go is the
// canonical mapping, so the file is generated, never hand-edited.
func writeFrameTypeGolden(t *testing.T) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`# navlistener — GNF1 frame_type golden matrix (generated; do not hand-edit)
#
# The single authority for the (gnssId, sigId) -> frame_type byte carried in every GNF1
# raw-nav record (docs/CONSTELLATIONS.md §6.1). Three implementations must agree on it:
#
#   go/internal/ingest/rawframe.go   RawFrame.NavType()      <- CANONICAL, generates this file
#   feeder/navfeeder.c               frame_type()
#   esp32/components/gnf1/src/gnf1.c gnf1_frame_type()
#
# frame_type is forensic metadata, not the decode dispatch key (the collector dispatches on
# (gnssId, sigId) regardless), but the historian's provenance depends on one input population
# never being split by which feeder saw it — so all three are differentially tested against
# these rows.
#
# 0x00 means "no mapping": an unsupported, unassigned, or deliberately reserved signal. Never
# widen a row to a constellation-family default — an unverified label persists a frame that
# will be re-decoded through the wrong layout on replay.
#
# Regenerate:  UPDATE_GOLDEN=1 go test ./internal/ingest -run TestNavTypeMatchesGolden
#
# gnssId	sigId	frame_type	name
`)
	for g := 0; g < goldenGnssIDs; g++ {
		for s := 0; s < goldenSigIDs; s++ {
			f := &RawFrame{GnssID: gnss.GNSSID(g), SigID: s}
			ft := f.NavType()
			fmt.Fprintf(&b, "%d\t%d\t0x%02x\t%s\n", g, s, ft, goldenName(ft))
		}
	}
	if err := os.MkdirAll(filepath.Dir(frameTypeGolden), 0o755); err != nil {
		t.Fatalf("mkdir golden dir: %v", err)
	}
	if err := os.WriteFile(frameTypeGolden, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
	t.Logf("regenerated %s", frameTypeGolden)
}
