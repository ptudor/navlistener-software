package integrity

import (
	"math"
	"sort"
	"time"
)

const (
	// CheckRegionalPressure compares one station's fractional pressure tendency
	// with qualified, surveyed neighbours. It is environmental context only.
	CheckRegionalPressure = "regional_pressure"
	// CheckBarometricHeight compares GNSS height change with the independently
	// measured, regional-weather-corrected pressure change.
	CheckBarometricHeight = "barometric_height"
)

var (
	regionalPressureInfo = Info{Name: CheckRegionalPressure, Version: 1, Domain: DomainEnvironment, Advisory: true, UnknownImmediate: true}
	barometricHeightInfo = Info{Name: CheckBarometricHeight, Version: 1, Domain: DomainBarometricPosition, LowerOnly: true, UnknownImmediate: true}
)

const (
	ReasonBarometerNotConfigured         = "barometer_not_configured"
	ReasonPressureSensorUnhealthy        = "pressure_sensor_unhealthy"
	ReasonPressureMeasurementUnavailable = "pressure_measurement_unavailable"
	ReasonPressureHistoryWarmup          = "pressure_history_warming_up"
	ReasonInsufficientPressureNeighbours = "insufficient_pressure_neighbours"
	ReasonRegionalPressureIncoherent     = "regional_pressure_incoherent"
	ReasonRegionalPressureDeparture      = "regional_pressure_departure"
	ReasonNoSynchronizedSolution         = "no_synchronized_gnss_height"
	ReasonNoSignificantHeightChange      = "no_significant_gnss_height_change"
	ReasonLocalPressureAmbiguous         = "local_pressure_change_ambiguous"
	ReasonBarometricHeightDivergence     = "barometric_height_divergence"
)

// BarometerPolicy explicitly qualifies one installed pressure channel for the
// regional and GNSS-height checks. Presence enables those checks. Absolute sensor
// offset is irrelevant because the model uses changes; RelativeUncertaintyPa must
// cover repeatability, temperature effects and the qualified enclosure/vent.
type BarometerPolicy struct {
	Sensor                string  `toml:"sensor" json:"sensor"`
	Vented                bool    `toml:"vented" json:"vented"`
	RelativeUncertaintyPa float64 `toml:"relative_uncertainty_pa" json:"relative_uncertainty_pa"`
}

func (p BarometerPolicy) validate() error {
	switch p.Sensor {
	case "bmp388_bmp384", "bmp580", "bmp581", "ms5607":
	default:
		return &profileError{message: "barometer sensor must be bmp388_bmp384, bmp580, bmp581 or ms5607"}
	}
	if !p.Vented {
		return &profileError{message: "barometer must be explicitly qualified as vented"}
	}
	if err := positive("barometer relative_uncertainty_pa", p.RelativeUncertaintyPa); err != nil {
		return err
	}
	return nil
}

// profileError keeps BarometerPolicy validation messages independent of the
// configuration package while still satisfying error.
type profileError struct{ message string }

func (e *profileError) Error() string { return "integrity: " + e.message }

// BarometricProfile contains the fleet-context and physical-model operating
// points. All time alignment is against collector receipt time; live admission has
// already rejected old or unstamped push records.
type BarometricProfile struct {
	Window             time.Duration `json:"window_ns"`
	MaxTimeSkew        time.Duration `json:"max_time_skew_ns"`
	SampleMaxAge       time.Duration `json:"sample_max_age_ns"`
	SolutionMaxSkew    time.Duration `json:"solution_max_skew_ns"`
	RadiusM            float64       `json:"radius_m"`
	MinNeighbours      int           `json:"min_neighbours"`
	MaxRegionalSpreadM float64       `json:"max_regional_spread_m"`
	ModelUncertaintyM  float64       `json:"model_uncertainty_m"`

	RegionalDepartureM EnvironmentalBands `json:"regional_departure_m"`
	Movement           ScaledBands        `json:"movement"`
	Divergence         ScaledBands        `json:"divergence"`
}

// PressureObservation is one qualified local-pressure sample. Temperature and RH
// describe the enclosure sensor location, not outside ambient air. They are used
// only to estimate virtual temperature for a short relative-height conversion.
type PressureObservation struct {
	At                    time.Time
	PressurePa            float64
	TemperatureC          float64
	HumidityPercent       float64
	HasHumidity           bool
	RelativeUncertaintyPa float64
}

// PressureContext aligns one local pressure tendency with the median tendency of
// qualified neighbours. Log-pressure rate makes station elevation and absolute
// sensor offset cancel before stations are compared.
type PressureContext struct {
	Start, End                          PressureObservation
	RegionalLogRatePerSecond            float64
	RegionalSpreadLogRatePerSecond      float64
	RegionalUncertaintyLogRatePerSecond float64
	NeighbourCount                      int
	MaxNeighbourAge                     time.Duration
	MaxNeighbourDistanceM               float64
}

// PressureSample advances the pressure checks. Observation is retained even while
// fleet context is warming up so its synchronized GNSS height can anchor the next
// comparison. Context is nil when UnavailableReason explains why no comparison is
// possible.
type PressureSample struct {
	Received          time.Time
	Observation       *PressureObservation
	Context           *PressureContext
	UnavailableReason string
}

// PressureObservationFromEnvironment selects the explicitly configured sensor and
// combines it with valid enclosure temperature/RH. It never substitutes a different
// pressure channel when the configured one is missing or faulty.
func PressureObservationFromEnvironment(sample EnvironmentSample, policy BarometerPolicy) (PressureObservation, bool) {
	var pressure *float64
	for _, reading := range sample.Readings {
		if reading.Name != policy.Sensor || reading.Location != SensorEnclosure || !reading.Ready || reading.Fault || reading.ExtendedPressure {
			continue
		}
		pressure = reading.PressurePa
		break
	}
	if pressure == nil || !finite(*pressure) || *pressure <= 0 {
		return PressureObservation{}, false
	}
	aggregate := aggregateEnvironment(sample)
	if !aggregate.hasTemperature || !finite(aggregate.temperature) {
		return PressureObservation{}, false
	}
	out := PressureObservation{
		At: sample.Received, PressurePa: *pressure, TemperatureC: aggregate.temperature,
		RelativeUncertaintyPa: policy.RelativeUncertaintyPa,
	}
	if aggregate.hasHumidity && finite(aggregate.humidity) && aggregate.humidity >= 0 && aggregate.humidity <= 100 {
		out.HumidityPercent, out.HasHumidity = aggregate.humidity, true
	}
	return out, true
}

type barometricSolution struct {
	at      time.Time
	heightM float64
	vAccM   float64
}

type barometricChecks struct {
	recent  []barometricSolution
	anchors []barometricSolution
}

func (b *barometricChecks) reset() {
	b.recent = nil
	b.anchors = nil
}

func (b *barometricChecks) observeSolution(sol Solution, prof BarometricProfile) {
	cutoff := sol.Received.Add(-2 * prof.SolutionMaxSkew)
	i := 0
	for i < len(b.recent) && b.recent[i].at.Before(cutoff) {
		i++
	}
	b.recent = b.recent[i:]
	if !sol.Usable3D() || !finite(sol.HeightM) || !finite(sol.VAccM) || sol.VAccM <= 0 {
		return
	}
	b.recent = append(b.recent, barometricSolution{at: sol.Received, heightM: sol.HeightM, vAccM: sol.VAccM})
}

func (b *barometricChecks) evaluate(sample PressureSample, prof BarometricProfile) (Verdict, Verdict) {
	if sample.Observation != nil {
		if solution, ok := nearestSolution(b.recent, sample.Observation.At, prof.SolutionMaxSkew); ok {
			b.setAnchor(solution, sample.Observation.At)
		}
		cutoff := sample.Observation.At.Add(-(prof.Window + prof.MaxTimeSkew + prof.SampleMaxAge))
		i := 0
		for i < len(b.anchors) && b.anchors[i].at.Before(cutoff) {
			i++
		}
		b.anchors = b.anchors[i:]
	}
	if sample.Context == nil {
		reason := sample.UnavailableReason
		if reason == "" {
			reason = ReasonPressureMeasurementUnavailable
		}
		unavailable := Verdict{State: Unavailable, Reasons: []string{reason}}
		return unavailable, unavailable
	}

	calc, ok := pressurePhysics(*sample.Context)
	if !ok {
		unavailable := Verdict{State: Unavailable, Reasons: []string{ReasonPressureMeasurementUnavailable}}
		return unavailable, unavailable
	}
	regional := regionalPressureVerdict(*sample.Context, calc, prof)
	barometric := b.barometricHeightVerdict(*sample.Context, calc, regional.State, prof)
	return regional, barometric
}

func (b *barometricChecks) setAnchor(solution barometricSolution, pressureAt time.Time) {
	solution.at = pressureAt
	for i := range b.anchors {
		if b.anchors[i].at.Equal(pressureAt) {
			b.anchors[i] = solution
			return
		}
	}
	b.anchors = append(b.anchors, solution)
	sort.Slice(b.anchors, func(i, j int) bool { return b.anchors[i].at.Before(b.anchors[j].at) })
}

func nearestSolution(solutions []barometricSolution, at time.Time, maxSkew time.Duration) (barometricSolution, bool) {
	var best barometricSolution
	bestSkew := maxSkew + 1
	for _, solution := range solutions {
		skew := solution.at.Sub(at)
		if skew < 0 {
			skew = -skew
		}
		if skew <= maxSkew && skew < bestSkew {
			best, bestSkew = solution, skew
		}
	}
	return best, bestSkew <= maxSkew
}

type pressureCalculation struct {
	durationS, virtualTemperatureK                          float64
	localLogChange, regionalLogChange, residualLogChange    float64
	barometricDeltaM, regionalSpreadM, pressureUncertaintyM float64
}

func pressurePhysics(context PressureContext) (pressureCalculation, bool) {
	dt := context.End.At.Sub(context.Start.At).Seconds()
	if dt <= 0 || context.Start.PressurePa <= 0 || context.End.PressurePa <= 0 {
		return pressureCalculation{}, false
	}
	temperatureC := (context.Start.TemperatureC + context.End.TemperatureC) / 2
	humidity := 0.0
	hasHumidity := context.Start.HasHumidity && context.End.HasHumidity
	if hasHumidity {
		humidity = (context.Start.HumidityPercent + context.End.HumidityPercent) / 2
	}
	pressure := math.Sqrt(context.Start.PressurePa * context.End.PressurePa)
	tv := virtualTemperatureK(temperatureC, humidity, hasHumidity, pressure)
	if !finite(tv) || tv <= 0 {
		return pressureCalculation{}, false
	}
	const dryAirGasConstant = 287.05
	const standardGravity = 9.80665
	scale := dryAirGasConstant * tv / standardGravity
	localLog := math.Log(context.End.PressurePa / context.Start.PressurePa)
	regionalLog := context.RegionalLogRatePerSecond * dt
	residual := localLog - regionalLog
	localUncertaintyLog := math.Hypot(
		context.Start.RelativeUncertaintyPa/context.Start.PressurePa,
		context.End.RelativeUncertaintyPa/context.End.PressurePa,
	)
	regionalUncertaintyLog := context.RegionalUncertaintyLogRatePerSecond * dt
	return pressureCalculation{
		durationS: dt, virtualTemperatureK: tv,
		localLogChange: localLog, regionalLogChange: regionalLog, residualLogChange: residual,
		barometricDeltaM:     -scale * residual,
		regionalSpreadM:      math.Abs(scale * context.RegionalSpreadLogRatePerSecond * dt),
		pressureUncertaintyM: scale * math.Hypot(localUncertaintyLog, regionalUncertaintyLog),
	}, true
}

func virtualTemperatureK(temperatureC, humidityPercent float64, hasHumidity bool, pressurePa float64) float64 {
	t := temperatureC + 273.15
	if !hasHumidity || humidityPercent <= 0 {
		return t
	}
	// Magnus saturation vapour pressure over water. Humidity changes virtual
	// temperature by well under the explicit model-uncertainty floor here.
	es := 610.94 * math.Exp(17.625*temperatureC/(temperatureC+243.04))
	e := humidityPercent / 100 * es
	if e <= 0 || e >= pressurePa {
		return t
	}
	mixingRatio := 0.622 * e / (pressurePa - e)
	specificHumidity := mixingRatio / (1 + mixingRatio)
	return t * (1 + 0.61*specificHumidity)
}

func regionalPressureVerdict(context PressureContext, calc pressureCalculation, prof BarometricProfile) Verdict {
	v := Verdict{State: Assured, Metrics: pressureMetrics(context, calc), Thresholds: map[string]float64{
		"regional_departure_inconsistent_m": prof.RegionalDepartureM.Inconsistent,
		"regional_departure_unassured_m":    prof.RegionalDepartureM.Unassured,
		"regional_spread_max_m":             prof.MaxRegionalSpreadM,
	}}
	if calc.regionalSpreadM > prof.MaxRegionalSpreadM {
		v.State = Unavailable
		v.Reasons = []string{ReasonRegionalPressureIncoherent}
		return v
	}
	uncertainty := math.Hypot(calc.pressureUncertaintyM, calc.regionalSpreadM)
	inc := math.Max(prof.RegionalDepartureM.Inconsistent, 3*uncertainty)
	un := math.Max(prof.RegionalDepartureM.Unassured, 6*uncertainty)
	un = math.Max(un, inc)
	v.Thresholds["regional_departure_applied_inconsistent_m"] = inc
	v.Thresholds["regional_departure_applied_unassured_m"] = un
	departure := math.Abs(calc.barometricDeltaM)
	switch {
	case departure >= un:
		v.State = Unassured
		v.Reasons = []string{ReasonRegionalPressureDeparture}
	case departure >= inc:
		v.State = Inconsistent
		v.Reasons = []string{ReasonRegionalPressureDeparture}
	}
	return v
}

func (b *barometricChecks) barometricHeightVerdict(context PressureContext, calc pressureCalculation, regional State, prof BarometricProfile) Verdict {
	v := Verdict{State: Unavailable, Metrics: pressureMetrics(context, calc), Thresholds: map[string]float64{}}
	start, okStart := nearestSolution(b.anchors, context.Start.At, 0)
	end, okEnd := nearestSolution(b.anchors, context.End.At, 0)
	if !okStart || !okEnd || start.at.Equal(end.at) {
		v.Reasons = []string{ReasonNoSynchronizedSolution}
		return v
	}
	gnssDelta := end.heightM - start.heightM
	gnssAccuracy := math.Hypot(start.vAccM, end.vAccM)
	movementState, moveInc, moveUn := prof.Movement.classify(math.Abs(gnssDelta), gnssAccuracy)
	v.Metrics["gnss_height_change_m"] = gnssDelta
	v.Metrics["gnss_height_accuracy_rss_m"] = gnssAccuracy
	v.Metrics["barometric_height_change_m"] = calc.barometricDeltaM
	v.Thresholds["gnss_movement_inconsistent_m"] = moveInc
	v.Thresholds["gnss_movement_unassured_m"] = moveUn
	// A stable GNSS height does not make missing or incoherent weather context
	// usable. Preserve unknown instead of reporting a normal barometric check
	// from a pressure model whose regional correction failed qualification.
	if regional == Unavailable {
		v.Reasons = []string{ReasonRegionalPressureIncoherent}
		return v
	}
	if movementState == Assured {
		v.State = Assured
		v.Reasons = []string{ReasonNoSignificantHeightChange}
		return v
	}

	divergence := gnssDelta - calc.barometricDeltaM
	combinedAccuracy := math.Sqrt(gnssAccuracy*gnssAccuracy + calc.pressureUncertaintyM*calc.pressureUncertaintyM +
		calc.regionalSpreadM*calc.regionalSpreadM + prof.ModelUncertaintyM*prof.ModelUncertaintyM)
	divergenceState, divInc, divUn := prof.Divergence.classify(math.Abs(divergence), combinedAccuracy)
	v.Metrics["barometric_divergence_m"] = divergence
	v.Metrics["barometric_combined_uncertainty_m"] = combinedAccuracy
	v.Thresholds["barometric_divergence_inconsistent_m"] = divInc
	v.Thresholds["barometric_divergence_unassured_m"] = divUn
	if regional != Assured {
		if divergenceState == Assured {
			v.State = Assured
			return v
		}
		v.State = Unavailable
		v.Reasons = []string{ReasonLocalPressureAmbiguous}
		return v
	}
	v.State = divergenceState
	if divergenceState != Assured {
		v.Reasons = []string{ReasonBarometricHeightDivergence}
	}
	return v
}

func pressureMetrics(context PressureContext, calc pressureCalculation) map[string]float64 {
	return map[string]float64{
		"pressure_start_pa":                 context.Start.PressurePa,
		"pressure_end_pa":                   context.End.PressurePa,
		"pressure_interval_s":               calc.durationS,
		"enclosure_virtual_temperature_k":   calc.virtualTemperatureK,
		"local_log_pressure_change":         calc.localLogChange,
		"regional_log_pressure_change":      calc.regionalLogChange,
		"regional_pressure_residual_m":      calc.barometricDeltaM,
		"regional_pressure_spread_m":        calc.regionalSpreadM,
		"pressure_model_uncertainty_m":      calc.pressureUncertaintyM,
		"pressure_neighbour_count":          float64(context.NeighbourCount),
		"pressure_neighbour_max_age_s":      context.MaxNeighbourAge.Seconds(),
		"pressure_neighbour_max_distance_m": context.MaxNeighbourDistanceM,
	}
}
