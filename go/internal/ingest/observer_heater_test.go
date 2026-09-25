package ingest

import (
	"encoding/json"
	"strings"
	"testing"
)

// The shared fixture's final component: a completed dry run, back in normal service.
const heaterOffset = 200

func TestObserverDetailsHumidityHeaterGolden(t *testing.T) {
	d, err := decodeObserverDetails(observerGolden(t))
	if err != nil {
		t.Fatal(err)
	}
	h := d.Heater
	if h == nil || h.State != "normal" || !h.TrustedUTC || !h.LastRunReadable || h.RunsSinceBoot != 1 ||
		h.AtLeast95Seconds != 20000 || h.AtLeast98Seconds != 14460 || h.CondensingS != 0 || *h.LastRunUnix != 1789400000 {
		t.Fatalf("heater: %+v", h)
	}
	r := h.LatestRun
	if r == nil || r.StartUptimeMS != 250000 || r.OnMS != 95000 || r.RecoveryMS != 630000 || *r.StopReason != "dry" ||
		*r.HumidityBefore != 99.12 || *r.HumidityAtStop != 4.3 || *r.HDCBeforeC != -1.5 || *r.HDCPeakC != 73.1 ||
		*r.HDCEndC != 13.1 || *r.MCPBeforeC != -2.1 || *r.MCPPeakC != 24.75 || *r.MCPEndC != 11.5 {
		t.Fatalf("run: %+v", r)
	}
	data, err := json.Marshal(d)
	if err != nil || !strings.Contains(string(data), `"humidity_heater":{"state":"normal"`) ||
		!strings.Contains(string(data), `"stop_reason":"dry"`) {
		t.Fatalf("json: %s", data)
	}
}

func TestObserverDetailsHumidityHeaterPhases(t *testing.T) {
	good := observerGolden(t)
	variant := func(edit func(v []byte)) []byte {
		b := append([]byte(nil), good...)
		edit(b[heaterOffset:])
		return b
	}
	clearEnd := func(v []byte) {
		v[41] &^= 16 | 128
		copy(v[50:52], []byte{0, 0})
		copy(v[56:58], []byte{0, 0})
	}
	// Heating: no stop reason, no recovery time, no end values; the on-time runs on.
	heating := variant(func(v []byte) { v[1] = 1; v[40] = 0; clearEnd(v); copy(v[36:40], []byte{0, 0, 0, 0}) })
	d, err := decodeObserverDetails(heating)
	if err != nil || d.Heater.State != "heating" || d.Heater.LatestRun.StopReason != nil || d.Heater.LatestRun.HDCEndC != nil {
		t.Fatalf("heating: %v %+v", err, d)
	}
	stopping := variant(func(v []byte) { v[1] = 2; clearEnd(v); copy(v[36:40], []byte{0, 0, 0, 0}) })
	if d, err = decodeObserverDetails(stopping); err != nil || d.Heater.State != "stopping" || *d.Heater.LatestRun.StopReason != "dry" {
		t.Fatalf("stopping: %v %+v", err, d)
	}
	recovering := variant(func(v []byte) { v[1] = 3; clearEnd(v) })
	if d, err = decodeObserverDetails(recovering); err != nil || d.Heater.State != "recovering" || d.Heater.LatestRun.MCPEndC != nil {
		t.Fatalf("recovering: %v %+v", err, d)
	}
	// No run this boot: the run fields are zero and omitted; a previous boot's run time remains.
	idle := variant(func(v []byte) {
		v[3] = 0
		for i := 24; i < 58; i++ {
			v[i] = 0
		}
		v[15] = 90
	})
	if d, err = decodeObserverDetails(idle); err != nil || d.Heater.LatestRun != nil || d.Heater.CondensingS != 90 || *d.Heater.LastRunUnix != 1789400000 {
		t.Fatalf("idle: %v %+v", err, d)
	}
	never := variant(func(v []byte) {
		v[2] = 1
		for i := 3; i < 58; i++ {
			if i < 4 || i >= 16 {
				v[i] = 0
			}
		}
	})
	if d, err = decodeObserverDetails(never); err != nil || d.Heater.LastRunUnix != nil || d.Heater.LastRunReadable {
		t.Fatalf("never: %v %+v", err, d)
	}
	// An unavailable MCP9808 leaves its values null.
	noMCP := variant(func(v []byte) {
		v[41] &^= 32 | 64 | 128
		for i := 52; i < 58; i++ {
			v[i] = 0
		}
	})
	if d, err = decodeObserverDetails(noMCP); err != nil || d.Heater.LatestRun.MCPPeakC != nil || d.Heater.LatestRun.HDCPeakC == nil {
		t.Fatalf("no MCP9808: %v %+v", err, d)
	}
}

func TestObserverDetailsHumidityHeaterInvalid(t *testing.T) {
	good := observerGolden(t)
	for _, change := range []struct {
		offset int
		value  byte
	}{
		{0, 2},     // version
		{1, 4},     // state
		{1, 1},     // heating with a stop reason
		{1, 3},     // recovering with end-of-recovery values
		{2, 1},     // unreadable last-run record, yet a run
		{2, 4},     // flags
		{8, 1},     // more time at 98 %RH than at 95 %RH
		{16, 16},   // last run outside 2000-2099
		{24, 1},    // run starts after the snapshot
		{37, 255},  // start + on + recovery after the snapshot
		{40, 6},    // stop reason
		{40, 0},    // completed run without a stop reason
		{41, 0xfe}, // run without the humidity before it
		{42, 0x30}, // humidity over 100 %
		{48, 0x40}, // HDC peak over 125 C
		{50, 0x80}, // HDC end under -40 C
	} {
		b := append([]byte(nil), good...)
		b[heaterOffset+change.offset] = change.value
		if _, err := decodeObserverDetails(b); err == nil {
			t.Errorf("accepted invalid heater offset %d", change.offset)
		}
	}
	// Recovery never reads zero once service resumed; a streak exists only in normal service.
	b := append([]byte(nil), good...)
	copy(b[heaterOffset+36:heaterOffset+40], []byte{0, 0, 0, 0})
	if _, err := decodeObserverDetails(b); err == nil {
		t.Error("accepted completed run without recovery time")
	}
	b = append([]byte(nil), good...)
	b[heaterOffset+1], b[heaterOffset+15] = 3, 1
	b[heaterOffset+41] &^= 16 | 128
	copy(b[heaterOffset+50:heaterOffset+52], []byte{0, 0})
	copy(b[heaterOffset+56:heaterOffset+58], []byte{0, 0})
	if _, err := decodeObserverDetails(b); err == nil {
		t.Error("accepted condensing streak while recovering")
	}
	// The length is fixed for version 1.
	b = append(append([]byte(nil), good[:170]...), 10, 0, 57)
	b = append(b, good[heaterOffset:heaterOffset+57]...)
	if _, err := decodeObserverDetails(b); err == nil {
		t.Error("accepted short heater component")
	}
}
