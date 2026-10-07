package updates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/wire"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/controlauth"
)

const token = "update-test-credential-00000000000000001"

var enrolled = Device{"observer-1", "enrollment-1", "org-1", "collector-1"}

func context() identity.ObserverContext {
	return identity.ObserverContext{ObserverID: enrolled.Observer, EnrollmentID: enrolled.Enrollment, OrganizationID: enrolled.Organization, CollectorInstanceID: enrolled.Collector}
}
func setup(t *testing.T) (*Manager, Config) {
	t.Helper()
	hash := sha256.Sum256([]byte(token))
	base := t.TempDir()
	c := Config{StateFile: filepath.Join(base, "state.json"), Repository: filepath.Join(base, "repository"), OpenRepository: filepath.Join(base, "open-repository"), Principals: []Principal{{ID: "operator-1", TokenSHA256: hex.EncodeToString(hash[:]), Devices: []Device{enrolled}}}}
	m, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	m.now = func() time.Time { return time.Unix(1800000000, 0) }
	m.BeginSession(context(), "boot-1")
	if err = m.Report(context(), "boot-1", 1, wire.UpdateStatus{Mode: "manual", Channel: "lab", State: "idle"}); err != nil {
		t.Fatal(err)
	}
	return m, c
}
func TestDurableRequestsReplayScopeAndExpiry(t *testing.T) {
	m, c := setup(t)
	command := wire.UpdateCommand{Action: 2, Generation: 7, Release: 31}
	first, err := m.request("operator-1", enrolled, strings.Repeat("a", 32), command)
	if err != nil {
		t.Fatal(err)
	}
	data := m.Pending(context())
	if len(data) != 36 {
		t.Fatal("request was not queued")
	}
	wrong := context()
	wrong.EnrollmentID = "replacement"
	if m.Pending(wrong) != nil {
		t.Fatal("cross-enrollment command")
	}
	duplicate, err := m.request("operator-1", enrolled, first.RequestID, command)
	if err != nil || duplicate.Command.ID != first.Command.ID {
		t.Fatal("non-idempotent retry", err)
	}
	command.Release++
	if _, err = m.request("operator-1", enrolled, first.RequestID, command); err == nil {
		t.Fatal("conflicting UUID accepted")
	}
	command.Release--
	m.Close()
	reopened, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.now = m.now
	if string(reopened.Pending(context())) != string(data) {
		t.Fatal("pending bytes changed after restart")
	}
	reopened.BeginSession(context(), "boot-2")
	s := wire.UpdateStatus{LastCommand: first.Command.ID, State: "staged", Channel: "lab", Mode: "manual"}
	if err = reopened.Report(context(), "boot-2", 2, s); err != nil {
		t.Fatal(err)
	}
	if reopened.Pending(context()) != nil {
		t.Fatal("accepted command replayed")
	}
	s.State = "confirmed"
	if err = reopened.Report(context(), "boot-1", 999, s); err != nil {
		t.Fatal(err)
	}
	if reopened.records[enrolled.key()].Status.State != "staged" {
		t.Fatal("old session regressed current telemetry")
	}
	second, err := reopened.request("operator-1", enrolled, strings.Repeat("b", 32), wire.UpdateCommand{Action: 4})
	if err != nil {
		t.Fatal(err)
	}
	old, err := reopened.request("operator-1", enrolled, first.RequestID, command)
	if err != nil || old.Command.ID != first.Command.ID {
		t.Fatal("lost old idempotency record", err)
	}
	if reopened.records[enrolled.key()].Command.ID != second.Command.ID {
		t.Fatal("old request replaced current command")
	}
	// The device accepts the cancel. Its report names the highest command id it
	// accepted, which must not make the replayed, older request look accepted.
	s.LastCommand = second.Command.ID
	if err = reopened.Report(context(), "boot-2", 3, s); err != nil {
		t.Fatal(err)
	}
	statusOf := func(method, body string) string {
		t.Helper()
		target := "/gnss/api/v2/updates"
		if method == "GET" {
			target += "?observer_id=" + enrolled.Observer
		}
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		reopened.ServeHTTP(w, r)
		if w.Code != 200 && w.Code != 202 {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body.String())
		}
		var reply struct {
			RequestStatus string `json:"request_status"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		return reply.RequestStatus
	}
	replay := `{"observer_id":"observer-1","request_id":"` + first.RequestID + `","action":"download","generation":"7","release":"31"}`
	if got := statusOf("POST", replay); got != "superseded" {
		t.Fatalf("replayed older request reported %q, want superseded", got)
	}
	if got := statusOf("GET", ""); got != "accepted" {
		t.Fatalf("current accepted command reported %q, want accepted", got)
	}
	reopened.now = func() time.Time { return time.Unix(int64(second.Command.Expires), 0) }
	if reopened.Pending(context()) != nil {
		t.Fatal("expired command sent")
	}
	info, _ := os.Stat(c.StateFile)
	if info.Mode().Perm() != 0600 {
		t.Fatal("public command state")
	}
}
func TestHTTPRequiresUpdateGrantAndCanonicalFields(t *testing.T) {
	m, _ := setup(t)
	body := `{"observer_id":"observer-1","request_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","action":"check","generation":"0","release":"0"}`
	for _, tc := range []struct {
		token, origin, body string
		code                int
	}{
		{"", "", body, 401}, {"read-only-test-credential-00000000000000", "", body, 403},
		{token, "https://example.invalid", body, 403}, {token, "", strings.Replace(body, `"generation":"0"`, `"generation":0`, 1), 400},
		{token, "", strings.Replace(body, `"generation":"0"`, `"generation":"00"`, 1), 400},
		{token, "", strings.Replace(body, `"release":"0"`, `"release":"0","release":"1"`, 1), 400},
		{token, "", body, 202}, {token, "", body, 202},
	} {
		r := httptest.NewRequest("POST", "/gnss/api/v2/updates", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		failures := testutil.ToFloat64(controlauth.FailuresTotal.WithLabelValues("updates"))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("got %d wanted %d: %s", w.Code, tc.code, w.Body.String())
		}
		// A missing or wrong credential counts; an accepted one, and a
		// browser-origin refusal made before the credential is judged, do not.
		if tc.code == 401 || (tc.code == 403 && tc.origin == "") {
			failures++
		}
		if got := testutil.ToFloat64(controlauth.FailuresTotal.WithLabelValues("updates")); got != failures {
			t.Fatalf("%s: control auth failures = %v, want %v", tc.body, got, failures)
		}
		if w.Code == 202 {
			var reply struct {
				RequestStatus string `json:"request_status"`
			}
			json.Unmarshal(w.Body.Bytes(), &reply)
			if reply.RequestStatus != "requested" {
				t.Fatal("HTTP receipt falsely claimed accepted")
			}
		}
	}
}
func TestStorageFailureCannotQueueSideEffect(t *testing.T) {
	m, c := setup(t)
	m.config.StateFile = filepath.Join(c.StateFile, "not-a-directory")
	if _, err := m.request("operator-1", enrolled, strings.Repeat("a", 32), wire.UpdateCommand{Action: 1}); err == nil {
		t.Fatal("failed durable commit accepted")
	}
	if m.Pending(context()) != nil {
		t.Fatal("uncommitted command escaped")
	}
}

// A changed key id or a changed raw error code is a transition, although the
// collector's derived error name and every other reported field are the same.
func TestTransitionsFollowKeyIdsAndRawErrorCodes(t *testing.T) {
	m, _ := setup(t)
	status := wire.UpdateStatus{Mode: "manual", Channel: "lab", State: "idle", BootKey: strings.Repeat("a", 64), ReleaseKey: strings.Repeat("b", 64)}
	sequence := uint64(1)
	report := func(s wire.UpdateStatus) int {
		t.Helper()
		sequence++
		if err := m.Report(context(), "boot-1", sequence, s); err != nil {
			t.Fatal(err)
		}
		return len(m.records[enrolled.key()].Transitions)
	}
	transitions := report(status)
	if report(status) != transitions {
		t.Fatal("an unchanged report recorded a transition")
	}
	bootChanges := testutil.ToFloat64(keyChanges.WithLabelValues(enrolled.Observer, "secure_boot"))
	status.BootKey = strings.Repeat("c", 64)
	if report(status) != transitions+1 {
		t.Fatal("a changed Secure Boot key id recorded no transition")
	}
	if got := testutil.ToFloat64(keyChanges.WithLabelValues(enrolled.Observer, "secure_boot")); got != bootChanges+1 {
		t.Fatalf("secure_boot key changes = %v, want %v", got, bootChanges+1)
	}
	releaseChanges := testutil.ToFloat64(keyChanges.WithLabelValues(enrolled.Observer, "tuf_release"))
	status.ReleaseKey = strings.Repeat("d", 64)
	if report(status) != transitions+2 {
		t.Fatal("a changed TUF release key id recorded no transition")
	}
	if got := testutil.ToFloat64(keyChanges.WithLabelValues(enrolled.Observer, "tuf_release")); got != releaseChanges+1 {
		t.Fatalf("tuf_release key changes = %v, want %v", got, releaseChanges+1)
	}
	status.Layout = 2
	if report(status) != transitions+3 {
		t.Fatal("a changed partition layout recorded no transition")
	}
	// Two error codes the collector cannot name share one derived name and are
	// still two different failures.
	unknown := func(reason uint16) wire.UpdateStatus {
		s := status
		s.State, s.ErrorDomain, s.ErrorReason = "failed", 7, reason
		s.Error = wire.UpdateErrorName(s.ErrorDomain, s.ErrorReason)
		return s
	}
	if unknown(2).Error != "UPDATE_UNKNOWN_ERROR" || unknown(3).Error != unknown(2).Error {
		t.Fatalf("test needs two unknown codes with one name, got %q", unknown(2).Error)
	}
	failed := testutil.ToFloat64(failures.WithLabelValues("UPDATE_UNKNOWN_ERROR"))
	if report(unknown(2)) != transitions+4 {
		t.Fatal("first unknown error recorded no transition")
	}
	if report(unknown(3)) != transitions+5 {
		t.Fatal("a different unknown error code recorded no transition")
	}
	if got := testutil.ToFloat64(failures.WithLabelValues("UPDATE_UNKNOWN_ERROR")); got != failed+2 {
		t.Fatalf("unknown failures = %v, want %v", got, failed+2)
	}
}
