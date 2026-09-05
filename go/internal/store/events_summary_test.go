package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

func TestIntegrationEventSummarySubjectContracts(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, config.Store{DSN: testDSN(t), RawRetention: "7 days", CompressAfter: "1 day"}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fixtures := []struct{ typ, subject, constellation string }{
		{"health_change", "G01@0", "gps"},
		{"health_change", "S120@0", "sbas"},
		{"xsig_divergence", "E14", "galileo"},
		{"sbas_health", "S120", "sbas"},
		{"sbas_lost", "S158", "sbas"},
		{"qzss_health", "J10@255", "qzss"},
		{"navic_health", "I14@0", "navic"},
		{"health_change", "R24@1", "glonass"},
		{"bds_integrity_flag", "C63@8", "beidou"},
		{"station_offline", "G01", ""},
		{"jamming_detected", "S120", ""},
		{"capability_signal_lost", "G01@0", ""},
		{"future_type", "E14@0", ""},
		{"health_change", "G01@0junk", ""},
		{"xsig_divergence", "E14@0", ""},
		{"sbas_health", "G01", ""},
		{"health_change", "G00@0", ""},
		{"health_change", "S159@0", ""},
		{"health_change", "G01@256", ""},
		{"health_change", "G001@0", ""},
		{"health_change", "G01@00", ""},
		{"sbas_health", "S120@0", "sbas"},
	}
	base := time.Now().UTC().Truncate(time.Microsecond).Add(12 * time.Hour)
	audiences := []string{"public", "operator:local", fmt.Sprintf("organization:summary-%d", base.UnixNano()), fmt.Sprintf("collection:summary-%d", base.UnixNano())}
	for _, audience := range audiences {
		for i, f := range fixtures {
			_, err = s.WriteEvent(ctx, EventRow{Audience: audience, Time: base.Add(time.Duration(i) * time.Microsecond), SV: f.subject, Type: f.typ, Severity: i % 3,
				DedupeKey: fmt.Sprintf("summary-%d-%s-%d", base.UnixNano(), audience, i)})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, audience := range audiences {
		for _, window := range [][2]int{{0, len(fixtures) - 1}, {3, 10}, {0, 0}, {2, 2}, {9, 12}, {21, 21}} {
			sum, e := s.SummarizeEventsForAudience(ctx, audience, base.Add(time.Duration(window[0])*time.Microsecond), base.Add(time.Duration(window[1])*time.Microsecond))
			if e != nil {
				t.Fatal(e)
			}
			wantTypes := map[string]int{}
			wantConstellations := map[string]int{}
			critical, warning := 0, 0
			var latest *time.Time
			for i := window[0]; i <= window[1]; i++ {
				f := fixtures[i]
				wantTypes[f.typ]++
				if f.constellation != "" {
					wantConstellations[f.constellation]++
				}
				if i%3 == 2 {
					critical++
					at := base.Add(time.Duration(i) * time.Microsecond)
					latest = &at
				}
				if i%3 == 1 {
					warning++
				}
			}
			if sum.TotalEvents != window[1]-window[0]+1 || !reflect.DeepEqual(sum.ByType, wantTypes) || !reflect.DeepEqual(sum.ByConstellation, wantConstellations) ||
				sum.CriticalEvents != critical || sum.WarningEvents != warning || !reflect.DeepEqual(sum.LastCritical, latest) {
				t.Fatalf("%s window %v: summary=%+v want types=%v constellations=%v", audience, window, sum, wantTypes, wantConstellations)
			}
		}
	}
}
