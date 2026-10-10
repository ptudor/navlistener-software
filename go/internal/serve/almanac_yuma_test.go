package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/state"
)

func TestFormatGPSYUMA(t *testing.T) {
	referenceWeek := 2107 // modulo-1024 week 59, as in ICD-GPS-240D Figure 40-2
	referenceTime := int64(1_700_000_000)
	records := []state.AlmanacRecord{{
		Name: "G01", GNSSID: 0, Source: "gps_lnav", ReferenceWeek: &referenceWeek,
		ReferenceTime: &referenceTime,
		LNAV: &state.LNAVAlmanacRecord{
			SVID: 1, Health: 0xfc, Eccentricity: 0.009913444519,
			TimeOfApplicabilityS: 503808, InclinationOffsetRad: 0.03803118499,
			RateOfRightAscensionRadS: -7.943188099e-9, SqrtASqrtM: 5153.577637,
			LongitudeOfAscendingNodeRad: 3.072393117, ArgumentOfPerigeeRad: 0.782072915,
			MeanAnomalyRad: 1.774841613, ClockBiasS: -3.862380981e-4,
			ClockDriftSS: -3.637978807e-12,
		},
	}}
	body := string(formatGPSYUMA(records, time.Unix(referenceTime+10, 0)))
	for _, want := range []string{
		"******** Week 59 almanac for PRN-01 ********",
		"Health:                     060",
		"Eccentricity:               0.9913444519E-002",
		"Time of Applicability(s):   503808.0000",
		"Orbital Inclination(rad):   0.9805089811",
		"Rate of Right Ascen(r/s):   -0.7943188099E-008",
		"SQRT(A)  (m 1/2):           5153.577637",
		"week:                       59",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("YUMA output missing %q:\n%s", want, body)
		}
	}
}

func TestGPSYUMAHandlerHeadersAndMethods(t *testing.T) {
	s := testServer(nil)
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/almanac/gps.yuma", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rr.Header().Get("Content-Disposition"); got != `attachment; filename="current.alm"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q", got)
	}

	post := httptest.NewRecorder()
	s.serveGPSYUMA(post, httptest.NewRequest(http.MethodPost, "/gnss/api/v2/almanac/gps.yuma", nil))
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST status = %d Allow = %q", post.Code, post.Header().Get("Allow"))
	}
}
