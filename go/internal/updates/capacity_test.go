package updates

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/wire"
)

// principalsGranting builds n principals that each grant the same devices.
func principalsGranting(n int, devices []Device) []Principal {
	out := make([]Principal, 0, n)
	for i := 0; i < n; i++ {
		hash := sha256.Sum256([]byte(fmt.Sprintf("principal-%d", i)))
		out = append(out, Principal{ID: fmt.Sprintf("operator-%d", i), TokenSHA256: hex.EncodeToString(hash[:]), Devices: devices})
	}
	return out
}

func devicesNumbered(from, to int) []Device {
	out := make([]Device, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, Device{fmt.Sprintf("observer-%d", i), "enrollment-1", "org-1", "collector-1"})
	}
	return out
}

// TestValidateBoundsDistinctDevices: the state holds one record and one
// session per device, so a configuration granting more distinct devices than
// that is refused up front instead of enrolling devices that silently never get
// a session, a record or a command. The same device granted to several
// principals is one device.
func TestValidateBoundsDistinctDevices(t *testing.T) {
	base := Config{StateFile: "/var/db/navlistener/updates.json", Repository: "/var/db/navlistener/repository"}
	full := base
	for i := 0; i < 8; i++ {
		full.Principals = append(full.Principals, principalsGranting(1, devicesNumbered(i*128, (i+1)*128))...)
		full.Principals[i].ID = fmt.Sprintf("operator-%d", i)
		hash := sha256.Sum256([]byte(fmt.Sprintf("principal-%d", i)))
		full.Principals[i].TokenSHA256 = hex.EncodeToString(hash[:])
	}
	if err := full.Validate(); err != nil {
		t.Fatalf("exactly %d distinct devices refused: %v", maxDevices, err)
	}
	over := full
	over.Principals = append(append([]Principal(nil), full.Principals...), principalsGranting(1, devicesNumbered(1024, 1025))...)
	over.Principals[8].ID = "operator-8"
	hash := sha256.Sum256([]byte("principal-8"))
	over.Principals[8].TokenSHA256 = hex.EncodeToString(hash[:])
	err := over.Validate()
	if err == nil || !strings.Contains(err.Error(), "1025") || !strings.Contains(err.Error(), "at most 1024") {
		t.Fatalf("1025 distinct devices: err = %v, want a capacity refusal naming both counts", err)
	}
	shared := base
	shared.Principals = principalsGranting(16, devicesNumbered(0, 128))
	if err := shared.Validate(); err != nil {
		t.Fatalf("16 principals sharing 128 devices (2048 grants, 128 devices) refused: %v", err)
	}
}

// TestSessionCapacityRefusalIsVisible: a device turned away at the session cap
// used to vanish — no log, no counter, Report answered nil forever. The refusal
// is now counted and logged, and a device that already holds a session keeps
// it.
func TestSessionCapacityRefusalIsVisible(t *testing.T) {
	hash := sha256.Sum256([]byte(token))
	second := Device{"observer-2", "enrollment-1", "org-1", "collector-1"}
	base := t.TempDir()
	c := Config{StateFile: filepath.Join(base, "state.json"), Repository: filepath.Join(base, "repository"),
		Principals: []Principal{{ID: "operator-1", TokenSHA256: hex.EncodeToString(hash[:]), Devices: []Device{enrolled, second}}}}
	m, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.BeginSession(context(), "boot-1")
	for i := len(m.active); i < maxDevices; i++ {
		m.active[fmt.Sprintf("filler-%d", i)] = "boot"
	}
	before := testutil.ToFloat64(capacityRefusals.WithLabelValues("sessions"))
	secondContext := identity.ObserverContext{ObserverID: second.Observer, EnrollmentID: second.Enrollment, OrganizationID: second.Organization, CollectorInstanceID: second.Collector}
	m.BeginSession(secondContext, "boot-2")
	if m.active[second.key()] != "" {
		t.Fatal("device admitted past the session capacity")
	}
	if got := testutil.ToFloat64(capacityRefusals.WithLabelValues("sessions")); got != before+1 {
		t.Fatalf("sessions refusal counter = %v, want %v", got, before+1)
	}
	if err := m.Report(secondContext, "boot-2", 1, wire.UpdateStatus{Mode: "manual", Channel: "lab", State: "idle"}); err != nil {
		t.Fatalf("report from a refused device: %v", err)
	}
	if _, ok := m.records[second.key()]; ok {
		t.Fatal("refused device acquired a record")
	}
	m.BeginSession(context(), "boot-1b")
	if m.active[enrolled.key()] != "boot-1b" {
		t.Fatal("a device holding a session could not renew it at capacity")
	}
}

// TestCommitDoesNotHoldTheLockDuringTheWrite: the marshal, write and fsync of
// a transition run with only the writers' lock held, so every other push
// connection's Pending and BeginSession — and the operator's GET — complete
// while a commit is in flight. The committed transition is still durable.
func TestCommitDoesNotHoldTheLockDuringTheWrite(t *testing.T) {
	m, c := setup(t)
	entered, release := make(chan struct{}), make(chan struct{})
	m.writeHook = func() { close(entered); <-release }
	reported := make(chan error, 1)
	go func() {
		reported <- m.Report(context(), "boot-1", 2, wire.UpdateStatus{Mode: "manual", Channel: "lab", State: "checking"})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("commit never reached the write")
	}
	readers := make(chan struct{})
	go func() {
		defer close(readers)
		_ = m.Pending(context())
		m.BeginSession(context(), "boot-1")
		m.mu.Lock()
		_ = m.records[enrolled.key()]
		m.mu.Unlock()
	}()
	select {
	case <-readers:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("readers blocked behind an in-flight commit")
	}
	// The write has not been published: readers still see the previous state.
	m.mu.Lock()
	state := m.records[enrolled.key()].Status.State
	m.mu.Unlock()
	if state != "idle" {
		t.Fatalf("unpublished transition visible: %s", state)
	}
	close(release)
	select {
	case err := <-reported:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("report did not complete after the write was released")
	}
	m.mu.Lock()
	r := m.records[enrolled.key()]
	m.mu.Unlock()
	if r.Status.State != "checking" || len(r.Transitions) != 2 {
		t.Fatalf("published record: %+v", r)
	}
	durable, err := loadRecords(c.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := durable[enrolled.key()]; got.Status == nil || got.Status.State != "checking" || len(got.Transitions) != 2 {
		t.Fatalf("durable record: %+v", got)
	}
}
