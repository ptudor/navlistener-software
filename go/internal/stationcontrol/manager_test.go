package stationcontrol

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/reception"
)

func fixture(now time.Time) (*Manager, identity.ObserverContext) {
	digest := sha256.Sum256([]byte("control-test"))
	c := reception.Config{OperatorTokenSHA256: hex.EncodeToString(digest[:]), Stations: []reception.Site{{Observer: "edge"}}}
	m := New(c, func(_ identity.ObserverContext, _ reception.Site, at time.Time) reception.Expectation {
		e := reception.Expectation{ID: uint64(at.Unix()), Issued: at.Unix(), RadiusM: 1000, AlarmSeconds: 5, ClearSeconds: 5, MinExpected: 4, MinMissing: 3, MissingPercent: 50}
		for sv := uint8(1); sv <= 12; sv++ {
			e.Entries = append(e.Entries, reception.Entry{GNSS: 0, SV: sv, Signal: reception.Satellite, Slots: 31})
		}
		return e
	})
	cx := identity.ObserverContext{ObserverID: "edge"}
	m.BeginSession(cx, "session")
	m.Pending(cx, "session", now)
	return m, cx
}

func TestIndependentAlarmAndUnknownRecovery(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m, cx := fixture(now)
	s := reception.Sample{ExpectationID: uint64(now.Unix()), Valid: 1, Expected: [8]uint8{99}, Observed: [8]uint8{99}}
	s.Matched[0] = 3 // two of twelve, even though the edge reports no alarm and false counts
	check := func(second int, valid uint8, id uint64) *reception.Check {
		s.Unix = now.Unix() + int64(second)
		s.UptimeMS = uint64(second+1) * 1000
		s.Valid = valid
		s.ExpectationID = id
		return m.Check(cx, "session", s, now.Add(time.Duration(second)*time.Second))
	}
	for second := 0; second <= 5; second++ {
		check(second, 1, uint64(now.Unix()))
	}
	got := check(6, 1, uint64(now.Unix()))
	if got.Alarm != 1 || got.Disagreement != 1 || got.Expected[0] != 12 || got.Observed[0] != 2 {
		t.Fatalf("independent deficit: %+v", got)
	}
	s.Matched[0] = 255
	s.Matched[1] = 15
	check(7, 1, uint64(now.Unix()))
	check(10, 0, uint64(now.Unix())) // unknown interrupts recovery
	if got = check(13, 1, uint64(now.Unix())); got.Alarm != 1 {
		t.Fatal("unknown cleared alarm")
	}
	check(15, 1, 123) // unknown forecast interrupts recovery too
	for second := 16; second < 21; second++ {
		if check(second, 1, uint64(now.Unix())).Alarm != 1 {
			t.Fatal("recovery dwell ended early")
		}
	}
	if check(21, 1, uint64(now.Unix())).Alarm != 0 {
		t.Fatal("sustained recovery did not clear")
	}
	if m.Check(cx, "other-session", s, now) != nil {
		t.Fatal("accepted another session")
	}
}

func TestRepeatedMeasurementDoesNotAdvanceAlarm(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m, cx := fixture(now)
	s := reception.Sample{ExpectationID: uint64(now.Unix()), Valid: 1, UptimeMS: 1000}
	for second := 0; second < 12; second++ {
		s.Unix = now.Unix() + int64(second)
		if got := m.Check(cx, "session", s, now.Add(time.Duration(second)*time.Second)); got.Alarm != 0 {
			t.Fatal("repeated measurement advanced dwell")
		}
	}
	// A delayed sample cannot finish onset, and neither can a subsequent fresh sample.
	s.UptimeMS = 2000
	s.Unix = now.Unix() + 3
	m.Check(cx, "session", s, now.Add(30*time.Second))
	s.UptimeMS = 31000
	s.Unix = now.Unix() + 30
	if m.Check(cx, "session", s, now.Add(30*time.Second)).Alarm != 0 {
		t.Fatal("stale report bridged onset")
	}
}

func TestSnapshotControls(t *testing.T) {
	now := time.Now()
	m, cx := fixture(now)
	call := func(method, body, token, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/gnss/api/v2/station-snapshot", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		return w
	}
	q := `{"observer_id":"edge","request_id":"42","scopes":7}`
	if w := call("POST", q, "read-credential", ""); w.Code != 401 {
		t.Fatalf("read credential: %d", w.Code)
	}
	if w := call("POST", q, "control-test", "https://example.invalid"); w.Code != 401 {
		t.Fatalf("browser control: %d", w.Code)
	}
	for _, bad := range []string{strings.Replace(q, `"42"`, `42`, 1), strings.Replace(q, `"42"`, `"042"`, 1), strings.Replace(q, `:7`, `:8`, 1), q + `{}`} {
		if w := call("POST", bad, "control-test", ""); w.Code != 400 {
			t.Fatalf("bad command accepted: %s %d", bad, w.Code)
		}
	}
	for i := 0; i < 2; i++ {
		if w := call("POST", q, "control-test", ""); w.Code != 202 {
			t.Fatalf("idempotent request: %d %s", w.Code, w.Body.String())
		}
	}
	_, frame := m.Pending(cx, "session", now.Add(time.Second))
	if len(frame) != 20 || frame[1] != 7 || binary.BigEndian.Uint64(frame[4:]) != 42 {
		t.Fatalf("command frame: %x", frame)
	}
	if _, b := m.Pending(cx, "old-session", now); len(b) != 0 {
		t.Fatal("command escaped active session")
	}
	if w := call("POST", strings.Replace(q, `"42"`, `"43"`, 1), "control-test", ""); w.Code != 409 {
		t.Fatal("replaced pending command")
	}
	m.SnapshotResult(cx, "old-session", reception.SnapshotResult{ID: 42, Status: 1, Scopes: 7})
	if m.stations["edge"].result != nil {
		t.Fatal("stale result accepted")
	}
	m.SnapshotResult(cx, "session", reception.SnapshotResult{ID: 42, Status: 2, Scopes: 3})
	if _, b := m.Pending(cx, "session", now.Add(2*time.Second)); len(b) != 0 {
		t.Fatal("completed command resent")
	}
	if w := call("POST", strings.Replace(q, `:7`, `:1`, 1), "control-test", ""); w.Code != 409 {
		t.Fatal("reused command ID with new scopes")
	}
	if w := call("POST", strings.Replace(q, `"42"`, `"43"`, 1), "control-test", ""); w.Code != 202 {
		t.Fatal("new request after completion rejected")
	}
	if _, b := m.Pending(cx, "session", now.Add(time.Minute)); len(b) != 0 {
		t.Fatal("expired command sent")
	}
}
