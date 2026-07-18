package state

import (
	"testing"
	"time"
)

// sf1WordsURA is sf1Words with an explicit 4-bit URA index (word 3 bits 13-16),
// so a test can broadcast the URA-15 "no accuracy prediction" sentinel
// (IS-GPS-200N §20.3.3.3.1.3).
func sf1WordsURA(iodcLo, ura int64) []uint32 {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 1)
	setField(buf, 3, 1, 10, 2200)
	setField(buf, 3, 13, 4, ura)
	setField(buf, 8, 1, 8, iodcLo)
	setField(buf, 8, 9, 16, 27000)
	setField(buf, 10, 1, 22, 214748)
	return packWords(buf)
}

// sf1WordsAlert is sf1Words with the HOW alert flag (word 2 bit 18) raised.
func sf1WordsAlert(iodcLo int64) []uint32 {
	buf := make([]byte, 30)
	setField(buf, 2, 18, 1, 1)
	setField(buf, 2, 20, 3, 1)
	setField(buf, 3, 1, 10, 2200)
	setField(buf, 3, 13, 4, 4)
	setField(buf, 8, 1, 8, iodcLo)
	setField(buf, 8, 9, 16, 27000)
	setField(buf, 10, 1, 22, 214748)
	return packWords(buf)
}

// TestFeedAlertFlagServed guards state/feed half: the HOW alert flag
// must reach the feed freshest-wins (it rides every subframe, outside any
// IODC-gated set), so the detector can classify its transitions. Absent until a
// subframe has decoded ("absent = unknown").
func TestFeedAlertFlagServed(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(gpsFrame(sf1Words(85), now))
	s.Apply(gpsFrame(sf2Words(85, 205075516), now))
	s.Apply(gpsFrame(sf3Words(85), now))

	sv := s.FeedSVs(now)["G05@0"]
	if sv.Alert == nil || *sv.Alert {
		t.Fatalf("alert = %v, want present and false (clear HOW decoded)", sv.Alert)
	}

	// A re-broadcast subframe 1 with the alert raised (same IODC — no data-set
	// changeover) must flip the served flag immediately.
	s.Apply(gpsFrame(sf1WordsAlert(85), now))
	sv = s.FeedSVs(now)["G05@0"]
	if sv.Alert == nil || !*sv.Alert {
		t.Fatalf("alert = %v after raised HOW, want true (freshest-wins)", sv.Alert)
	}
}

// TestFeedURA15ServesAccIndex guards a GPS SV broadcasting URA index 15
// ("no accuracy prediction is available … use at own risk", IS-GPS-200N
// §20.3.3.3.1.3) with a healthy health word must NOT collapse to a fully-healthy
// entry with the accuracy silently absent. The feed serves sisa_valid=false with
// no sisa_m (there is no metres value) but exposes the raw index as acc_index,
// so a consumer — and the detector's no_accuracy classifier — can distinguish
// "index says do-not-trust" from "no accuracy field decoded yet".
func TestFeedURA15ServesAccIndex(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(gpsFrame(sf1WordsURA(85, 15), now))
	s.Apply(gpsFrame(sf2Words(85, 205075516), now))
	s.Apply(gpsFrame(sf3Words(85), now))

	sv, ok := s.FeedSVs(now)["G05@0"]
	if !ok {
		t.Fatal("G05@0 missing from svs feed")
	}
	if sv.SISAValid || sv.SISAM != nil {
		t.Errorf("URA 15 served as a real accuracy: sisa_valid=%v sisa_m=%v", sv.SISAValid, sv.SISAM)
	}
	if sv.AccIndex == nil || *sv.AccIndex != 15 {
		t.Fatalf("acc_index = %v, want 15 (the sentinel itself must be served)", sv.AccIndex)
	}
	if sv.HealthCode != 1 {
		t.Errorf("health_code = %d, want 1 (health word is independent of URA)", sv.HealthCode)
	}

	// A normal index serves both the metres value and the raw index.
	s.Apply(gpsFrame(sf1WordsURA(85, 4), now))
	sv = s.FeedSVs(now)["G05@0"]
	if !sv.SISAValid || sv.SISAM == nil {
		t.Fatalf("URA 4: sisa_valid=%v sisa_m=%v, want a decoded metres value", sv.SISAValid, sv.SISAM)
	}
	if sv.AccIndex == nil || *sv.AccIndex != 4 {
		t.Errorf("acc_index = %v, want 4", sv.AccIndex)
	}
}
