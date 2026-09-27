// Package stationcontrol distributes reception models without moving alarm ownership off the edge.
package stationcontrol

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/reception"
)

type Forecast func(identity.ObserverContext, reception.Site, time.Time) reception.Expectation
type station struct {
	site         reception.Site
	session      string
	lastControl  time.Time
	lastForecast time.Time
	forecasts    []reception.Expectation
	machine      reception.Machine
	request      uint64
	scopes       uint8
	expires      int64
	result       *reception.SnapshotResult
	lastUptime   uint64
}
type Manager struct {
	mu       sync.Mutex
	stations map[string]*station
	forecast Forecast
	token    []byte
}

func New(c reception.Config, f Forecast) *Manager {
	m := &Manager{stations: map[string]*station{}, forecast: f}
	m.token, _ = hex.DecodeString(c.OperatorTokenSHA256)
	for _, s := range c.Stations {
		m.stations[s.Observer] = &station{site: s}
	}
	return m
}
func (m *Manager) BeginSession(c identity.ObserverContext, session string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.stations[c.ObserverID]; s != nil {
		s.session = session
		s.lastForecast = time.Time{}
		s.forecasts = nil
		s.machine = reception.Machine{Alarm: s.machine.Alarm}
		s.lastUptime = 0
	}
}
func (m *Manager) Pending(c identity.ObserverContext, session string, now time.Time) ([]byte, []byte) {
	m.mu.Lock()
	s := m.stations[c.ObserverID]
	if s == nil || s.session != session {
		m.mu.Unlock()
		return nil, nil
	}
	s.lastControl = now
	due := s.lastForecast.IsZero() || now.Sub(s.lastForecast) >= 30*time.Second
	site := s.site
	if due {
		s.lastForecast = now
	}
	m.mu.Unlock()
	// Orbit propagation must not block other stations' reports or controls.
	var e reception.Expectation
	var forecast, request []byte
	if due {
		e = m.forecast(c, site, now)
		forecast, _ = e.Encode()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.session != session {
		return nil, nil
	}
	if due {
		if s.lastForecast != now {
			return nil, nil
		}
		if len(forecast) != 0 {
			s.forecasts = append(s.forecasts, e)
			if len(s.forecasts) > 12 {
				s.forecasts = s.forecasts[1:]
			}
		} else {
			s.lastForecast = time.Time{}
		}
	}
	if s.request != 0 && s.result == nil && s.expires > now.Unix() {
		request = make([]byte, 20)
		request[0] = 1
		request[1] = s.scopes
		binary.BigEndian.PutUint64(request[4:], s.request)
		binary.BigEndian.PutUint64(request[12:], uint64(s.expires))
	}
	return forecast, request
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
	if len(m.token) != 32 || r.Header.Get("Origin") != "" || len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(auth, "Bearer ") || subtle.ConstantTimeCompare(digest[:], m.token) != 1 {
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
