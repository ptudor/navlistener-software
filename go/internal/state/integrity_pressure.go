package state

import (
	"math"
	"time"

	"github.com/ptudor/navlistener/internal/integrity"
)

// integrityPressure records one qualified local sample, assembles a regional
// pressure tendency from surveyed peers, and advances the two pressure checks. The
// caller holds rfMu.
func (s *Store) integrityPressure(id string, st *integrityStation, sample integrity.EnvironmentSample, health integrity.State) {
	pressure := integrity.PressureSample{Received: sample.Received}
	if s.integrityCfg == nil {
		pressure.UnavailableReason = integrity.ReasonBarometerNotConfigured
		st.eval.ApplyPressure(pressure)
		return
	}
	station, configured := s.integrityCfg.stations[id]
	if !configured || station.Barometer == nil {
		pressure.UnavailableReason = integrity.ReasonBarometerNotConfigured
		st.eval.ApplyPressure(pressure)
		return
	}
	if sample.Session != "" && st.pressureSession != "" && sample.Session != st.pressureSession {
		st.pressure = nil
	}
	if sample.Session != "" {
		st.pressureSession = sample.Session
	}
	if health != integrity.Assured {
		pressure.UnavailableReason = integrity.ReasonPressureSensorUnhealthy
		st.eval.ApplyPressure(pressure)
		return
	}
	observation, ok := integrity.PressureObservationFromEnvironment(sample, *station.Barometer)
	if !ok {
		pressure.UnavailableReason = integrity.ReasonPressureMeasurementUnavailable
		st.eval.ApplyPressure(pressure)
		return
	}
	pressure.Observation = &observation
	st.pressure = appendPressureObservation(st.pressure, observation)
	prof := s.integrityCfg.profile.Barometric
	cutoff := observation.At.Add(-(2*prof.Window + 2*prof.MaxTimeSkew + prof.SampleMaxAge))
	for len(st.pressure) > 0 && st.pressure[0].At.Before(cutoff) {
		st.pressure = st.pressure[1:]
	}
	context, reason := s.regionalPressureContext(id, observation, prof)
	pressure.Context, pressure.UnavailableReason = context, reason
	st.eval.ApplyPressure(pressure)
}

func appendPressureObservation(history []integrity.PressureObservation, observation integrity.PressureObservation) []integrity.PressureObservation {
	if len(history) == 0 {
		return append(history, observation)
	}
	last := &history[len(history)-1]
	switch {
	case observation.At.After(last.At):
		return append(history, observation)
	case observation.At.Equal(last.At):
		*last = observation
	}
	return history
}

func pressureTendency(history []integrity.PressureObservation, end integrity.PressureObservation, prof integrity.BarometricProfile) (integrity.PressureObservation, float64, float64, bool) {
	var best integrity.PressureObservation
	bestError := prof.MaxTimeSkew + 1
	for _, candidate := range history {
		interval := end.At.Sub(candidate.At)
		if interval <= 0 {
			continue
		}
		error := interval - prof.Window
		if error < 0 {
			error = -error
		}
		if error <= prof.MaxTimeSkew && error < bestError {
			best, bestError = candidate, error
		}
	}
	if bestError > prof.MaxTimeSkew || best.PressurePa <= 0 || end.PressurePa <= 0 {
		return integrity.PressureObservation{}, 0, 0, false
	}
	durationS := end.At.Sub(best.At).Seconds()
	rate := math.Log(end.PressurePa/best.PressurePa) / durationS
	uncertaintyRate := math.Hypot(
		best.RelativeUncertaintyPa/best.PressurePa,
		end.RelativeUncertaintyPa/end.PressurePa,
	) / durationS
	return best, rate, uncertaintyRate, true
}

func (s *Store) regionalPressureContext(id string, current integrity.PressureObservation, prof integrity.BarometricProfile) (*integrity.PressureContext, string) {
	station := s.integrity[id]
	if station == nil {
		return nil, integrity.ReasonPressureHistoryWarmup
	}
	start, _, _, ok := pressureTendency(station.pressure, current, prof)
	if !ok {
		return nil, integrity.ReasonPressureHistoryWarmup
	}
	here := s.integrityCfg.stations[id].Position
	if here == nil {
		return nil, integrity.ReasonBarometerNotConfigured
	}

	var rates, uncertaintyRates []float64
	maxAge := time.Duration(0)
	maxDistance := 0.0
	for otherID, otherProfile := range s.integrityCfg.stations {
		if otherID == id || otherProfile.Barometer == nil || otherProfile.Position == nil {
			continue
		}
		distance := integrity.SurveyedDistanceM(*here, *otherProfile.Position)
		if distance > prof.RadiusM {
			continue
		}
		other := s.integrity[otherID]
		if other == nil || len(other.pressure) == 0 {
			continue
		}
		end := other.pressure[len(other.pressure)-1]
		age := current.At.Sub(end.At)
		if age < 0 || age > prof.SampleMaxAge {
			continue
		}
		otherStart, rate, uncertaintyRate, ok := pressureTendency(other.pressure, end, prof)
		if !ok || absoluteDuration(otherStart.At.Sub(start.At)) > prof.SampleMaxAge {
			continue
		}
		rates = append(rates, rate)
		uncertaintyRates = append(uncertaintyRates, uncertaintyRate)
		maxAge = max(maxAge, age)
		maxDistance = math.Max(maxDistance, distance)
	}
	if len(rates) < prof.MinNeighbours {
		return nil, integrity.ReasonInsufficientPressureNeighbours
	}
	regional := medianFloat(rates)
	spread := 0.0
	for _, rate := range rates {
		spread = math.Max(spread, math.Abs(rate-regional))
	}
	uncertainty := 0.0
	for _, rate := range uncertaintyRates {
		uncertainty = math.Max(uncertainty, rate)
	}
	return &integrity.PressureContext{
		Start: start, End: current,
		RegionalLogRatePerSecond: regional, RegionalSpreadLogRatePerSecond: spread,
		RegionalUncertaintyLogRatePerSecond: uncertainty,
		NeighbourCount:                      len(rates), MaxNeighbourAge: maxAge, MaxNeighbourDistanceM: maxDistance,
	}, ""
}

func absoluteDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}
