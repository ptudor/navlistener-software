package ingest

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden telemetry fixtures")

// solutionGoldenPath is the shared case every implementation of telemetry 0x03 is
// checked against: this codec, common/receiver_solution.h in the ESP32 host tests,
// and the C feeder's test.
const solutionGoldenPath = "../../../testdata/receiver_solution_v1.txt"

func goldenPayloads() [][2]string {
	return [][2]string{
		{"nav-pvt", hex.EncodeToString(ubxPVT(345_600_000, 374219000, -1220841000))},
		{"nav-clock", hex.EncodeToString(ubxClock(345_600_000, -412_345, 87))},
		{"nav-status", hex.EncodeToString(ubxStatus(345_600_000, 2))},
	}
}

func readGolden(t *testing.T) map[string][]byte {
	t.Helper()
	return readGoldenFile(t, solutionGoldenPath)
}

// readGoldenFile reads a golden fixture of "name hex" lines.
func readGoldenFile(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][]byte{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, " ")
		b, err := hex.DecodeString(value)
		if !ok || err != nil {
			t.Fatalf("bad golden line %q", line)
		}
		out[name] = b
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestReceiverSolutionGolden assembles the golden UBX epoch and requires the exact
// golden body, then decodes it back. Run with -update to rewrite the fixture.
func TestReceiverSolutionGolden(t *testing.T) {
	var stream []byte
	ids := map[string]byte{"nav-pvt": ubxIDNAVPVT, "nav-clock": ubxIDNAVCLOCK, "nav-status": ubxIDNAVSTATUS}
	payloads := goldenPayloads()
	if !*updateGolden {
		g := readGolden(t)
		for i, p := range payloads {
			if got := hex.EncodeToString(g[p[0]]); got != p[1] {
				t.Fatalf("golden %s differs from the test's payload:\n%s\n%s", p[0], got, p[1])
			}
			payloads[i][1] = hex.EncodeToString(g[p[0]])
		}
	}
	for _, p := range payloads {
		b, _ := hex.DecodeString(p[1])
		stream = append(stream, ubxMsg(ubxClassNAV, ids[p[0]], b)...)
	}
	stream = append(stream, ubxMsg(ubxClassNAV, ubxIDNAVEOE, ubxEOE(345_600_000))...)
	frames, errs := scanSolutions(t, stream)
	if len(frames) != 1 || len(errs) != 0 {
		t.Fatalf("frames %d errs %v", len(frames), errs)
	}
	body := EncodeReceiverSolution(frames[0].Solution)
	if *updateGolden {
		var buf bytes.Buffer
		buf.WriteString("# Telemetry 0x03 (receiver solution) golden case, body version 1\n")
		buf.WriteString("# (docs/proposals/STATION-ASSURANCE.md §2). The three UBX payloads are one\n")
		buf.WriteString("# epoch, little-endian as the receiver sends them, with bits the record drops\n")
		buf.WriteString("# set (validMag, psmState, headVehValid, correction age). body is the record\n")
		buf.WriteString("# body every implementation must produce from them. Regenerate with\n")
		buf.WriteString("#   go test ./internal/ingest -run TestReceiverSolutionGolden -update\n")
		for _, p := range payloads {
			fmt.Fprintf(&buf, "%s %s\n", p[0], p[1])
		}
		fmt.Fprintf(&buf, "body %s\n", hex.EncodeToString(body))
		if err := os.WriteFile(solutionGoldenPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want := readGolden(t)["body"]
	if !bytes.Equal(body, want) {
		t.Fatalf("encoded body differs from golden:\n got %x\nwant %x", body, want)
	}
	decoded, err := decodeReceiverSolution(want)
	if err != nil || decoded.Status.SpoofState != 2 || decoded.PVT.PosFlags != PosFlagInvalidLLH || decoded.Clock.BiasNS != -412_345 {
		t.Fatalf("golden body decodes to %+v, %v", decoded, err)
	}
}
