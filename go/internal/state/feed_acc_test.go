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
