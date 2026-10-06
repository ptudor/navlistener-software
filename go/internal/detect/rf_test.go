package detect

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/state"
)

func band(dep float64, cw, ant, jam int) state.StationRFBand {
	return state.StationRFBand{Block: 0, AGCDeparture: &dep, CWSuppress: cw, AntStatus: ant, JamState: jam}
}

func station(id string, bands ...state.StationRFBand) map[string]state.StationRF {
	return map[string]state.StationRF{id: {ID: id, Bands: bands, RFTrust: 1}}
}

// find returns the first event of a type, or a zero Event.
func find(evs []Event, typ string) (Event, bool) {
	for _, e := range evs {
		if e.Type == typ {
			return e, true
		}
	}
	return Event{}, false
}

// TestRFJammingConfirmed drives a corroborated jamming departure through the debounce: a
// large AGC departure plus a CW spike must confirm as jamming_detected after the window,
// not before.
func TestRFJammingConfirmed(t *testing.T) {
	d := New(0) // 60 s debounce
	t0 := time.Unix(1_700_000_000, 0)
	clear := station("stn1", band(0, 0, 2, 1))
	jam := station("stn1", band(1200, 220, 2, 1)) // AGC departure + CW tone

	d.TickStations(t0, clear) // seed ok
	if evs := d.TickStations(t0.Add(20*time.Second), jam); len(evs) != 0 {
		t.Fatalf("confirmed before debounce: %+v", evs)
	}
	evs := d.TickStations(t0.Add(85*time.Second), jam)
	e, ok := find(evs, "jamming_detected")
	if !ok {
		t.Fatalf("no jamming_detected after debounce; got %+v", evs)
	}
	if e.NewValue != "warn" || e.Severity != SevWarning {
		t.Errorf("jamming event = %+v, want warn/1", e)
	}
}

// TestRFJammingSevereImmediateBand confirms a near-total AGC collapse classifies critical
// regardless of CW corroboration (it is unambiguous).
func TestRFJammingSevereCrit(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	d.TickStations(t0, station("s", band(0, 0, 2, 1)))
	d.TickStations(t0.Add(10*time.Second), station("s", band(2500, 0, 2, 0))) // huge departure, no CW
	evs := d.TickStations(t0.Add(75*time.Second), station("s", band(2500, 0, 2, 0)))
	e, ok := find(evs, "jamming_detected")
	if !ok || e.NewValue != "crit" || e.Severity != SevCritical {
		t.Fatalf("severe jamming = %+v (ok=%v), want crit/2", e, ok)
	}
}

// TestRFMissingBandTelemetryHoldsAlarm guards a live station whose MON-RF
// evidence aged out must not recover a confirmed alarm merely because an empty band
// slice initializes every aggregate to zero.
func TestRFMissingBandTelemetryHoldsAlarm(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	clear := station("s", band(0, 0, 2, 0))
	crit := station("s", band(2500, 0, 2, 0))
	d.TickStations(t0, clear)
	d.TickStations(t0.Add(10*time.Second), crit)
	if e, ok := find(d.TickStations(t0.Add(75*time.Second), crit), "jamming_detected"); !ok || e.NewValue != "crit" {
		t.Fatalf("failed to seed confirmed critical state: %+v", e)
	}
	mean, resid := 40.0, 10.0
	missingBands := map[string]state.StationRF{"s": {ID: "s", Cn0Mean: &mean, Cn0Resid: &resid}}
	if evs := d.TickStations(t0.Add(150*time.Second), missingBands); len(evs) != 0 {
		t.Fatalf("missing band evidence emitted a recovery: %+v", evs)
	}
	if evs := d.TickStations(t0.Add(230*time.Second), missingBands); len(evs) != 0 {
		t.Fatalf("missing band evidence confirmed a recovery: %+v", evs)
	}
	// Actual clear evidence should still recover the held machine, after the
	// station clear dwell rather than the shorter onset debounce.
	d.TickStations(t0.Add(240*time.Second), clear)
	if evs := d.TickStations(t0.Add(310*time.Second), clear); len(evs) != 0 {
		t.Fatalf("recovery confirmed inside the clear dwell: %+v", evs)
	}
	e, ok := find(d.TickStations(t0.Add(240*time.Second+StationClearDwell), clear), "jamming_detected")
	if !ok || e.NewValue != "ok" {
		t.Fatalf("measured clear state did not recover held alarm: %+v", e)
	}
}

// TestRFDegradedNotJamming confirms a lone metric departure (AGC down, but no CW and no
// receiver jam flag) reports as station_rf_degraded, not a jamming attack claim.
func TestRFDegradedNotJamming(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	d.TickStations(t0, station("s", band(0, 0, 2, 0)))
	uncorr := station("s", band(1200, 0, 2, 0)) // departure alone, uncorroborated
	d.TickStations(t0.Add(10*time.Second), uncorr)
	evs := d.TickStations(t0.Add(75*time.Second), uncorr)
	if _, ok := find(evs, "jamming_detected"); ok {
		t.Error("uncorroborated departure wrongly called jamming")
	}
	e, ok := find(evs, "station_rf_degraded")
	if !ok || e.NewValue != "degraded" {
		t.Fatalf("station_rf_degraded = %+v (ok=%v), want degraded", e, ok)
	}
}

// TestRFReceiverJamFlagAloneDegraded guards a receiver reporting its own jam flag
// (jamState >= 2) with AGC below the departure threshold and no CW spike must surface as
// station_rf_degraded (DEFENSE-PNT §2's single-metric rule), not classify "ok".
func TestRFReceiverJamFlagAloneDegraded(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	d.TickStations(t0, station("s", band(0, 0, 2, 0))) // seed clear
	jamFlag := station("s", band(0, 0, 2, 3))          // receiver jamState=3 alone, no AGC/CW
	d.TickStations(t0.Add(10*time.Second), jamFlag)
	evs := d.TickStations(t0.Add(75*time.Second), jamFlag)
	if _, ok := find(evs, "jamming_detected"); ok {
		t.Error("jamState-alone wrongly called a jamming attack")
	}
	e, ok := find(evs, "station_rf_degraded")
	if !ok || e.NewValue != "degraded" {
		t.Fatalf("station_rf_degraded = %+v (ok=%v), want degraded for jamState-alone", e, ok)
	}
}

// TestRFAntennaFault confirms an antenna open/short surfaces antenna_fault.
func TestRFAntennaFault(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	d.TickStations(t0, station("s", band(0, 0, 2, 1)))                     // antenna ok
	d.TickStations(t0.Add(10*time.Second), station("s", band(0, 0, 4, 1))) // open
	evs := d.TickStations(t0.Add(75*time.Second), station("s", band(0, 0, 4, 1)))
	if e, ok := find(evs, "antenna_fault"); !ok || e.NewValue != "fault" {
		t.Fatalf("antenna_fault = %+v (ok=%v)", e, ok)
	}
}

// TestRFRecoverySeverityInfo guards the clear severity of the station RF alarms: a raise
// keeps its warning/critical severity, and the confirmed return to the nominal state is
// info, like the other integrity alarms (wn_mismatch, ura_alert, xsig_divergence).
func TestRFRecoverySeverityInfo(t *testing.T) {
	cases := []struct {
		name, typ, raised string
		degraded          map[string]state.StationRF
		raisedSev         int
	}{
		{"jamming", "jamming_detected", "crit", station("s", band(2500, 0, 2, 0)), SevCritical},
		{"antenna", "antenna_fault", "fault", station("s", band(0, 0, 3, 0)), SevWarning},
		{"degraded", "station_rf_degraded", "degraded", station("s", band(1200, 0, 2, 0)), SevWarning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := New(0)
			t0 := time.Unix(1_700_000_000, 0)
			clear := station("s", band(0, 0, 2, 0))
			d.TickStations(t0, clear)
			d.TickStations(t0.Add(10*time.Second), tc.degraded)
			raise, ok := find(d.TickStations(t0.Add(75*time.Second), tc.degraded), tc.typ)
			if !ok || raise.NewValue != tc.raised || raise.Severity != tc.raisedSev {
				t.Fatalf("raise = %+v (ok=%v), want %s/%d", raise, ok, tc.raised, tc.raisedSev)
			}
			d.TickStations(t0.Add(80*time.Second), clear)
			var cleared Event
			for at := 95 * time.Second; at <= 20*time.Minute; at += 15 * time.Second {
				if e, ok := find(d.TickStations(t0.Add(at), clear), tc.typ); ok {
					cleared = e
					break
				}
			}
			if cleared.NewValue != "ok" || cleared.Severity != SevInfo {
				t.Fatalf("clear = %+v, want ok/%d", cleared, SevInfo)
			}
		})
	}
}

// TestRFClearDwellAsymmetric guards the station dwell policy: degradation confirms after
// the onset debounce, and the return to nominal must hold for StationClearDwell.
func TestRFClearDwellAsymmetric(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	clear, open := station("s", band(0, 0, 2, 0)), station("s", band(0, 0, 4, 0))
	d.TickStations(t0, clear)
	d.TickStations(t0.Add(15*time.Second), open)
	if _, ok := find(d.TickStations(t0.Add(15*time.Second+DebounceDuration), open), "antenna_fault"); !ok {
		t.Fatal("antenna fault did not confirm after the onset debounce")
	}
	recoverAt := t0.Add(2 * time.Minute)
	d.TickStations(recoverAt, clear)
	for at := 15 * time.Second; at < StationClearDwell; at += 15 * time.Second {
		if e, ok := find(d.TickStations(recoverAt.Add(at), clear), "antenna_fault"); ok {
			t.Fatalf("recovery confirmed %s into a %s clear dwell: %+v", at, StationClearDwell, e)
		}
	}
	e, ok := find(d.TickStations(recoverAt.Add(StationClearDwell), clear), "antenna_fault")
	if !ok || e.OldValue != "fault" || e.NewValue != "ok" {
		t.Fatalf("recovery after the clear dwell = %+v (ok=%v)", e, ok)
	}
}

// TestRFDegradedFirstObservationRaises guards the cold-start case: a station whose
// first observation is already jammed must raise jamming_detected after the onset
// dwell, reporting the unconfirmed prior state as "unknown" rather than seeding the
// jammed state silently.
func TestRFDegradedFirstObservationRaises(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	jammed := station("s", band(2500, 0, 2, 0))
	if evs := d.TickStations(t0, jammed); len(evs) != 0 {
		t.Fatalf("first observation emitted immediately: %+v", evs)
	}
	if evs := d.TickStations(t0.Add(30*time.Second), jammed); len(evs) != 0 {
		t.Fatalf("confirmed before the onset dwell: %+v", evs)
	}
	e, ok := find(d.TickStations(t0.Add(DebounceDuration), jammed), "jamming_detected")
	if !ok || e.OldValue != stateUnknown || e.NewValue != "crit" || e.Severity != SevCritical {
		t.Fatalf("cold-start jamming = %+v (ok=%v), want unknown→crit/2", e, ok)
	}
}

// TestRFUnconfirmedFirstDegradationRevertsSilently guards against a phantom recovery: a
// degraded first observation that returns to nominal before its onset confirms seeds
// nominal silently, exactly as a nominal first sighting would.
func TestRFUnconfirmedFirstDegradationRevertsSilently(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	d.TickStations(t0, station("s", band(2500, 0, 2, 0)))
	for at := 15 * time.Second; at <= 10*time.Minute; at += 15 * time.Second {
		if evs := d.TickStations(t0.Add(at), station("s", band(0, 0, 2, 0))); len(evs) != 0 {
			t.Fatalf("unconfirmed degradation produced events at +%s: %+v", at, evs)
		}
	}
	if band, _ := d.currentBand("s", "jamming"); band != "ok" {
		t.Fatalf("jamming band = %q, want ok", band)
	}
}
