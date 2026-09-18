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
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

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
}
type RequestAudit struct {
	ID      string             `json:"request_id"`
	Actor   string             `json:"actor"`
	Created time.Time          `json:"created"`
	Command wire.UpdateCommand `json:"command"`
}
type Record struct {
	Device      Device             `json:"device"`
	Command     wire.UpdateCommand `json:"command"`
	RequestID   string             `json:"request_id"`
	Actor       string             `json:"actor"`
	Created     time.Time          `json:"created"`
	Status      *wire.UpdateStatus `json:"status,omitempty"`
	Received    time.Time          `json:"received"`
	Session     string             `json:"session"`
	Sequence    uint64             `json:"sequence,string"`
	Transitions []Transition       `json:"transitions"`
	Requests    []RequestAudit     `json:"requests"`
}
type Manager struct {
	mu      sync.Mutex
	config  Config
	records map[string]Record
	active  map[string]string
	lock    *os.File
	fault   error
	now     func() time.Time
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
		}
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
	m := &Manager{config: c, records: map[string]Record{}, active: map[string]string{}, lock: lock, now: time.Now}
	if info, err := os.Stat(c.StateFile); err == nil {
		if info.Size() > 16<<20 || info.Mode().Perm()&0077 != 0 {
			m.Close()
			return nil, errors.New("update state must be private and bounded")
		}
		data, err := os.ReadFile(c.StateFile)
		if err != nil {
			m.Close()
			return nil, err
		}
		if err = json.Unmarshal(data, &m.records); err != nil {
			m.Close()
			return nil, err
		}
		if len(m.records) > 1024 {
			m.Close()
			return nil, errors.New("update state device limit exceeded")
		}
		for key, r := range m.records {
			if key != r.Device.key() || len(r.Transitions) > 32 || len(r.Requests) > 32 || (r.Command.ID != 0 && !r.Command.Valid()) {
				m.Close()
				return nil, errors.New("invalid persisted update record")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		m.Close()
		return nil, err
	}
	return m, nil
}
func (m *Manager) Close() error {
	if m == nil || m.lock == nil {
		return nil
	}
	return m.lock.Close()
}
func (m *Manager) commit(key string, record Record) error {
	if m.fault != nil {
		return m.fault
	}
	if _, ok := m.records[key]; !ok && len(m.records) >= 1024 {
		return errors.New("update state capacity reached")
	}
	candidate := make(map[string]Record, len(m.records)+1)
	for k, v := range m.records {
		candidate[k] = v
	}
	candidate[key] = record
	data, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("update state byte limit exceeded")
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
	if err != nil {
		m.fault = fmt.Errorf("update state commit is uncertain; restart after repairing storage: %w", err)
		return m.fault
	}
	m.records = candidate
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
	if m.enabled(d) && (len(m.active) < 1024 || m.active[d.key()] != "") {
		m.active[d.key()] = session
	}
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
	m.mu.Lock()
	defer m.mu.Unlock()
	d := device(context)
	if !m.enabled(d) || m.active[d.key()] != session || session == "" {
		return nil
	}
	r := m.records[d.key()]
	if r.Session == session && sequence <= r.Sequence {
		return nil
	}
	previous := r.Status
	r.Device = d
	r.Session = session
	r.Sequence = sequence
	r.Received = m.now()
	r.Status = &status
	changed := previous == nil || previous.Mode != status.Mode || previous.Channel != status.Channel || previous.State != status.State ||
		previous.Running != status.Running || previous.Available != status.Available || previous.Staged != status.Staged || previous.Failed != status.Failed ||
		previous.Security != status.Security || previous.Profile != status.Profile || previous.Error != status.Error || previous.LastCommand != status.LastCommand
	if !changed {
		m.records[d.key()] = r
		observe(d.Observer, previous, status, m.now())
		return nil
	}
	r.Transitions = append(append([]Transition(nil), r.Transitions...), Transition{m.now(), status})
	if len(r.Transitions) > 32 {
		r.Transitions = r.Transitions[len(r.Transitions)-32:]
	}
	if err := m.commit(d.key(), r); err != nil {
		return err
	}
	observe(d.Observer, previous, status, m.now())
	return nil
}
func (m *Manager) request(actor string, d Device, id string, command wire.UpdateCommand) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.records[d.key()]
	for _, previous := range r.Requests {
		if previous.ID != id {
			continue
		}
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
		if r.Actor != actor || r.Command.Action != command.Action || r.Command.Hint != command.Hint || r.Command.Generation != command.Generation || r.Command.Release != command.Release {
			return Record{}, errors.New("request_id already has different arguments")
		}
		return r, nil
	}
	if r.Status == nil {
		return Record{}, errors.New("wait for authenticated updater telemetry from this enrolled device")
	}
	if r.Command.ID != 0 && r.Status.LastCommand < r.Command.ID && r.Command.Expires > uint64(m.now().Unix()) && command.Action != 4 {
		return Record{}, errors.New("a request is still pending; inspect it or cancel")
	}
	floor := max(r.Command.ID, r.Status.LastCommand)
	if floor == math.MaxUint64 {
		return Record{}, errors.New("command number exhausted")
	}
	command.ID = max(floor+1, uint64(m.now().UnixNano()))
	command.Expires = uint64(m.now().Add(time.Hour).Unix())
	if !command.Valid() {
		return Record{}, errors.New("invalid update request")
	}
	r.Device = d
	r.Command = command
	r.RequestID = id
	r.Actor = actor
	r.Created = m.now()
	r.Requests = append(append([]RequestAudit(nil), r.Requests...), RequestAudit{id, actor, r.Created, command})
	if len(r.Requests) > 32 {
		r.Requests = r.Requests[len(r.Requests)-32:]
	}
	if err := m.commit(d.key(), r); err != nil {
		return Record{}, err
	}
	return r, nil
}
