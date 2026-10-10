package state

import (
	"fmt"
	"sort"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/gnss/gnsstime"
)

// AlmanacRecord is one retained, normalized broadcast-almanac source record.
// Source selects exactly one message-family payload. Unlike FeedAlmanac's
// evaluated ECEF positions, these fields are sufficient to reproduce the
// decoded orbit, clock, health, reference epoch, and source metadata without
// attempting to reverse a propagated position.
type AlmanacRecord struct {
	Name          string `json:"name"`
	GNSSID        int    `json:"gnssid"`
	Source        string `json:"source"`
	Transmitter   string `json:"transmitter,omitempty"`
	ReceivedAt    int64  `json:"received_at"`
	TimeSystem    string `json:"time_system"`
	ReferenceTime *int64 `json:"reference_time,omitempty"`
	ReferenceWeek *int   `json:"reference_week,omitempty"`
	WeekRaw       *int   `json:"week_raw,omitempty"`
	WeekBits      int    `json:"week_bits,omitempty"`

	LNAV         *LNAVAlmanacRecord     `json:"lnav,omitempty"`
	GalileoINAV  *GalileoAlmanacRecord  `json:"galileo_inav,omitempty"`
	BeiDouBCNAV2 *BeiDouAlmanacRecord   `json:"beidou_bcnav2,omitempty"`
	BeiDouD1     *BeiDouD1AlmanacRecord `json:"beidou_d1,omitempty"`
	GLONASSFDMA  *GLONASSAlmanacRecord  `json:"glonass_fdma,omitempty"`
}

// LNAVAlmanacRecord preserves a GPS or QZSS LNAV almanac page after scaling to
// SI. For QZSS, Eccentricity and InclinationOffsetRad retain the broadcast
// offsets; the source distinguishes them from GPS's corresponding semantics.
type LNAVAlmanacRecord struct {
	DataID                      int     `json:"data_id"`
	SVID                        int     `json:"svid"`
	Eccentricity                float64 `json:"eccentricity"`
	TimeOfApplicabilityS        float64 `json:"time_of_applicability_s"`
	InclinationOffsetRad        float64 `json:"inclination_offset_rad"`
	RateOfRightAscensionRadS    float64 `json:"rate_of_right_ascension_rad_s"`
	Health                      int     `json:"health"`
	SqrtASqrtM                  float64 `json:"sqrt_a_sqrt_m"`
	LongitudeOfAscendingNodeRad float64 `json:"longitude_of_ascending_node_rad"`
	ArgumentOfPerigeeRad        float64 `json:"argument_of_perigee_rad"`
	MeanAnomalyRad              float64 `json:"mean_anomaly_rad"`
	ClockBiasS                  float64 `json:"clock_bias_s"`
	ClockDriftSS                float64 `json:"clock_drift_s_s"`
}

// GalileoAlmanacRecord preserves one completed Galileo I/NAV almanac entry.
type GalileoAlmanacRecord struct {
	SVID                        int     `json:"svid"`
	IssueOfData                 int     `json:"issue_of_data"`
	WeekRaw                     int     `json:"week_raw"`
	TimeOfApplicabilityS        float64 `json:"time_of_applicability_s"`
	SqrtASqrtM                  float64 `json:"sqrt_a_sqrt_m"`
	Eccentricity                float64 `json:"eccentricity"`
	InclinationRad              float64 `json:"inclination_rad"`
	LongitudeOfAscendingNodeRad float64 `json:"longitude_of_ascending_node_rad"`
	RateOfRightAscensionRadS    float64 `json:"rate_of_right_ascension_rad_s"`
	ArgumentOfPerigeeRad        float64 `json:"argument_of_perigee_rad"`
	MeanAnomalyRad              float64 `json:"mean_anomaly_rad"`
	ClockBiasS                  float64 `json:"clock_bias_s"`
	ClockDriftSS                float64 `json:"clock_drift_s_s"`
	E5bSignalHealth             int     `json:"e5b_signal_health"`
	E1BSignalHealth             int     `json:"e1b_signal_health"`
}

// BeiDouAlmanacRecord preserves one B-CNAV2 midi almanac entry.
type BeiDouAlmanacRecord struct {
	PRN                         int     `json:"prn"`
	SatelliteType               int     `json:"satellite_type"`
	Week                        int     `json:"week"`
	TimeOfApplicabilityS        float64 `json:"time_of_applicability_s"`
	Eccentricity                float64 `json:"eccentricity"`
	InclinationOffsetRad        float64 `json:"inclination_offset_rad"`
	SqrtASqrtM                  float64 `json:"sqrt_a_sqrt_m"`
	LongitudeOfAscendingNodeRad float64 `json:"longitude_of_ascending_node_rad"`
	RateOfRightAscensionRadS    float64 `json:"rate_of_right_ascension_rad_s"`
	ArgumentOfPerigeeRad        float64 `json:"argument_of_perigee_rad"`
	MeanAnomalyRad              float64 `json:"mean_anomaly_rad"`
	ClockBiasS                  float64 `json:"clock_bias_s"`
	ClockDriftSS                float64 `json:"clock_drift_s_s"`
	Health                      int     `json:"health"`
}

// BeiDouD1AlmanacRecord preserves one basic or resolved expanded D1 almanac
// page. The batch's page-8 week is common metadata on AlmanacRecord.
type BeiDouD1AlmanacRecord struct {
	SVID                        int     `json:"svid"`
	Page                        int     `json:"page"`
	Expanded                    bool    `json:"expanded"`
	AlmanacID                   int     `json:"almanac_id"`
	TimeOfApplicabilityS        float64 `json:"time_of_applicability_s"`
	SqrtASqrtM                  float64 `json:"sqrt_a_sqrt_m"`
	Eccentricity                float64 `json:"eccentricity"`
	InclinationOffsetRad        float64 `json:"inclination_offset_rad"`
	LongitudeOfAscendingNodeRad float64 `json:"longitude_of_ascending_node_rad"`
	RateOfRightAscensionRadS    float64 `json:"rate_of_right_ascension_rad_s"`
	ArgumentOfPerigeeRad        float64 `json:"argument_of_perigee_rad"`
	MeanAnomalyRad              float64 `json:"mean_anomaly_rad"`
	ClockBiasS                  float64 `json:"clock_bias_s"`
	ClockDriftSS                float64 `json:"clock_drift_s_s"`
}

// GLONASSAlmanacRecord preserves one FDMA two-string almanac entry. ReferenceDay
// is NA in the current four-year interval; TimeOfAscendingNodeS is t_lambda on
// that GLONASS calendar day.
type GLONASSAlmanacRecord struct {
	ReferenceDay                int     `json:"reference_day"`
	LongitudeOfAscendingNodeRad float64 `json:"longitude_of_ascending_node_rad"`
	TimeOfAscendingNodeS        float64 `json:"time_of_ascending_node_s"`
	InclinationOffsetRad        float64 `json:"inclination_offset_rad"`
	DraconianPeriodOffsetS      float64 `json:"draconian_period_offset_s"`
	DraconianPeriodRateS        float64 `json:"draconian_period_rate_s"`
	Eccentricity                float64 `json:"eccentricity"`
	ArgumentOfPerigeeRad        float64 `json:"argument_of_perigee_rad"`
	Slot                        int     `json:"slot"`
	FrequencyChannel            int     `json:"frequency_channel"`
	Operable                    bool    `json:"operable"`
	SatelliteType               int     `json:"satellite_type"`
	ClockCorrectionS            float64 `json:"clock_correction_s"`
}

// FeedAlmanacRecords returns every retained, still-valid source record. Records
// are sorted by subject name and source so JSON and text exporters are stable.
func (s *Store) FeedAlmanacRecords(now time.Time) []AlmanacRecord {
	s.almMu.Lock()
	keplerRecords := make([]almanacSourceRecord, 0, len(s.almanacRecords))
	for _, record := range s.almanacRecords {
		if age := now.Sub(record.toa); age > -keplerAlmanacValidity(record.g) && age < keplerAlmanacValidity(record.g) {
			keplerRecords = append(keplerRecords, record)
		}
	}
	s.almMu.Unlock()

	out := make([]AlmanacRecord, 0, len(keplerRecords)+24)
	for _, record := range keplerRecords {
		out = append(out, publicAlmanacRecord(record))
	}

	s.gloAlmMu.Lock()
	for _, slot := range s.gloAlmanac {
		if now.Sub(slot.lastSeen) > gloAlmanacStaleAfter {
			continue
		}
		a := slot.entry
		receivedAt := slot.receivedAt
		if receivedAt.IsZero() { // test/manual injection compatibility
			receivedAt = slot.lastSeen
		}
		record := AlmanacRecord{
			Name: fmt.Sprintf("R%02d", a.Alm.Slot), GNSSID: int(gnss.GLONASS),
			Source: almanacSourceGLONASSFDMA, ReceivedAt: receivedAt.Unix(), TimeSystem: "GLONASST",
			GLONASSFDMA: &GLONASSAlmanacRecord{
				ReferenceDay: a.Alm.NA, LongitudeOfAscendingNodeRad: a.Alm.Lambda,
				TimeOfAscendingNodeS: a.Alm.Tlambda, InclinationOffsetRad: a.Alm.DeltaI,
				DraconianPeriodOffsetS: a.Alm.DeltaT, DraconianPeriodRateS: a.Alm.DeltaTdot,
				Eccentricity: a.Alm.Ecc, ArgumentOfPerigeeRad: a.Alm.Omega,
				Slot: a.Alm.Slot, FrequencyChannel: a.Alm.FreqCh, Operable: a.Cn != 0,
				SatelliteType: a.SatType, ClockCorrectionS: a.TauNA,
			},
		}
		if slot.transmitter > 0 {
			record.Transmitter = fmt.Sprintf("R%02d", slot.transmitter)
		}
		out = append(out, record)
	}
	s.gloAlmMu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].Source < out[j].Source
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func publicAlmanacRecord(record almanacSourceRecord) AlmanacRecord {
	referenceTime := record.toa.Unix()
	referenceWeek, _ := gnsstime.WeekAt(almanacTimeSystem(record.g), float64(referenceTime), float64(gpsUTCOffset))
	out := AlmanacRecord{
		Name: fmt.Sprintf("%c%02d", record.g.Letter(), record.subject), GNSSID: int(record.g),
		Source: record.source, ReceivedAt: record.stamp.Unix(), TimeSystem: almanacTimeSystemName(record.g),
		ReferenceTime: &referenceTime, ReferenceWeek: &referenceWeek,
	}
	if record.transmitter > 0 {
		out.Transmitter = fmt.Sprintf("%c%02d", record.g.Letter(), record.transmitter)
	}
	if record.weekBits > 0 {
		weekRaw := record.weekRaw
		out.WeekRaw, out.WeekBits = &weekRaw, record.weekBits
	}
	if a := record.lnav; a != nil {
		out.LNAV = lnavAlmanacRecord(a)
	}
	if a := record.galileo; a != nil {
		out.GalileoINAV = galileoAlmanacRecord(a)
	}
	if a := record.beidouMidi; a != nil {
		out.BeiDouBCNAV2 = beiDouAlmanacRecord(a)
	}
	if a := record.beidouD1; a != nil {
		out.BeiDouD1 = beiDouD1AlmanacRecord(a)
	}
	return out
}

func almanacTimeSystem(g gnss.GNSSID) gnsstime.System {
	switch g {
	case gnss.Galileo:
		return gnsstime.SysGalileo
	case gnss.BeiDou:
		return gnsstime.SysBeiDou
	default:
		return gnsstime.SysGPS
	}
}

func almanacTimeSystemName(g gnss.GNSSID) string {
	switch g {
	case gnss.Galileo:
		return "GST"
	case gnss.BeiDou:
		return "BDT"
	default:
		return "GPST"
	}
}

func lnavAlmanacRecord(a *frame.LNAVAlmanac) *LNAVAlmanacRecord {
	return &LNAVAlmanacRecord{
		DataID: a.DataID, SVID: a.SVID, Eccentricity: a.Ecc,
		TimeOfApplicabilityS: a.Toa, InclinationOffsetRad: a.DeltaI,
		RateOfRightAscensionRadS: a.OmegaDot, Health: a.Health, SqrtASqrtM: a.SqrtA,
		LongitudeOfAscendingNodeRad: a.Omega0, ArgumentOfPerigeeRad: a.Omega,
		MeanAnomalyRad: a.M0, ClockBiasS: a.Af0, ClockDriftSS: a.Af1,
	}
}

func galileoAlmanacRecord(a *frame.GalileoAlmanac) *GalileoAlmanacRecord {
	return &GalileoAlmanacRecord{
		SVID: a.SVID, IssueOfData: a.IODa, WeekRaw: a.WNa, TimeOfApplicabilityS: a.T0a,
		SqrtASqrtM: a.SqrtA, Eccentricity: a.Ecc, InclinationRad: a.I0,
		LongitudeOfAscendingNodeRad: a.Omega0, RateOfRightAscensionRadS: a.OmegaDot,
		ArgumentOfPerigeeRad: a.Omega, MeanAnomalyRad: a.M0,
		ClockBiasS: a.Af0, ClockDriftSS: a.Af1,
		E5bSignalHealth: a.E5bSHS, E1BSignalHealth: a.E1BSHS,
	}
}

func beiDouAlmanacRecord(a *frame.BeiDouMidiAlmanac) *BeiDouAlmanacRecord {
	return &BeiDouAlmanacRecord{
		PRN: a.PRN, SatelliteType: a.SatType, Week: a.WN, TimeOfApplicabilityS: a.Toa,
		Eccentricity: a.Ecc, InclinationOffsetRad: a.DeltaI, SqrtASqrtM: a.SqrtA,
		LongitudeOfAscendingNodeRad: a.Omega0, RateOfRightAscensionRadS: a.OmegaDot,
		ArgumentOfPerigeeRad: a.Omega, MeanAnomalyRad: a.M0,
		ClockBiasS: a.Af0, ClockDriftSS: a.Af1, Health: a.Health,
	}
}

func beiDouD1AlmanacRecord(a *frame.BeiDouD1Almanac) *BeiDouD1AlmanacRecord {
	return &BeiDouD1AlmanacRecord{
		SVID: a.SVID, Page: a.Pnum, Expanded: a.Expanded, AlmanacID: a.AmID,
		TimeOfApplicabilityS: a.Toa, SqrtASqrtM: a.SqrtA, Eccentricity: a.Ecc,
		InclinationOffsetRad: a.DeltaI, LongitudeOfAscendingNodeRad: a.Omega0,
		RateOfRightAscensionRadS: a.OmegaDot, ArgumentOfPerigeeRad: a.Omega,
		MeanAnomalyRad: a.M0, ClockBiasS: a.A0, ClockDriftSS: a.A1,
	}
}
