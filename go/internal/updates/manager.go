// Package updates retains operator requests and device-reported transitions.
// It cannot authorize firmware: every device verifies its own signed repository.
package updates

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ptudor/navlistener/internal/controlauth"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/wire"
)

type Device struct {
	Observer     string `toml:"observer_id" json:"observer_id"`
	Enrollment   string `toml:"enrollment_id" json:"enrollment_id"`
	Organization string `toml:"organization_id" json:"organization_id"`
	Collector    string `toml:"collector_instance_id" json:"collector_instance_id"`
}

func (d Device) key() string { b, _ := json.Marshal(d); return string(b) }
func device(c identity.ObserverContext) Device {
	return Device{c.ObserverID, c.EnrollmentID, c.OrganizationID, c.CollectorInstanceID}
}

type Principal struct {
	ID          string   `toml:"id"`
	TokenSHA256 string   `toml:"token_sha256"`
	Devices     []Device `toml:"device"`
}

// Each release track is a separate published tree. repository_path is the
// trusted track's; a collector may serve either track or both.
type Config struct {
	StateFile      string      `toml:"state_file"`
	Repository     string      `toml:"repository_path"`
	OpenRepository string      `toml:"open_repository_path"`
	Principals     []Principal `toml:"principal"`
}
type Transition struct {
	At     time.Time         `json:"at"`
	Status wire.UpdateStatus `json:"status"`
	// HardwareTrust is what the collector verified for the session that made
	// this report. It sits beside the status rather than inside it because the
	// status is the device's own account and this is not.
	HardwareTrust string `json:"hardware_trust,omitempty"`
}
type RequestAudit struct {
	ID      string             `json:"request_id"`
	Actor   string             `json:"actor"`
	Created time.Time          `json:"created"`
	Command wire.UpdateCommand `json:"command"`
}
type Record struct {
	Device    Device             `json:"device"`
	Command   wire.UpdateCommand `json:"command"`
	RequestID string             `json:"request_id"`
	Actor     string             `json:"actor"`
	Created   time.Time          `json:"created"`
	Status    *wire.UpdateStatus `json:"status,omitempty"`
	// HardwareTrust is the collector-verified hardware trust of the session that
	// made the latest report (none, open, test, trusted). Status.Profile beside
	// it is the device's own label; only this value is evidence. Empty until a
	// report has been received.
	HardwareTrust string         `json:"hardware_trust,omitempty"`
	Received      time.Time      `json:"received"`
	Session       string         `json:"session"`
	Sequence      uint64         `json:"sequence,string"`
	Transitions   []Transition   `json:"transitions"`
	Requests      []RequestAudit `json:"requests"`
}

// State capacity. The state is one JSON file rewritten whole on every
// transition, so a bounded fleet and bounded per-device history keep each
// commit bounded; Config.Validate refuses a configuration that grants more
// distinct devices than the state can hold, so the session and record caps
// are never reached by a validated deployment.
const (
	maxDevices    = 1024     // distinct enrolled devices: records and active sessions
	maxHistory    = 32       // transitions and request audits retained per device
	maxStateBytes = 16 << 20 // serialised state file
)

type Manager struct {
	// mu guards records, active, fault and verifiesHardware. It is held only
	// to read and to publish, never across the marshal, write and fsync of a
	// commit, so Pending and BeginSession on every push connection are not
	// stalled by storage latency.
	mu sync.Mutex
	// commitMu serialises writers (Report's transition path and request): each
	// one snapshots the state the previous one published, writes it durably,
	// then publishes under mu.
	commitMu sync.Mutex
	config   Config
	records  map[string]Record
	active   map[string]string
	lock     *os.File
	fault    error
	now      func() time.Time
	log      *slog.Logger
	// verifiesHardware is set when the push endpoint pins manufacturer keys, so
	// an unverified session is a finding rather than the only possible result.
	verifiesHardware bool
	// writeHook, when set, runs after a commit's snapshot is taken and before
	// its file is written — outside mu. Tests use it to hold a commit open.
	writeHook func()
	// auth records refused control credentials: the endpoint shares the read
	// listener, so a guessed token must leave a trace.
	auth controlauth.Limiter
}

// SetLogger directs the manager's own log lines (capacity refusals) to log.
// Call before the push endpoint serves; the default logger is used otherwise.
func (m *Manager) SetLogger(log *slog.Logger) {
	if m == nil || log == nil {
		return
	}
	m.mu.Lock()
	m.log = log
	m.mu.Unlock()
}

// SetHardwareVerification tells the manager whether the push endpoint verifies
// hardware evidence. Call before the push endpoint serves.
func (m *Manager) SetHardwareVerification(enabled bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.verifiesHardware = enabled
	m.mu.Unlock()
}

func (c Config) Validate() error {
	if c.StateFile == "" {
		if len(c.Principals) > 0 || c.Repository != "" || c.OpenRepository != "" {
			return errors.New("update controls require an explicit durable state_file")
		}
		return nil
	}
	if !filepath.IsAbs(c.StateFile) || (c.Repository == "" && c.OpenRepository == "") || len(c.Principals) == 0 || len(c.Principals) > 128 {
		return errors.New("update controls require absolute state/repository paths and bounded explicit principals")
	}
	for _, repository := range []string{c.Repository, c.OpenRepository} {
		if repository != "" && !filepath.IsAbs(repository) {
			return errors.New("update controls require absolute state/repository paths and bounded explicit principals")
		}
	}
	if c.Repository != "" && filepath.Clean(c.Repository) == filepath.Clean(c.OpenRepository) {
		return errors.New("the trusted and open tracks are separate repositories")
	}
	ids, tokens := map[string]bool{}, map[string]bool{}
	devices := map[string]bool{}
	for _, p := range c.Principals {
		token, err := hex.DecodeString(p.TokenSHA256)
		if !identity.ValidScopeID(p.ID) || ids[p.ID] || tokens[p.TokenSHA256] || err != nil || len(token) != 32 || len(p.Devices) == 0 || len(p.Devices) > 128 {
			return errors.New("invalid update principal")
		}
		ids[p.ID] = true
		tokens[p.TokenSHA256] = true
		seen := map[string]bool{}
		for _, d := range p.Devices {
			if !identity.ValidOpaqueObserverID(d.Observer) || len(d.Observer) > 256 || !identity.ValidScopeID(d.Enrollment) || !identity.ValidScopeID(d.Organization) || !identity.ValidScopeID(d.Collector) || seen[d.Observer] {
				return errors.New("invalid or ambiguous update device grant")
			}
			seen[d.Observer] = true
			devices[d.key()] = true
		}
	}
	// Several principals may be granted one device; the state holds one record
	// and one session per device, so the fleet is counted by distinct device.
	// Beyond the capacity a device would be enrolled but never get a session,
	// a record or a command — silently — so the configuration is refused.
	if len(devices) > maxDevices {
		return fmt.Errorf("update principals grant %d distinct devices; the update state holds at most %d", len(devices), maxDevices)
	}
	return nil
}
func Open(c Config) (*Manager, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.StateFile == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(c.StateFile), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(c.StateFile+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, err
	}
	m := &Manager{config: c, records: map[string]Record{}, active: map[string]string{}, lock: lock, now: time.Now, log: slog.Default()}
	records, err := loadRecords(c.StateFile)
	if err != nil {
		m.Close()
		return nil, err
	}
	m.records = records
	return m, nil
}

// Preflight checks an existing state file exactly as Open would load it, and
// nothing else: it does not create the state directory or the lock file and
// never takes the instance lock, which the running daemon legitimately holds
// while a configuration check runs beside it. The directory's writability is
// the configuration loader's check.
func (c Config) Preflight() error {
	if c.StateFile == "" {
		return nil
	}
	_, err := loadRecords(c.StateFile)
	return err
}

// loadRecords reads the state file, or returns an empty map when it does not
// exist yet. It is the one reader shared by Open and Preflight, so a file the
// daemon would refuse is refused by the configuration check too.
func loadRecords(path string) (map[string]Record, error) {
	records := map[string]Record{}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return records, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("update state must be a regular file")
	}
	if info.Size() > maxStateBytes || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("update state must be private and bounded")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	if len(records) > maxDevices {
		return nil, errors.New("update state device limit exceeded")
	}
	for key, r := range records {
		if key != r.Device.key() || len(r.Transitions) > maxHistory || len(r.Requests) > maxHistory || (r.Command.ID != 0 && !r.Command.Valid()) {
			return nil, errors.New("invalid persisted update record")
		}
	}
	return records, nil
}
func (m *Manager) Close() error {
	if m == nil || m.lock == nil {
		return nil
	}
	return m.lock.Close()
}

// snapshotLocked returns the state to serialise for a commit of record under
// key: a copy of every current record plus this one. Called with mu held by a
// writer that holds commitMu, so the copy is the state the previous commit
// published and nothing else can add a key before this one is published.
func (m *Manager) snapshotLocked(key string, record Record) (map[string]Record, error) {
	if m.fault != nil {
		return nil, m.fault
	}
	if _, ok := m.records[key]; !ok && len(m.records) >= maxDevices {
		capacityRefusals.WithLabelValues("records").Inc()
		m.log.Warn("update state capacity reached; device record not stored", "observer", record.Device.Observer, "capacity", maxDevices)
		return nil, errors.New("update state capacity reached")
	}
	candidate := make(map[string]Record, len(m.records)+1)
	for k, v := range m.records {
		candidate[k] = v
	}
	candidate[key] = record
	return candidate, nil
}

// commit writes candidate durably (private temporary file, fsync, rename,
// directory fsync) and then publishes record under key. It runs with commitMu
// held and mu released: the marshal, the write and the fsyncs are the slow
// part, and nothing that only reads the state needs to wait for them. A
// rename whose durability is uncertain faults the manager closed, as before.
func (m *Manager) commit(key string, record Record, candidate map[string]Record) error {
	data, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	if len(data) > maxStateBytes {
		capacityRefusals.WithLabelValues("bytes").Inc()
		m.log.Warn("update state byte limit exceeded; transition not stored", "observer", record.Device.Observer, "bytes", len(data), "limit", maxStateBytes)
		return errors.New("update state byte limit exceeded")
	}
	if m.writeHook != nil {
		m.writeHook()
	}
	f, err := os.CreateTemp(filepath.Dir(m.config.StateFile), ".updates-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, m.config.StateFile); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(m.config.StateFile))
	if err == nil {
		err = dir.Sync()
		dir.Close()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.fault = fmt.Errorf("update state commit is uncertain; restart after repairing storage: %w", err)
		return m.fault
	}
	// Publish in place rather than swapping in the snapshot: an unchanged
	// report of another device may have refreshed its record under mu while
	// this write was in flight, and that refresh must survive.
	m.records[key] = record
	return nil
}
func (m *Manager) authorized(token, observer string) (string, Device, bool) {
	if len(token) < 32 || len(token) > 512 {
		return "", Device{}, false
	}
	digest := sha256.Sum256([]byte(token))
	for _, p := range m.config.Principals {
		expected, _ := hex.DecodeString(p.TokenSHA256)
		if subtle.ConstantTimeCompare(digest[:], expected) != 1 {
			continue
		}
		for _, d := range p.Devices {
			if d.Observer == observer {
				return p.ID, d, true
			}
		}
	}
	return "", Device{}, false
}
func (m *Manager) enabled(d Device) bool {
	for _, p := range m.config.Principals {
		for _, allowed := range p.Devices {
			if allowed == d {
				return true
			}
		}
	}
	return false
}

// BeginSession pins reports to the most recently admitted authenticated session.
// An older connection cannot overwrite the new boot's status after reconnect.
func (m *Manager) BeginSession(context identity.ObserverContext, session string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := device(context)
	if !m.enabled(d) {
		return
	}
	if len(m.active) >= maxDevices && m.active[d.key()] == "" {
		// Unreachable for a validated configuration (Validate bounds the
		// distinct devices), but never silent: a device refused here reports
		// into nothing and can never be commanded.
		capacityRefusals.WithLabelValues("sessions").Inc()
		m.log.Warn("update session capacity reached; device reports will be ignored", "observer", d.Observer, "capacity", maxDevices)
		return
	}
	m.active[d.key()] = session
}
func (m *Manager) Pending(context identity.ObserverContext) []byte {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := device(context)
	r, ok := m.records[d.key()]
	if m.fault != nil || !ok || !m.enabled(d) || r.Command.Expires <= uint64(m.now().Unix()) || r.Command.ID == 0 ||
		(r.Status != nil && r.Status.LastCommand >= r.Command.ID) {
		return nil
	}
	bytes, _ := r.Command.Encode()
	return bytes
}

// Report is called only for current authenticated receipts, with collector
// receive time and per-session sequence ordering. Replayed samples cannot turn
// an old Requested state into a false Completed state.
func (m *Manager) Report(context identity.ObserverContext, session string, sequence uint64, status wire.UpdateStatus) error {
	if m == nil {
		return nil
	}
	// Writers are serialised by commitMu; mu covers only the read and the
	// publish (see Manager).
	m.commitMu.Lock()
	defer m.commitMu.Unlock()
	m.mu.Lock()
	d := device(context)
	if !m.enabled(d) || m.active[d.key()] != session || session == "" {
		m.mu.Unlock()
		return nil
	}
	r := m.records[d.key()]
	if r.Session == session && sequence <= r.Sequence {
		m.mu.Unlock()
		return nil
	}
	previous := r.Status
	trust := string(context.HardwareTrust)
	if trust == "" {
		trust = string(identity.HardwareTrustNone)
	}
	evidence := verified{trust: trust, oldTrust: r.HardwareTrust, verifies: m.verifiesHardware}
	r.Device = d
	r.Session = session
	r.Sequence = sequence
	r.Received = m.now()
	r.Status = &status
	r.HardwareTrust = trust
	// A reconnect whose evidence verifies differently is a transition even when
	// the device's own report is unchanged: it is the one change a device
	// cannot describe about itself. The raw error code, partition layout and
	// the two key ids are compared as the device sent them: two codes the
	// collector cannot name are still two different errors, and a changed boot
	// or release key id is exactly the evidence wanted when a rotation goes
	// wrong. Progress counters and check times churn and are left out.
	changed := previous == nil || previous.Mode != status.Mode || previous.Channel != status.Channel || previous.State != status.State ||
		previous.Running != status.Running || previous.Available != status.Available || previous.Staged != status.Staged || previous.Failed != status.Failed ||
		previous.Security != status.Security || previous.Profile != status.Profile || previous.Layout != status.Layout ||
		previous.ErrorDomain != status.ErrorDomain || previous.ErrorReason != status.ErrorReason || previous.Error != status.Error ||
		previous.LastCommand != status.LastCommand || previous.BootKey != status.BootKey || previous.ReleaseKey != status.ReleaseKey ||
		evidence.oldTrust != trust
	if !changed {
		m.records[d.key()] = r
		m.mu.Unlock()
		observe(d.Observer, previous, status, evidence, m.now())
		return nil
	}
	r.Transitions = append(append([]Transition(nil), r.Transitions...), Transition{At: m.now(), Status: status, HardwareTrust: trust})
	if len(r.Transitions) > maxHistory {
		r.Transitions = r.Transitions[len(r.Transitions)-maxHistory:]
	}
	candidate, err := m.snapshotLocked(d.key(), r)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if err := m.commit(d.key(), r, candidate); err != nil {
		return err
	}
	observe(d.Observer, previous, status, evidence, m.now())
	return nil
}
func (m *Manager) request(actor string, d Device, id string, command wire.UpdateCommand) (Record, error) {
	m.commitMu.Lock()
	defer m.commitMu.Unlock()
	m.mu.Lock()
	r := m.records[d.key()]
	for _, previous := range r.Requests {
		if previous.ID != id {
			continue
		}
		m.mu.Unlock()
		if previous.Actor != actor || previous.Command.Action != command.Action || previous.Command.Hint != command.Hint || previous.Command.Generation != command.Generation || previous.Command.Release != command.Release {
			return Record{}, errors.New("request_id already has different arguments")
		}
		r.Command = previous.Command
		r.RequestID = previous.ID
		r.Actor = previous.Actor
		r.Created = previous.Created
		return r, nil
	}
	if r.RequestID == id && r.Command.ID != 0 {
		m.mu.Unlock()
		if r.Actor != actor || r.Command.Action != command.Action || r.Command.Hint != command.Hint || r.Command.Generation != command.Generation || r.Command.Release != command.Release {
			return Record{}, errors.New("request_id already has different arguments")
		}
		return r, nil
	}
	if r.Status == nil {
		m.mu.Unlock()
		return Record{}, errors.New("wait for authenticated updater telemetry from this enrolled device")
	}
	if r.Command.ID != 0 && r.Status.LastCommand < r.Command.ID && r.Command.Expires > uint64(m.now().Unix()) && command.Action != 4 {
		m.mu.Unlock()
		return Record{}, errors.New("a request is still pending; inspect it or cancel")
	}
	floor := max(r.Command.ID, r.Status.LastCommand)
	if floor == math.MaxUint64 {
		m.mu.Unlock()
		return Record{}, errors.New("command number exhausted")
	}
	command.ID = max(floor+1, uint64(m.now().UnixNano()))
	command.Expires = uint64(m.now().Add(time.Hour).Unix())
	if !command.Valid() {
		m.mu.Unlock()
		return Record{}, errors.New("invalid update request")
	}
	r.Device = d
	r.Command = command
	r.RequestID = id
	r.Actor = actor
	r.Created = m.now()
	r.Requests = append(append([]RequestAudit(nil), r.Requests...), RequestAudit{id, actor, r.Created, command})
	if len(r.Requests) > maxHistory {
		r.Requests = r.Requests[len(r.Requests)-maxHistory:]
	}
	candidate, err := m.snapshotLocked(d.key(), r)
	m.mu.Unlock()
	if err != nil {
		return Record{}, err
	}
	if err := m.commit(d.key(), r, candidate); err != nil {
		return Record{}, err
	}
	return r, nil
}
