package updates

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/wire"
)

func TestUnverifiedTrustedReportIsDriftOnlyWhereEvidenceIsVerified(t *testing.T) {
	locked := wire.UpdateStatus{Profile: "trusted", Security: 31}
	for _, c := range []struct {
		name     string
		status   wire.UpdateStatus
		trust    identity.HardwareTrust
		verifies bool
		want     bool
	}{
		{"verified trusted", locked, identity.HardwareTrustTrusted, true, false},
		{"reports trusted, proved nothing", locked, identity.HardwareTrustNone, true, true},
		{"reports trusted, commissioned open", locked, identity.HardwareTrustOpen, true, true},
		{"collector verifies nothing", locked, identity.HardwareTrustNone, false, false},
		{"open build needs no proof", wire.UpdateStatus{Profile: "open", Security: 16}, identity.HardwareTrustNone, true, false},
		{"test build needs no proof", wire.UpdateStatus{Profile: "test", Security: 16}, identity.HardwareTrustTest, true, false},
	} {
		if got := unverifiedTrusted(c.status, string(c.trust), c.verifies); got != c.want {
			t.Errorf("%s: unverifiedTrusted = %t, want %t", c.name, got, c.want)
		}
	}
}

func TestReportRecordsVerifiedHardwareTrustBesideTheDeviceTrack(t *testing.T) {
	m, _ := setup(t)
	m.SetHardwareVerification(true)
	status := wire.UpdateStatus{Mode: "manual", Channel: "lab", State: "idle", Profile: "trusted", Security: 31}

	trusted := context()
	trusted.HardwareTrust = identity.HardwareTrustTrusted
	trusted.CommissioningFingerprint = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	// Evidence is session-scoped and never part of the enrollment identity: a
	// verified session reports into the same device record.
	if device(trusted) != enrolled {
		t.Fatalf("session evidence changed the device key: %+v", device(trusted))
	}
	m.BeginSession(trusted, "boot-2")
	if err := m.Report(trusted, "boot-2", 1, status); err != nil {
		t.Fatal(err)
	}
	record := m.records[enrolled.key()]
	if record.HardwareTrust != "trusted" || record.Status.Profile != "trusted" {
		t.Fatalf("record = trust %q, profile %q", record.HardwareTrust, record.Status.Profile)
	}
	if got := testutil.ToFloat64(securityDrift.WithLabelValues(enrolled.Observer)); got != 0 {
		t.Fatalf("verified trusted device drifts: %v", got)
	}
	if got := testutil.ToFloat64(hardwareTrust.WithLabelValues(enrolled.Observer, "trusted")); got != 1 {
		t.Fatalf("hardware trust info = %v", got)
	}
	transitions := len(record.Transitions)

	// The same device report over a session that proved nothing is a recorded
	// transition and a drift finding, although the device's account is unchanged.
	m.BeginSession(context(), "boot-3")
	if err := m.Report(context(), "boot-3", 1, status); err != nil {
		t.Fatal(err)
	}
	record = m.records[enrolled.key()]
	if record.HardwareTrust != "none" || len(record.Transitions) != transitions+1 {
		t.Fatalf("record = trust %q with %d transitions, want none and %d", record.HardwareTrust, len(record.Transitions), transitions+1)
	}
	if last := record.Transitions[len(record.Transitions)-1]; last.HardwareTrust != "none" || last.Status.Profile != "trusted" {
		t.Fatalf("transition = %+v", last)
	}
	if got := testutil.ToFloat64(securityDrift.WithLabelValues(enrolled.Observer)); got != 1 {
		t.Fatalf("unverified trusted report did not drift: %v", got)
	}
	if got := testutil.CollectAndCount(hardwareTrust); got != 1 {
		t.Fatalf("superseded hardware trust label retained: %d series", got)
	}

	// The read API serves the verified value beside, never inside, the status.
	request := httptest.NewRequest("GET", "/?observer_id="+enrolled.Observer, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	m.ServeHTTP(response, request)
	var body struct {
		Record struct {
			HardwareTrust string         `json:"hardware_trust"`
			Status        map[string]any `json:"status"`
		} `json:"record"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Record.HardwareTrust != "none" || body.Record.Status["trust_profile"] != "trusted" {
		t.Fatalf("served record = %+v", body.Record)
	}
	if _, inside := body.Record.Status["hardware_trust"]; inside {
		t.Fatal("verified trust was served inside the device-reported status")
	}
}
