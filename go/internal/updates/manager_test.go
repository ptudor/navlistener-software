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
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("got %d wanted %d: %s", w.Code, tc.code, w.Body.String())
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
