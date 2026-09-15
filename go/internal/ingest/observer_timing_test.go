package ingest

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"strings"
	"testing"
)

// Optional replay check for a hardware capture, without committing device logs.
func TestTimingSerialCapture(t *testing.T) {
	path := os.Getenv("NAVLISTEN_TIMING_LOG")
	if path == "" {
		t.Skip("set NAVLISTEN_TIMING_LOG to validate a serial timing capture")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	n := 0
	for scanner.Scan() {
		_, sample, ok := strings.Cut(scanner.Text(), "pulse_timing: sample=")
		if !ok {
			continue
		}
		b, err := hex.DecodeString(strings.TrimSpace(sample))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeObserverDetails(b); err != nil {
			t.Fatalf("hardware sample %d: %v", n, err)
		}
		n++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no timing samples")
	}
	t.Logf("validated %d hardware timing samples", n)
}

func timingGolden(t *testing.T) []byte {
	t.Helper()
	s, err := os.ReadFile("../../../testdata/observer_timing_v1.hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(s)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTimingSharedGolden(t *testing.T) {
	d, err := decodeObserverDetails(timingGolden(t))
	if err != nil {
		t.Fatal(err)
	}
	v := d.Timing
	if v == nil || d.Environment != nil || v.Clock != "esp_apb" || v.ElapsedMS != 999000 || v.RTCState != "enabled_1hz" {
		t.Fatalf("timing envelope: %+v", d)
	}
	if *v.PhaseNS != -1000 || *v.GNSS.PeriodNS != 1000001000 || *v.RTC.WidthNS != 500000000 ||
		math.Abs(*v.GNSS.PeriodErrorPPM-1) > 1e-8 || math.Abs(*v.RTC.PeriodErrorPPM-2) > 1e-8 ||
		v.GNSS.Physical != 999 || v.GNSS.Captured != 999 || *v.TimepulseFlags != 3 {
		t.Fatalf("timing units/counts: %+v", v)
	}
}

func TestTimingInvalidAndUnavailable(t *testing.T) {
	good := timingGolden(t)[27:]
	for n := 0; n < len(good); n++ {
		if _, err := decodeBoardTiming(good[:n], 1000000); err == nil {
			t.Fatalf("truncation %d", n)
		}
	}
	for name, mutate := range map[string]func([]byte){
		"clock":               func(b []byte) { b[1] = 2 },
		"reserved flags":      func(b []byte) { b[5] = 4 },
		"zero resolution":     func(b []byte) { clear(b[8:12]) },
		"future start":        func(b []byte) { binary.BigEndian.PutUint64(b[16:], 1000001) },
		"future edge":         func(b []byte) { binary.BigEndian.PutUint64(b[100:], 1000001) },
		"reserved channel":    func(b []byte) { b[115] = 1 },
		"ambiguous counter":   func(b []byte) { b[111] = 1 },
		"zero interval":       func(b []byte) { clear(b[40:44]) },
		"phase boundary":      func(b []byte) { binary.BigEndian.PutUint32(b[24:], 40000000) },
		"span without pulses": func(b []byte) { clear(b[68:76]) },
	} {
		t.Run(name, func(t *testing.T) {
			b := append([]byte(nil), good...)
			mutate(b)
			if _, err := decodeBoardTiming(b, 1000000); err == nil {
				t.Fatal("invalid sample accepted")
			}
		})
	}
	// Stale edges keep historical counts and spans, but provide no current
	// period, width, phase, or drift estimate.
	b := append([]byte(nil), good...)
	clear(b[5:8])
	clear(b[24:36])
	for _, p := range []int{36, 116} {
		binary.BigEndian.PutUint32(b[p:], 3)
		clear(b[p+4 : p+12])
	}
	d, err := decodeBoardTiming(b, 1000000)
	if err != nil || d.PhaseNS != nil || d.GNSS.PeriodNS != nil || d.GNSS.PeriodErrorPPM != nil || d.GNSS.Physical != 999 {
		t.Fatalf("stale: %+v %v", d, err)
	}
	// Capture initialization failure is a reportable unavailable state.
	b = make([]byte, 196)
	b[0], b[1] = 1, 1
	if _, err := decodeBoardTiming(b, 1000); err != nil {
		t.Fatal(err)
	}
}
