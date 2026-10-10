package serve

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/ptudor/navlistener/internal/state"
)

const yumaWeekSeconds = 604800.0

// serveGPSYUMA publishes the GPS records in the control-segment YUMA layout
// specified by ICD-GPS-240D §40.5. It uses the same audience resolution and
// delivery-revocation boundary as the JSON feeds.
func (s *Server) serveGPSYUMA(w http.ResponseWriter, r *http.Request) {
	if methodNotAllowedGetHead(w, r) {
		return
	}
	view, ok := s.resolveRequestView(w, r)
	if !ok {
		return
	}
	delivery := s.beginDelivery(r, view.audience)
	defer delivery.finish()

	now := s.now()
	body, ok := buildGPSYUMAStable(view.store, now)
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "audience state changed; retry request")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="current.alm"`)
	s.setAudienceCacheHeaders(w, view.audience)
	if r.Method != http.MethodHead {
		delivery.write(w, body)
	}
}

func buildGPSYUMAStable(st *state.Store, now time.Time) ([]byte, bool) {
	if st == nil {
		return nil, false
	}
	for range 3 {
		generation := st.Generation()
		body := formatGPSYUMA(st.FeedAlmanacRecords(now), now)
		if generation == st.Generation() {
			return body, true
		}
	}
	return nil, false
}

func formatGPSYUMA(records []state.AlmanacRecord, now time.Time) []byte {
	var out bytes.Buffer
	for _, record := range records {
		if record.Source != "gps_lnav" || record.LNAV == nil || record.ReferenceWeek == nil || record.ReferenceTime == nil {
			continue
		}
		a := record.LNAV
		referenceWeek := moduloWeek(*record.ReferenceWeek)
		generatedWeek := generatedYUMAWeek(record, now)
		health := a.Health & 0x1f
		if a.Health>>5 != 0 {
			health |= 1 << 5
		}
		fmt.Fprintf(&out, "******** Week %d almanac for PRN-%02d ********\n", generatedWeek, a.SVID)
		fmt.Fprintf(&out, "%-28s%02d\n", "ID:", a.SVID)
		fmt.Fprintf(&out, "%-28s%03d\n", "Health:", health)
		fmt.Fprintf(&out, "%-28s%s\n", "Eccentricity:", yumaScientific(a.Eccentricity))
		fmt.Fprintf(&out, "%-28s%.4f\n", "Time of Applicability(s):", a.TimeOfApplicabilityS)
		fmt.Fprintf(&out, "%-28s%.10f\n", "Orbital Inclination(rad):", 0.30*math.Pi+a.InclinationOffsetRad)
		fmt.Fprintf(&out, "%-28s%s\n", "Rate of Right Ascen(r/s):", yumaScientific(a.RateOfRightAscensionRadS))
		fmt.Fprintf(&out, "%-28s%.6f\n", "SQRT(A)  (m 1/2):", a.SqrtASqrtM)
		fmt.Fprintf(&out, "%-28s%s\n", "Right Ascen at Week(rad):", yumaScientific(a.LongitudeOfAscendingNodeRad))
		fmt.Fprintf(&out, "%-28s%s\n", "Argument of Perigee(rad):", yumaScientific(a.ArgumentOfPerigeeRad))
		fmt.Fprintf(&out, "%-28s%s\n", "Mean Anom(rad):", yumaScientific(a.MeanAnomalyRad))
		fmt.Fprintf(&out, "%-28s%s\n", "Af0(s):", yumaScientific(a.ClockBiasS))
		fmt.Fprintf(&out, "%-28s%s\n", "Af1(s/s):", yumaScientific(a.ClockDriftSS))
		fmt.Fprintf(&out, "%-28s%d\n\n", "week:", referenceWeek)
	}
	return out.Bytes()
}

// yumaScientific matches ICD-GPS-240D's 0.xxxxxxxxxxE±nnn notation rather
// than Go's usual 1.xxxxxxxxxxE±nn. Zero is given the same fixed exponent form.
func yumaScientific(value float64) string {
	if value == 0 {
		return "0.0000000000E+000"
	}
	exponent := int(math.Floor(math.Log10(math.Abs(value)))) + 1
	mantissa := value / math.Pow10(exponent)
	return fmt.Sprintf("%.10fE%+04d", mantissa, exponent)
}

func generatedYUMAWeek(record state.AlmanacRecord, now time.Time) int {
	week := *record.ReferenceWeek
	delta := now.Unix() - *record.ReferenceTime
	seconds := record.LNAV.TimeOfApplicabilityS + float64(delta)
	week += int(math.Floor(seconds / yumaWeekSeconds))
	return moduloWeek(week)
}

func moduloWeek(week int) int {
	week %= 1024
	if week < 0 {
		week += 1024
	}
	return week
}
