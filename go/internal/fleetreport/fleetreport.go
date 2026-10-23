// Package fleetreport builds and sends a collector's field report: what it saw
// of the boards enrolled under one manufacturer authority (docs/FLEET-REPORTS.md).
//
// Nothing is sent unless the operator sets [fleet_report] url. A board never
// reports to its manufacturer itself; the collector it already reports to
// does, and only with the operator's configuration. The report carries board
// identities, the collector's hardware-trust verdict and commissioning
// fingerprint, the board's own firmware and update status, and the hour each
// was last seen: no position, observation, network or enrollment detail.
package fleetreport

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ptudor/navlistener/internal/boardid"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/wire"
)

const (
	Format          = "navlistener-field-report-v1"
	MaxBoards       = 10000
	DefaultInterval = 24 * time.Hour
	MinInterval     = time.Hour
	maxTokenBytes   = 4096
)

var (
	fingerprintRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tokenRe       = regexp.MustCompile(`^[\x21-\x7e]{16,256}$`)
	errorNameRe   = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)
)

// Config is [fleet_report]. Reporting is on exactly when URL is set.
type Config struct {
	// URL receives the report as an HTTPS POST. Redirects are not followed.
	URL string `toml:"url"`
	// TokenFile holds the bearer token the manufacturer issued, on one line,
	// mode 0600. It is read again for every attempt, so it can be replaced
	// without a restart.
	TokenFile string `toml:"token_file"`
	// ManufacturerAuthorityID names the authority whose boards are reported;
	// it must be a configured [[manufacturer_authority]].
	ManufacturerAuthorityID string        `toml:"manufacturer_authority_id"`
	Intervals               string        `toml:"interval"` // default "24h", at least "1h"
	Interval                time.Duration `toml:"-"`
}

// Enabled reports whether the operator turned reporting on.
func (c Config) Enabled() bool { return c.URL != "" }

// Validate checks a [fleet_report] table against the configured manufacturer
// authorities and reads the token once, so a misconfiguration stops startup
// and -check-config rather than the first send a day later.
func (c *Config) Validate(authorities []string) error {
	if !c.Enabled() {
		if c.TokenFile != "" || c.ManufacturerAuthorityID != "" || c.Intervals != "" {
			return errors.New("fleet_report: url is required with token_file, manufacturer_authority_id or interval")
		}
		return nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("fleet_report.url %q: want an https:// URL with a host and no credentials", c.URL)
	}
	if !slices.Contains(authorities, c.ManufacturerAuthorityID) {
		return fmt.Errorf("fleet_report.manufacturer_authority_id %q is not a configured [[manufacturer_authority]]", c.ManufacturerAuthorityID)
	}
	c.Interval = DefaultInterval
	if c.Intervals != "" {
		d, err := time.ParseDuration(c.Intervals)
		if err != nil {
			return fmt.Errorf("fleet_report.interval: %w", err)
		}
		c.Interval = d
	}
	if c.Interval < MinInterval {
		return fmt.Errorf("fleet_report.interval %s: at least %s", c.Interval, MinInterval)
	}
	if c.TokenFile == "" {
		return errors.New("fleet_report.token_file is required")
	}
	if _, err := ReadToken(c.TokenFile); err != nil {
		return fmt.Errorf("fleet_report.token_file: %w", err)
	}
	return nil
}

// ReadToken reads the bearer token: one line of 16-256 printable characters
// in a regular file no other account can read.
func ReadToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if m := info.Mode().Perm(); m&0o077 != 0 {
		return "", fmt.Errorf("%s is group/world-accessible (mode %04o); a token file must be 0600", path, m)
	}
	if info.Size() > maxTokenBytes {
		return "", fmt.Errorf("%s is larger than a token", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if !tokenRe.MatchString(token) {
		return "", fmt.Errorf("%s must hold one line of 16-256 printable characters", path)
	}
	return token, nil
}

// Board is what the collector holds of one board for a report. HardwareTrust
// and CommissioningFingerprint are the collector's verification of the board's
// latest session; Firmware and Update are the board's own account.
type Board struct {
	ObserverID               string
	HardwareTrust            identity.HardwareTrust
	CommissioningFingerprint string
	LastReceived             time.Time
	Firmware                 string
	Update                   *wire.UpdateStatus
}

// Source supplies the boards enrolled under one manufacturer authority.
type Source interface {
	FleetBoards(manufacturerAuthorityID string) []Board
}

type report struct {
	Format                  string  `json:"format"`
	ReportID                string  `json:"report_id"`
	ManufacturerAuthorityID string  `json:"manufacturer_authority_id"`
	CollectorInstanceID     *string `json:"collector_instance_id"`
	GeneratedAt             string  `json:"generated_at"`
	Boards                  []board `json:"boards"`
}

type board struct {
	ObserverID               string  `json:"observer_id"`
	HardwareTrust            string  `json:"hardware_trust"`
	CommissioningFingerprint *string `json:"commissioning_fingerprint"`
	LastSeen                 string  `json:"last_seen"`
	Firmware                 *string `json:"firmware"`
	Update                   *update `json:"update"`
}

type update struct {
	Channel        string `json:"channel"`
	Mode           string `json:"mode"`
	State          string `json:"state"`
	TrustProfile   string `json:"trust_profile"`
	RunningRelease string `json:"running_release"`
	FailedRelease  string `json:"failed_release"`
	Error          string `json:"error"`
}

// Built is one report, ready to send and to resend unchanged.
type Built struct {
	ID      string
	Body    []byte
	Boards  int
	Omitted int // boards the report could not carry faithfully
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// boardIdentity parses an observer id as a board identity, accepting only its
// canonical spelling.
func boardIdentity(observerID string) (boardid.ID, bool) {
	kind, value, ok := strings.Cut(strings.TrimPrefix(observerID, boardid.ObserverPrefix), "-")
	if !strings.HasPrefix(observerID, boardid.ObserverPrefix) || !ok || len(kind) != 4 {
		return boardid.ID{}, false
	}
	code, err := strconv.ParseUint(kind, 16, 16)
	raw, herr := hex.DecodeString(value)
	if err != nil || herr != nil {
		return boardid.ID{}, false
	}
	id, err := boardid.FromBytes(uint16(code), raw)
	if err != nil || id.ObserverID() != observerID {
		return boardid.ID{}, false
	}
	return id, true
}

func printable(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func convert(b Board) (board, bool) {
	if _, ok := boardIdentity(b.ObserverID); !ok {
		return board{}, false
	}
	out := board{ObserverID: b.ObserverID, HardwareTrust: string(b.HardwareTrust), LastSeen: stamp(b.LastReceived.Truncate(time.Hour))}
	switch b.HardwareTrust {
	case identity.HardwareTrustNone:
		if b.CommissioningFingerprint != "" {
			return board{}, false
		}
	case identity.HardwareTrustOpen, identity.HardwareTrustTest, identity.HardwareTrustTrusted:
		if !fingerprintRe.MatchString(b.CommissioningFingerprint) {
			return board{}, false
		}
		fingerprint := b.CommissioningFingerprint
		out.CommissioningFingerprint = &fingerprint
	default:
		return board{}, false
	}
	if printable(b.Firmware, 32) {
		firmware := b.Firmware
		out.Firmware = &firmware
	}
	if u := b.Update; u != nil {
		if !errorNameRe.MatchString(u.Error) {
			return board{}, false
		}
		out.Update = &update{
			Channel: u.Channel, Mode: u.Mode, State: u.State, TrustProfile: u.Profile,
			RunningRelease: strconv.FormatUint(u.Running, 10), FailedRelease: strconv.FormatUint(u.Failed, 10),
			Error: u.Error,
		}
	}
	return out, true
}

// NewReportID returns a random (version 4) UUID. One report keeps its ID
// through every retry; the next report gets a new one.
func NewReportID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// Build encodes one report. Boards that are not board identities, or whose
// evidence or status cannot be represented exactly, are left out and counted
// rather than reported wrongly.
func Build(id, authority, instance string, now time.Time, boards []Board) (Built, error) {
	r := report{Format: Format, ReportID: id, ManufacturerAuthorityID: authority, GeneratedAt: stamp(now), Boards: []board{}}
	if instance != "" {
		r.CollectorInstanceID = &instance
	}
	omitted := 0
	for _, b := range boards {
		converted, ok := convert(b)
		if !ok {
			omitted++
			continue
		}
		r.Boards = append(r.Boards, converted)
	}
	slices.SortFunc(r.Boards, func(a, b board) int { return strings.Compare(a.ObserverID, b.ObserverID) })
	if len(r.Boards) > MaxBoards {
		omitted += len(r.Boards) - MaxBoards
		r.Boards = r.Boards[:MaxBoards]
	}
	body, err := json.Marshal(r)
	if err != nil {
		return Built{}, err
	}
	return Built{ID: id, Body: body, Boards: len(r.Boards), Omitted: omitted}, nil
}

// jitter spreads collectors' first reports so a fleet restarted together
// does not report together: up to a tenth of base, from crypto/rand.
func jitter(base time.Duration) time.Duration {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil || base <= 0 {
		return 0
	}
	return time.Duration(binary.BigEndian.Uint64(b[:]) % uint64(base/10+1))
}
