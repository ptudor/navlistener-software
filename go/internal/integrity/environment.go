package integrity

import (
	"math"
	"slices"
	"sort"
	"time"
)

const (
	// CheckEnvironmentHealth reports instrumentation health independently of the
	// conditions the instruments measure.
	CheckEnvironmentHealth = "environment_sensor_health"
	// CheckEnvironmentChange reports a station-local environmental departure. It is
	// advisory context, not RF-interference or spoofing evidence.
	CheckEnvironmentChange = "environment_change"
)

var (
	environmentHealthInfo = Info{Name: CheckEnvironmentHealth, Version: 1, Domain: DomainEnvironment, Advisory: true, UnknownImmediate: true}
	environmentChangeInfo = Info{Name: CheckEnvironmentChange, Version: 1, Domain: DomainEnvironment, Advisory: true, UnknownImmediate: true}
)

const (
	ReasonNoEnvironmentSensors = "no_environment_sensors"
	ReasonNoEnvironmentValue   = "no_environment_measurement"
	ReasonEnvironmentWarmup    = "environment_baseline_warming_up"
	ReasonSensorNotReady       = "environment_sensor_not_ready"
	ReasonSensorInvalid        = "environment_sensor_invalid"
	ReasonSensorFault          = "environment_sensor_fault"
	ReasonSensorSaturated      = "environment_sensor_saturated"
	ReasonSensorFlatline       = "environment_sensor_flatline"
	ReasonSensorDisagreement   = "environment_sensor_disagreement"
	ReasonPressureExtended     = "pressure_extended_range"
	ReasonHumidityHeater       = "humidity_heater_active"
	ReasonTemperatureChange    = "temperature_change"
	ReasonHumidityChange       = "humidity_change"
	ReasonPressureChange       = "pressure_change"
	ReasonTemperatureRate      = "temperature_rate"
	ReasonHumidityRate         = "humidity_rate"
	ReasonPressureRate         = "pressure_rate"
	ReasonTemperatureSustained = "temperature_sustained_change"
	ReasonHumiditySustained    = "humidity_sustained_change"
	ReasonPressureSustained    = "pressure_sustained_change"
	ReasonDewPointSpread       = "low_dew_point_spread"
)

// SensorLocation states what a temperature or humidity reading physically
// describes. Only enclosure-air readings are compared with one another or used for
// the station environmental baseline.
type SensorLocation string

const (
	SensorEnclosure SensorLocation = "enclosure"
	SensorBoard     SensorLocation = "board"
	SensorRegulator SensorLocation = "regulator"
	SensorRTC       SensorLocation = "rtc"
)

// EnvironmentalReading is one physical sensor, kept separate so disagreement and
// validity are not hidden by an aggregate. Expected fields describe channels the
// hardware record says should exist. Fault is a positive device/calibration failure,
// not merely a temporarily unavailable conversion.
type EnvironmentalReading struct {
	Name     string
	Location SensorLocation

	ExpectedTemperature bool
	ExpectedHumidity    bool
	ExpectedPressure    bool
	Ready               bool
	Fault               bool
	ExtendedPressure    bool

	TemperatureC    *float64
	HumidityPercent *float64
	PressurePa      *float64
}

// EnvironmentSample is one accepted board report. Received is the collector clock
// used for all persistence and hysteresis. UptimeMS detects a board restart without
// trusting its wall clock. HeaterActive prevents an intentionally heated HDC channel
// from training or clearing an environmental baseline.
type EnvironmentSample struct {
	Received     time.Time
	Session      string
	UptimeMS     uint64
	Readings     []EnvironmentalReading
	HeaterActive bool
}

// EnvironmentalBands are absolute inconsistent and unassured bands in the unit
// named by the profile field that contains them.
type EnvironmentalBands struct {
	Inconsistent float64 `json:"inconsistent"`
	Unassured    float64 `json:"unassured"`
}

func (b EnvironmentalBands) classify(value float64) State {
	switch {
	case value >= b.Unassured:
		return Unassured
	case value >= b.Inconsistent:
		return Inconsistent
	default:
		return Assured
	}
}

// EnvironmentProfile is the complete environmental operating profile. Change and
// rate bands are deliberately advisory; only the later barometric consistency model
// may contribute independent physical evidence to GNSS assurance.
type EnvironmentProfile struct {
	StaleAfter         time.Duration `json:"stale_after_ns"`
	BaselineWindow     time.Duration `json:"baseline_window_ns"`
	BaselineMinAge     time.Duration `json:"baseline_min_age_ns"`
	BaselineMinSamples int           `json:"baseline_min_samples"`
	PersistenceWindow  time.Duration `json:"persistence_window_ns"`
	PersistenceMinAge  time.Duration `json:"persistence_min_age_ns"`
	RateMinInterval    time.Duration `json:"rate_min_interval_ns"`
	RateMaxInterval    time.Duration `json:"rate_max_interval_ns"`
	FlatlineAfter      time.Duration `json:"flatline_after_ns"`
	FlatlineMinSamples int           `json:"flatline_min_samples"`

	TemperatureChangeC       EnvironmentalBands `json:"temperature_change_c"`
	HumidityChangePercent    EnvironmentalBands `json:"humidity_change_percent"`
	PressureChangePa         EnvironmentalBands `json:"pressure_change_pa"`
	TemperatureRateCPerHour  EnvironmentalBands `json:"temperature_rate_c_per_hour"`
	HumidityRatePctPerHour   EnvironmentalBands `json:"humidity_rate_percent_per_hour"`
	PressureRatePaPerHour    EnvironmentalBands `json:"pressure_rate_pa_per_hour"`
	TemperatureDisagreementC EnvironmentalBands `json:"temperature_disagreement_c"`
	HumidityDisagreementPct  EnvironmentalBands `json:"humidity_disagreement_percent"`
	// DewPointSpreadC is air temperature minus dew point. Its bands are reversed:
	// values at or below Unassured are more severe than values at or below
	// Inconsistent. This is proximity to saturation, not proof of condensation.
	DewPointSpreadInconsistentC float64 `json:"dew_point_spread_inconsistent_c"`
	DewPointSpreadUnassuredC    float64 `json:"dew_point_spread_unassured_c"`
}

type environmentPoint struct {
	at                                       time.Time
	temperature, humidity, pressure          float64
	hasTemperature, hasHumidity, hasPressure bool
}

type flatlineState struct {
	value   float64
	since   time.Time
	samples int
	set     bool
}

type environmentChecks struct {
	history    []environmentPoint
	previous   *environmentPoint
	flatlines  map[string]flatlineState
	haveUptime bool
	uptimeMS   uint64
	session    string
	anomalous  bool
}

func (e *environmentChecks) reset() {
	e.history = nil
	e.previous = nil
	e.flatlines = nil
	e.anomalous = false
}

func (e *environmentChecks) evaluate(sample EnvironmentSample, prof EnvironmentProfile) (Verdict, Verdict) {
	if e.session != "" && sample.Session != "" && sample.Session != e.session || e.haveUptime && sample.UptimeMS < e.uptimeMS {
		e.reset()
	}
	if sample.Session != "" {
		e.session = sample.Session
	}
	e.haveUptime, e.uptimeMS = true, sample.UptimeMS
	health := e.health(sample, prof)
	current := aggregateEnvironment(sample)
	change := e.change(current, sample, health.State, prof)
	return health, change
}

func (e *environmentChecks) health(sample EnvironmentSample, prof EnvironmentProfile) Verdict {
	v := Verdict{State: Assured, Metrics: map[string]float64{}, Thresholds: map[string]float64{
		"flatline_after_s":                           prof.FlatlineAfter.Seconds(),
		"flatline_min_samples":                       float64(prof.FlatlineMinSamples),
		"temperature_disagreement_inconsistent_c":    prof.TemperatureDisagreementC.Inconsistent,
		"temperature_disagreement_unassured_c":       prof.TemperatureDisagreementC.Unassured,
		"humidity_disagreement_inconsistent_percent": prof.HumidityDisagreementPct.Inconsistent,
		"humidity_disagreement_unassured_percent":    prof.HumidityDisagreementPct.Unassured,
	}}
	if len(sample.Readings) == 0 {
		v.State, v.Reasons = Unavailable, []string{ReasonNoEnvironmentSensors}
		return v
	}
	if e.flatlines == nil {
		e.flatlines = make(map[string]flatlineState)
	}
	var temperatures, humidities []float64
	invalid, faults, saturated, flatlined, extended := 0, 0, 0, 0, 0
	for _, r := range sample.Readings {
		if r.Fault {
			faults++
		}
		expected := r.ExpectedTemperature || r.ExpectedHumidity || r.ExpectedPressure
		if expected && !r.Ready {
			invalid++
		}
		if r.ExpectedTemperature {
			if r.TemperatureC == nil {
				if r.Ready && !(sample.HeaterActive && r.Name == "hdc") {
					invalid++
				}
				e.clearFlatline(r.Name + "/temperature")
			} else {
				flatlined += e.observeFlatline(r.Name+"/temperature", *r.TemperatureC, sample.Received, prof)
				if r.Location == SensorEnclosure {
					temperatures = append(temperatures, *r.TemperatureC)
				}
			}
		}
		if r.ExpectedHumidity {
			if r.HumidityPercent == nil {
				if r.Ready && !(sample.HeaterActive && r.Name == "hdc") {
					invalid++
				}
				e.clearFlatline(r.Name + "/humidity")
			} else {
				flatlined += e.observeFlatline(r.Name+"/humidity", *r.HumidityPercent, sample.Received, prof)
				if *r.HumidityPercent <= 0 || *r.HumidityPercent >= 100 {
					saturated++
				}
				if r.Location == SensorEnclosure {
					humidities = append(humidities, *r.HumidityPercent)
				}
			}
		}
		if r.ExpectedPressure {
			if r.PressurePa == nil {
				if r.Ready {
					invalid++
				}
				e.clearFlatline(r.Name + "/pressure")
			} else {
				flatlined += e.observeFlatline(r.Name+"/pressure", *r.PressurePa, sample.Received, prof)
			}
			if r.ExtendedPressure {
				extended++
			}
		}
	}
	v.Metrics["sensors"] = float64(len(sample.Readings))
	v.Metrics["invalid_channels"] = float64(invalid)
	v.Metrics["faulted_sensors"] = float64(faults)
	v.Metrics["saturated_channels"] = float64(saturated)
	v.Metrics["flatlined_channels"] = float64(flatlined)
	v.Metrics["extended_pressure_channels"] = float64(extended)
	if sample.HeaterActive {
		v.Metrics["humidity_heater_active"] = 1
	}
	tempSpread := spread(temperatures)
	rhSpread := spread(humidities)
	if len(temperatures) >= 2 {
		v.Metrics["temperature_spread_c"] = tempSpread
	}
	if len(humidities) >= 2 {
		v.Metrics["humidity_spread_percent"] = rhSpread
	}
	setHealth := func(state State, reason string) {
		v.State = worse(v.State, state)
		v.Reasons = appendReason(v.Reasons, reason)
	}
	if invalid > 0 {
		setHealth(Inconsistent, ReasonSensorInvalid)
		if slices.ContainsFunc(sample.Readings, func(r EnvironmentalReading) bool {
			return !r.Ready && (r.ExpectedTemperature || r.ExpectedHumidity || r.ExpectedPressure)
		}) {
			v.Reasons = appendReason(v.Reasons, ReasonSensorNotReady)
		}
	}
	if faults > 0 {
		setHealth(Unassured, ReasonSensorFault)
	}
	if saturated > 0 {
		setHealth(Inconsistent, ReasonSensorSaturated)
	}
	if flatlined > 0 {
		setHealth(Unassured, ReasonSensorFlatline)
	}
	if extended > 0 {
		setHealth(Inconsistent, ReasonPressureExtended)
	}
	if state := prof.TemperatureDisagreementC.classify(tempSpread); len(temperatures) >= 2 && state != Assured {
		setHealth(state, ReasonSensorDisagreement)
	}
	if state := prof.HumidityDisagreementPct.classify(rhSpread); len(humidities) >= 2 && state != Assured {
		setHealth(state, ReasonSensorDisagreement)
	}
	if sample.HeaterActive {
		v.Reasons = appendReason(v.Reasons, ReasonHumidityHeater)
	}
	return v
}

func (e *environmentChecks) observeFlatline(key string, value float64, at time.Time, prof EnvironmentProfile) int {
	f := e.flatlines[key]
	if !f.set || f.value != value || at.Before(f.since) {
		e.flatlines[key] = flatlineState{value: value, since: at, samples: 1, set: true}
		return 0
	}
	f.samples++
	e.flatlines[key] = f
	if f.samples >= prof.FlatlineMinSamples && at.Sub(f.since) >= prof.FlatlineAfter {
		return 1
	}
	return 0
}

func (e *environmentChecks) clearFlatline(key string) { delete(e.flatlines, key) }

func aggregateEnvironment(sample EnvironmentSample) environmentPoint {
	p := environmentPoint{at: sample.Received}
	var temperatures, humidities, pressures []float64
	for _, r := range sample.Readings {
		if !r.Ready || r.Fault || r.Location != SensorEnclosure {
			continue
		}
		if r.TemperatureC != nil && !(sample.HeaterActive && r.Name == "hdc") {
			temperatures = append(temperatures, *r.TemperatureC)
		}
		if r.HumidityPercent != nil && !(sample.HeaterActive && r.Name == "hdc") {
			humidities = append(humidities, *r.HumidityPercent)
		}
		if r.PressurePa != nil && !r.ExtendedPressure {
			pressures = append(pressures, *r.PressurePa)
		}
	}
	if len(temperatures) > 0 {
		p.temperature, p.hasTemperature = median(temperatures), true
	}
	if len(humidities) > 0 {
		p.humidity, p.hasHumidity = median(humidities), true
	}
	if len(pressures) > 0 {
		p.pressure, p.hasPressure = median(pressures), true
	}
	return p
}

func (e *environmentChecks) change(current environmentPoint, sample EnvironmentSample, health State, prof EnvironmentProfile) Verdict {
	v := Verdict{State: Assured, Metrics: map[string]float64{}, Thresholds: map[string]float64{
		"temperature_change_inconsistent_c":           prof.TemperatureChangeC.Inconsistent,
		"temperature_change_unassured_c":              prof.TemperatureChangeC.Unassured,
		"humidity_change_inconsistent_percent":        prof.HumidityChangePercent.Inconsistent,
		"humidity_change_unassured_percent":           prof.HumidityChangePercent.Unassured,
		"pressure_change_inconsistent_pa":             prof.PressureChangePa.Inconsistent,
		"pressure_change_unassured_pa":                prof.PressureChangePa.Unassured,
		"temperature_rate_inconsistent_c_per_hour":    prof.TemperatureRateCPerHour.Inconsistent,
		"temperature_rate_unassured_c_per_hour":       prof.TemperatureRateCPerHour.Unassured,
		"humidity_rate_inconsistent_percent_per_hour": prof.HumidityRatePctPerHour.Inconsistent,
		"humidity_rate_unassured_percent_per_hour":    prof.HumidityRatePctPerHour.Unassured,
		"pressure_rate_inconsistent_pa_per_hour":      prof.PressureRatePaPerHour.Inconsistent,
		"pressure_rate_unassured_pa_per_hour":         prof.PressureRatePaPerHour.Unassured,
		"dew_point_spread_inconsistent_c":             prof.DewPointSpreadInconsistentC,
		"dew_point_spread_unassured_c":                prof.DewPointSpreadUnassuredC,
	}}
	if !current.hasTemperature && !current.hasHumidity && !current.hasPressure {
		v.State, v.Reasons = Unavailable, []string{ReasonNoEnvironmentValue}
		return v
	}
	if current.hasTemperature {
		v.Metrics["temperature_c"] = current.temperature
	}
	if current.hasHumidity {
		v.Metrics["humidity_percent"] = current.humidity
	}
	if current.hasPressure {
		v.Metrics["pressure_pa"] = current.pressure
	}
	if current.hasTemperature && current.hasHumidity && current.humidity > 0 {
		dew := dewPointC(current.temperature, current.humidity)
		spread := current.temperature - dew
		v.Metrics["dew_point_c"] = dew
		v.Metrics["dew_point_spread_c"] = spread
		switch {
		case spread <= prof.DewPointSpreadUnassuredC:
			v.State = worse(v.State, Unassured)
			v.Reasons = appendReason(v.Reasons, ReasonDewPointSpread)
		case spread <= prof.DewPointSpreadInconsistentC:
			v.State = worse(v.State, Inconsistent)
			v.Reasons = appendReason(v.Reasons, ReasonDewPointSpread)
		}
	}
	instantDegraded := v.State != Assured

	if !e.anomalous {
		e.prune(current.at.Add(-prof.PersistenceWindow))
	}
	baselineHistory := historyAfter(e.history, current.at.Add(-prof.BaselineWindow))
	ready := len(baselineHistory) >= prof.BaselineMinSamples && current.at.Sub(baselineHistory[0].at) >= prof.BaselineMinAge
	persistentHistory := historyBefore(e.history, current.at.Add(-prof.PersistenceMinAge))
	persistent := len(persistentHistory) >= prof.BaselineMinSamples
	if ready {
		baseline := aggregateHistory(baselineHistory)
		compareChange(&v, current.hasTemperature && baseline.hasTemperature, math.Abs(current.temperature-baseline.temperature), "temperature_delta_c", prof.TemperatureChangeC, ReasonTemperatureChange)
		compareChange(&v, current.hasHumidity && baseline.hasHumidity, math.Abs(current.humidity-baseline.humidity), "humidity_delta_percent", prof.HumidityChangePercent, ReasonHumidityChange)
		compareChange(&v, current.hasPressure && baseline.hasPressure, math.Abs(current.pressure-baseline.pressure), "pressure_delta_pa", prof.PressureChangePa, ReasonPressureChange)
		if baseline.hasTemperature {
			v.Metrics["temperature_baseline_c"] = baseline.temperature
		}
		if baseline.hasHumidity {
			v.Metrics["humidity_baseline_percent"] = baseline.humidity
		}
		if baseline.hasPressure {
			v.Metrics["pressure_baseline_pa"] = baseline.pressure
		}
	}
	if persistent {
		baseline := aggregateHistory(persistentHistory)
		compareChange(&v, current.hasTemperature && baseline.hasTemperature, math.Abs(current.temperature-baseline.temperature), "temperature_sustained_delta_c", prof.TemperatureChangeC, ReasonTemperatureSustained)
		compareChange(&v, current.hasHumidity && baseline.hasHumidity, math.Abs(current.humidity-baseline.humidity), "humidity_sustained_delta_percent", prof.HumidityChangePercent, ReasonHumiditySustained)
		compareChange(&v, current.hasPressure && baseline.hasPressure, math.Abs(current.pressure-baseline.pressure), "pressure_sustained_delta_pa", prof.PressureChangePa, ReasonPressureSustained)
		if baseline.hasTemperature {
			v.Metrics["temperature_persistent_baseline_c"] = baseline.temperature
		}
		if baseline.hasHumidity {
			v.Metrics["humidity_persistent_baseline_percent"] = baseline.humidity
		}
		if baseline.hasPressure {
			v.Metrics["pressure_persistent_baseline_pa"] = baseline.pressure
		}
	}
	if !ready && !persistent {
		if !instantDegraded {
			v.State = Unavailable
		}
		v.Reasons = appendReason(v.Reasons, ReasonEnvironmentWarmup)
		v.Metrics["baseline_samples"] = float64(len(e.history))
	}

	if previous := e.previous; previous != nil {
		dt := current.at.Sub(previous.at)
		if dt >= prof.RateMinInterval && dt <= prof.RateMaxInterval {
			hours := dt.Hours()
			compareChange(&v, current.hasTemperature && previous.hasTemperature, math.Abs(current.temperature-previous.temperature)/hours, "temperature_rate_c_per_hour", prof.TemperatureRateCPerHour, ReasonTemperatureRate)
			compareChange(&v, current.hasHumidity && previous.hasHumidity, math.Abs(current.humidity-previous.humidity)/hours, "humidity_rate_percent_per_hour", prof.HumidityRatePctPerHour, ReasonHumidityRate)
			compareChange(&v, current.hasPressure && previous.hasPressure, math.Abs(current.pressure-previous.pressure)/hours, "pressure_rate_pa_per_hour", prof.PressureRatePaPerHour, ReasonPressureRate)
		}
	}
	copy := current
	e.previous = &copy
	// Invalid instrumentation, deliberate heater activity, or an environmental
	// departure cannot train the reference that will judge the next report.
	if health == Assured && !sample.HeaterActive {
		switch v.State {
		case Inconsistent, Unassured:
			e.anomalous = true
		case Assured:
			e.anomalous = false
			e.prune(current.at.Add(-prof.PersistenceWindow))
			e.history = append(e.history, current)
		case Unavailable:
			if !instantDegraded {
				e.anomalous = false
				e.prune(current.at.Add(-prof.PersistenceWindow))
				e.history = append(e.history, current)
			}
		}
	}
	return v
}

func compareChange(v *Verdict, available bool, value float64, metric string, bands EnvironmentalBands, reason string) {
	if !available || !finite(value) {
		return
	}
	v.Metrics[metric] = value
	if state := bands.classify(value); state != Assured {
		v.State = worse(v.State, state)
		v.Reasons = appendReason(v.Reasons, reason)
	}
}

func (e *environmentChecks) prune(cutoff time.Time) {
	i := 0
	for i < len(e.history) && e.history[i].at.Before(cutoff) {
		i++
	}
	e.history = e.history[i:]
}

func historyAfter(history []environmentPoint, cutoff time.Time) []environmentPoint {
	i := 0
	for i < len(history) && history[i].at.Before(cutoff) {
		i++
	}
	return history[i:]
}

func historyBefore(history []environmentPoint, cutoff time.Time) []environmentPoint {
	i := 0
	for i < len(history) && !history[i].at.After(cutoff) {
		i++
	}
	return history[:i]
}

func aggregateHistory(history []environmentPoint) environmentPoint {
	var temperatures, humidities, pressures []float64
	for _, p := range history {
		if p.hasTemperature {
			temperatures = append(temperatures, p.temperature)
		}
		if p.hasHumidity {
			humidities = append(humidities, p.humidity)
		}
		if p.hasPressure {
			pressures = append(pressures, p.pressure)
		}
	}
	var out environmentPoint
	if len(temperatures) > 0 {
		out.temperature, out.hasTemperature = median(temperatures), true
	}
	if len(humidities) > 0 {
		out.humidity, out.hasHumidity = median(humidities), true
	}
	if len(pressures) > 0 {
		out.pressure, out.hasPressure = median(pressures), true
	}
	return out
}

func spread(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	lo, hi := values[0], values[0]
	for _, value := range values[1:] {
		lo, hi = min(lo, value), max(hi, value)
	}
	return hi - lo
}

func median(values []float64) float64 {
	copy := append([]float64(nil), values...)
	sort.Float64s(copy)
	middle := len(copy) / 2
	if len(copy)%2 == 1 {
		return copy[middle]
	}
	return (copy[middle-1] + copy[middle]) / 2
}

// dewPointC uses the Magnus form for water over the ordinary environmental range.
// It describes saturation proximity at the sensor. Condensation still requires a
// relevant surface at or below this temperature.
func dewPointC(temperatureC, humidityPercent float64) float64 {
	const a, b = 17.625, 243.04
	gamma := math.Log(humidityPercent/100) + a*temperatureC/(b+temperatureC)
	return b * gamma / (a - gamma)
}
