// Package stationcontrol distributes reception models without moving alarm ownership off the edge.
package stationcontrol

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/ptudor/navlistener/internal/controlauth"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/reception"
)

// A forecast that cannot be encoded reaches no station. The counter is the
// alertable signal; the log line, bounded per station, says which station and
// which companion so the forecast function can be reproduced.
var forecastEncodeFailures = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "navlistener_station_forecast_encode_failures_total",
	Help: "Reception forecasts (reception) or power companions (power) the collector computed but could not encode for a station; the station received none for that cadence.",
}, []string{"observer", "kind"})

const (
	// forecastEvery is the cadence at which a station receives a forecast; a
	// forecast that failed to encode is retried at the same cadence rather
	// than on every control poll, so a persistent fault does not burn orbit
	// propagation every few seconds.
	forecastEvery = 30 * time.Second
	// forecastWarnEvery bounds the encode-failure log per station.
	forecastWarnEvery = 5 * time.Minute
)

type Forecast func(identity.ObserverContext, reception.Site, time.Time) reception.Expectation
type powerForecast struct {
	base  reception.Expectation
	power reception.PowerExpectation
}
type station struct {
	site               reception.Site
	session            string
	receptionVersion   uint8
	lastControl        time.Time
	lastForecast       time.Time
	forecasts          []reception.Expectation
	powerForecasts     []powerForecast
	machine            reception.Machine
	remotePowerMachine reception.Machine
	jointPowerMachine  reception.Machine
	model              *reception.PowerModel
	modelRevision      uint64
	savedRevision      uint64
	request            uint64
	scopes             uint8
	expires            int64
	result             *reception.SnapshotResult
	lastUptime         uint64
	lastPowerUptime    uint64
	lastForecastWarn   time.Time
}
type Manager struct {
	mu       sync.Mutex
	stations map[string]*station
	forecast Forecast
	token    []byte
	auth     controlauth.Limiter
	log      *slog.Logger
}

// SetLogger directs this manager's log lines; without it they go through
// slog.Default.
func (m *Manager) SetLogger(log *slog.Logger) {
	m.mu.Lock()
	m.log = log
	m.mu.Unlock()
}

func (m *Manager) logger() *slog.Logger {
	if m.log != nil {
		return m.log
	}
	return slog.Default()
}

type PowerModelSnapshot struct {
	Observer string
	Revision uint64
	ModelID  uint64
	Data     []byte
}

func New(c reception.Config, f Forecast) *Manager {
	m := &Manager{stations: map[string]*station{}, forecast: f}
	m.token, _ = hex.DecodeString(c.OperatorTokenSHA256)
	for _, s := range c.Stations {
		m.stations[s.Observer] = &station{site: s, model: reception.NewPowerModel(s)}
	}
	return m
}

func (m *Manager) RestorePowerModel(observer string, expectedID uint64, data []byte) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stations[observer]
	if s == nil {
		return 0, reception.ErrPowerModel
	}
	model := reception.NewPowerModel(s.site)
	if err := model.UnmarshalBinary(data); err != nil {
		return 0, err
	}
	if expectedID != 0 && model.ModelID() != expectedID {
		return 0, reception.ErrPowerModel
	}
	s.model = model
	s.modelRevision, s.savedRevision = 1, 1
	return model.ModelID(), nil
}

func (m *Manager) PowerModels(dirtyOnly bool) ([]PowerModelSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PowerModelSnapshot, 0, len(m.stations))
	for observer, s := range m.stations {
		if dirtyOnly && s.modelRevision == s.savedRevision {
			continue
		}
		data, err := s.model.MarshalBinary()
		if err != nil {
			return nil, err
		}
		out = append(out, PowerModelSnapshot{Observer: observer, Revision: s.modelRevision, ModelID: s.model.ModelID(), Data: data})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Observer < out[j].Observer })
	return out, nil
}

func (m *Manager) MarkPowerModelSaved(observer string, revision uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.stations[observer]; s != nil && revision > s.savedRevision && revision <= s.modelRevision {
		s.savedRevision = revision
	}
}
func (m *Manager) BeginSession(c identity.ObserverContext, session string, version uint8) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.stations[c.ObserverID]; s != nil {
		s.session = session
		s.receptionVersion = version
		s.lastForecast = time.Time{}
		s.forecasts = nil
		s.powerForecasts = nil
		s.machine = reception.Machine{Alarm: s.machine.Alarm}
		s.remotePowerMachine = reception.Machine{Alarm: s.remotePowerMachine.Alarm}
		s.jointPowerMachine = reception.Machine{Alarm: s.jointPowerMachine.Alarm}
		s.lastUptime = 0
		s.lastPowerUptime = 0
	}
}
func (m *Manager) Pending(c identity.ObserverContext, session string, now time.Time) ([]byte, []byte, []byte) {
	m.mu.Lock()
	s := m.stations[c.ObserverID]
	if s == nil || s.session != session {
		m.mu.Unlock()
		return nil, nil, nil
	}
	s.lastControl = now
	due := s.lastForecast.IsZero() || now.Sub(s.lastForecast) >= forecastEvery
	site := s.site
	if due {
		s.lastForecast = now
	}
	m.mu.Unlock()
	// Orbit propagation must not block other stations' reports or controls.
	var e reception.Expectation
	var forecast, power, request []byte
	var encodeErr error
	if due {
		e = m.forecast(c, site, now)
		forecast, encodeErr = e.Encode()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.session != session {
		return nil, nil, nil
	}
	if due {
		if s.lastForecast != now {
			return nil, nil, nil
		}
		if encodeErr != nil {
			// The station gets no forecast this cadence. lastForecast stays at
			// now, so the next attempt is a cadence away rather than on the
			// next control poll: a persistent fault is retried, not spun on.
			m.forecastFailed(s, c.ObserverID, "reception", encodeErr, now)
		} else {
			s.forecasts = append(s.forecasts, e)
			if len(s.forecasts) > 12 {
				s.forecasts = s.forecasts[1:]
			}
			if s.receptionVersion >= 2 {
				p := s.model.Forecast(e)
				var powerErr error
				if power, powerErr = p.Encode(); powerErr != nil {
					m.forecastFailed(s, c.ObserverID, "power", powerErr, now)
				} else {
					s.powerForecasts = append(s.powerForecasts, powerForecast{base: e, power: p})
					if len(s.powerForecasts) > 12 {
						s.powerForecasts = s.powerForecasts[1:]
					}
				}
			}
		}
	}
	if s.request != 0 && s.result == nil && s.expires > now.Unix() {
		request = make([]byte, 20)
		request[0] = 1
		request[1] = s.scopes
		binary.BigEndian.PutUint64(request[4:], s.request)
		binary.BigEndian.PutUint64(request[12:], uint64(s.expires))
	}
	return forecast, power, request
}

// forecastFailed counts a forecast the station will not receive and logs it,
// at most once per forecastWarnEvery per station. Called with m.mu held.
func (m *Manager) forecastFailed(s *station, observer, kind string, err error, now time.Time) {
	forecastEncodeFailures.WithLabelValues(observer, kind).Inc()
	if !s.lastForecastWarn.IsZero() && now.Sub(s.lastForecastWarn) < forecastWarnEvery {
		return
	}
	s.lastForecastWarn = now
	m.logger().Warn("station forecast could not be encoded; the station receives none this cadence",
		"observer", observer, "kind", kind, "error", err, "retry_after", forecastEvery)
}

// ObservePower trains only configured, well-above-mask satellite observations.
// NAV-SAT supplies satellite-level C/N0, so per-signal cells remain explicitly
// unknown until signal-level receiver telemetry is available.
func (m *Manager) ObservePower(observer string, at time.Time, observations []reception.PowerObservation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stations[observer]
	if s == nil || at.Unix() < 946684800 || at.Unix() >= 4102444800 {
		return
	}
	for _, observation := range observations {
		if observation.Signal != reception.Satellite || !s.site.AllowsGNSS(observation.GNSS) ||
			observation.Elevation < 0 || observation.Elevation > 90 ||
			float64(observation.Elevation) < s.site.Elevation+2 {
			continue
		}
		if s.model.Observe(at.Unix(), observation) {
			s.modelRevision++
		}
	}
}

func bitmapConstellations(base reception.Expectation, count uint8, bits [16]byte) uint8 {
	if int(count) != len(base.Entries) {
		return 0
	}
	var mask uint8
	for i, entry := range base.Entries {
		if bits[i/8]&(1<<uint(i%8)) != 0 {
			mask |= 1 << entry.GNSS
		}
	}
	return mask
}

// CheckPower recomputes the delivered-model comparison from the reported C/N0
// values. Local and remote history must agree before JointAlarm can advance.
func (m *Manager) CheckPower(c identity.ObserverContext, session string, sample reception.PowerSample, now time.Time) *reception.PowerCheck {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stations[c.ObserverID]
	if s == nil || s.session != session {
		return nil
	}
	check := &reception.PowerCheck{LocalModelID: sample.LocalModelID, RemoteModelID: sample.RemoteModelID}
	at := time.Unix(sample.Unix, 0)
	if now.Sub(at) > 15*time.Second || at.Sub(now) > 5*time.Second {
		s.remotePowerMachine = reception.Machine{Alarm: s.remotePowerMachine.Alarm}
		s.jointPowerMachine = reception.Machine{Alarm: s.jointPowerMachine.Alarm}
		return check
	}
	for _, forecast := range s.powerForecasts {
		if forecast.base.ID != sample.ExpectationID || forecast.power.ModelID != sample.RemoteModelID {
			continue
		}
		remote := reception.ComparePower(forecast.power, sample)
		check.Modeled, check.Anomalous, check.RemoteValid, check.RemoteAbnormal = reception.CountPower(forecast.base, sample.Count, remote)
		if sample.Flags&reception.PowerFlagLocal != 0 {
			_, _, check.LocalValid, check.LocalAbnormal = reception.CountPower(forecast.base, sample.Count, sample.LocalAssessment)
		}
		var joint, conflict, disagreement reception.PowerAssessment
		for i := range joint.Valid {
			joint.Valid[i] = remote.Valid[i] & sample.LocalAssessment.Valid[i]
			joint.Bad[i] = remote.Bad[i] & sample.LocalAssessment.Bad[i] & joint.Valid[i]
			conflict.Bad[i] = (remote.Bad[i] ^ sample.LocalAssessment.Bad[i]) & joint.Valid[i]
			disagreement.Bad[i] = (remote.Valid[i] ^ sample.RemoteAssessment.Valid[i]) |
				((remote.Bad[i] ^ sample.RemoteAssessment.Bad[i]) & remote.Valid[i])
		}
		_, _, check.JointValid, check.JointAbnormal = reception.CountPower(forecast.base, sample.Count, joint)
		check.ModelConflict = bitmapConstellations(forecast.base, sample.Count, conflict.Bad)
		check.ReportDisagreement = bitmapConstellations(forecast.base, sample.Count, disagreement.Bad)
		s.remotePowerMachine.RetainCoverage(check.RemoteValid)
		s.jointPowerMachine.RetainCoverage(check.JointValid)
		if sample.UptimeMS > s.lastPowerUptime {
			check.RemoteAlarm = s.remotePowerMachine.Step(check.RemoteValid, check.RemoteAbnormal, at, forecast.base.AlarmSeconds, forecast.base.ClearSeconds)
			check.JointAlarm = s.jointPowerMachine.Step(check.JointValid, check.JointAbnormal, at, forecast.base.AlarmSeconds, forecast.base.ClearSeconds)
			s.lastPowerUptime = sample.UptimeMS
		} else {
			check.RemoteAlarm = s.remotePowerMachine.Alarm
			check.JointAlarm = s.jointPowerMachine.Alarm
		}
		check.ReportDisagreement |= (check.RemoteValid ^ sample.RemoteValid) | (check.RemoteAlarm ^ sample.RemoteAlarm)
		return check
	}
	s.remotePowerMachine = reception.Machine{Alarm: s.remotePowerMachine.Alarm}
	s.jointPowerMachine = reception.Machine{Alarm: s.jointPowerMachine.Alarm}
	return check
}
func (m *Manager) Check(c identity.ObserverContext, session string, sample reception.Sample, now time.Time) *reception.Check {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stations[c.ObserverID]
	if s == nil || s.session != session {
		return nil
	}
	check := &reception.Check{Alarm: s.machine.Alarm, PerSignal: s.site.PerSignal}
	at := time.Unix(sample.Unix, 0)
	if now.Sub(at) > 15*time.Second || at.Sub(now) > 5*time.Second {
		s.machine = reception.Machine{Alarm: s.machine.Alarm}
		return check
	}
	for _, e := range s.forecasts {
		if e.ID != sample.ExpectationID {
			continue
		}
		var bad uint8
		check.Expected, check.Observed, check.Valid, bad = reception.Counts(e, sample)
		s.machine.RetainCoverage(check.Valid)
		if check.Valid == 0 {
			s.machine = reception.Machine{Alarm: s.machine.Alarm}
		} else if sample.UptimeMS > s.lastUptime {
			check.Alarm = s.machine.Step(check.Valid, bad, at, e.AlarmSeconds, e.ClearSeconds)
			s.lastUptime = sample.UptimeMS
		}
		check.Disagreement = (check.Alarm ^ sample.Alarm) & check.Valid
		return check
	}
	s.machine = reception.Machine{Alarm: s.machine.Alarm}
	return check
}
func (m *Manager) SnapshotResult(c identity.ObserverContext, session string, r reception.SnapshotResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stations[c.ObserverID]
	if s != nil && s.session == session && s.request == r.ID && r.ID != 0 && r.Scopes & ^s.scopes == 0 {
		s.result = &r
	}
}

// Controls use a separate operator credential. Read-audience credentials cannot issue commands.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	auth := r.Header.Get("Authorization")
	digest := sha256.Sum256([]byte(strings.TrimPrefix(auth, "Bearer ")))
	// A browser-origin request is refused before its credential is judged, so
	// it is not counted as a guessed one.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "station control credential required", http.StatusUnauthorized)
		return
	}
	if len(m.token) != 32 || len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(auth, "Bearer ") || subtle.ConstantTimeCompare(digest[:], m.token) != 1 {
		m.auth.Failed("station-snapshot", r)
		http.Error(w, "station control credential required", http.StatusUnauthorized)
		return
	}
	var q struct {
		Observer string `json:"observer_id"`
		ID       string `json:"request_id"`
		Scopes   uint8  `json:"scopes"`
	}
	if r.Method == http.MethodGet {
		q.Observer = r.URL.Query().Get("observer_id")
	} else if r.Method == http.MethodPost && r.Header.Get("Content-Type") == "application/json" && r.URL.RawQuery == "" {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		d.DisallowUnknownFields()
		if d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid request", 400)
			return
		}
	} else {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "unsupported request", 405)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stations[q.Observer]
	if s == nil {
		http.Error(w, "station has no reception profile", 404)
		return
	}
	now := time.Now()
	if r.Method == http.MethodPost {
		id, err := strconv.ParseUint(q.ID, 10, 64)
		if err != nil || id == 0 || strconv.FormatUint(id, 10) != q.ID || q.Scopes == 0 || q.Scopes&^7 != 0 {
			http.Error(w, "request_id must be a positive decimal string; scopes must be 1..7", 400)
			return
		}
		if now.Sub(s.lastControl) > 15*time.Second {
			http.Error(w, "station control session unavailable", 409)
			return
		}
		if id != s.request {
			if s.request != 0 && s.result == nil && s.expires > now.Unix() {
				http.Error(w, "snapshot already pending", 409)
				return
			}
			s.request = id
			s.scopes = q.Scopes
			s.expires = now.Unix() + 30
			s.result = nil
		} else if q.Scopes != s.scopes {
			http.Error(w, "request_id already has different scopes", 409)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
	state := "idle"
	if s.request != 0 {
		state = "pending"
		if s.result != nil {
			state = "reported"
		} else if s.expires <= now.Unix() {
			state = "expired"
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"observer_id": q.Observer, "request_id": strconv.FormatUint(s.request, 10), "state": state, "result": s.result})
}
