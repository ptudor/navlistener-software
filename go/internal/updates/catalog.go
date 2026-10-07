package updates

// The catalog provides routing hints for the operator UI. It is never firmware
// authorization: the device independently authenticates the complete TUF chain.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Every role a published tree serves. Root, targets and releases are offline
// roles renewed in a planned signing ceremony, so the collector warns 90 days
// ahead as the design promises; the others are renewed by the routine online
// refresh, so a week of margin is enough to notice a missed one.
var (
	catalogRoles = []string{"root", "targets", "releases", "stable", "canary", "lab", "snapshot", "timestamp"}
	offlineRoles = map[string]bool{"root": true, "targets": true, "releases": true}
)

const (
	offlineExpiryWarning = 90 * 24 * time.Hour
	onlineExpiryWarning  = 7 * 24 * time.Hour
	lifetimeCheckEvery   = 6 * time.Hour
	// A device walks at most 32 root rotations per refresh but keeps its place,
	// so a longer chain is still followed; this bound only stops a runaway scan.
	rootChainLimit = 256
)

type Choice struct {
	Generation uint64 `json:"generation,string"`
	Release    uint64 `json:"release,string"`
	Percentage int    `json:"percentage"`
	Advisory   string `json:"advisory"`
}
type reference struct {
	Version uint64            `json:"version"`
	Length  int64             `json:"length"`
	Hashes  map[string]string `json:"hashes"`
}
type catalogMetadata struct {
	Signed struct {
		Version uint64               `json:"version"`
		Expires time.Time            `json:"expires"`
		Meta    map[string]reference `json:"meta"`
		Targets map[string]reference `json:"targets"`
	} `json:"signed"`
}

// repository selects the published tree for the track a device reports. A
// device or persisted record that predates the report is on the trusted track;
// a test build, or a track this collector does not serve, has no catalog.
func (m *Manager) repository(profile string) (string, error) {
	root := ""
	switch profile {
	case "trusted", "unreported", "":
		root = m.config.Repository
	case "open":
		root = m.config.OpenRepository
	}
	if root == "" {
		return "", errors.New("no catalog for this release track")
	}
	return filepath.Clean(root), nil
}
func (m *Manager) object(root, path string, limit int64, ref *reference) ([]byte, error) {
	if !filepath.IsLocal(path) || strings.Contains(path, "..") {
		return nil, errors.New("invalid catalog path")
	}
	full := filepath.Join(root, path)
	for p := full; p != root; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("catalog symlink refused")
		}
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || int64(len(data)) > limit {
		return nil, errors.New("catalog object exceeds limit")
	}
	if ref != nil {
		sum := sha256.Sum256(data)
		if ref.Length != int64(len(data)) || len(ref.Hashes) != 1 || ref.Hashes["sha256"] != hex.EncodeToString(sum[:]) {
			return nil, errors.New("catalog hash differs")
		}
	}
	return data, nil
}

// readMetadata parses one metadata object without judging its freshness; a
// reference pins its length, hash and version.
func (m *Manager) readMetadata(root, path string, limit int64, ref *reference) (catalogMetadata, error) {
	var md catalogMetadata
	data, err := m.object(root, path, limit, ref)
	if err != nil {
		return md, err
	}
	if err = json.Unmarshal(data, &md); err != nil {
		return md, err
	}
	if md.Signed.Version == 0 || (ref != nil && ref.Version != md.Signed.Version) {
		return md, errors.New("catalog metadata version differs")
	}
	return md, nil
}
func (m *Manager) metadata(root, name string, limit int64, ref *reference) (catalogMetadata, error) {
	path := "metadata/" + name + ".json"
	if ref != nil {
		path = fmt.Sprintf("metadata/%d.%s.json", ref.Version, name)
	}
	md, err := m.readMetadata(root, path, limit, ref)
	if err == nil && !md.Signed.Expires.After(m.now()) {
		return md, errors.New("catalog metadata expired")
	}
	return md, err
}

// lifetimes reports when each role the tree at root serves expires: timestamp,
// the snapshot it names, the five roles that snapshot names, and the latest
// root, which is where a device's rotation walk ends. Expired metadata is
// reported rather than refused; the point is to say so.
func (m *Manager) lifetimes(root string) (map[string]time.Time, error) {
	expiry := map[string]time.Time{}
	timestamp, err := m.readMetadata(root, "metadata/timestamp.json", 4096, nil)
	if err != nil {
		return nil, fmt.Errorf("timestamp: %w", err)
	}
	expiry["timestamp"] = timestamp.Signed.Expires
	ref := timestamp.Signed.Meta["snapshot.json"]
	snapshot, err := m.readMetadata(root, fmt.Sprintf("metadata/%d.snapshot.json", ref.Version), 8192, &ref)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	expiry["snapshot"] = snapshot.Signed.Expires
	for _, role := range []string{"targets", "releases", "stable", "canary", "lab"} {
		ref = snapshot.Signed.Meta[role+".json"]
		md, err := m.readMetadata(root, fmt.Sprintf("metadata/%d.%s.json", ref.Version, role), 12288, &ref)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", role, err)
		}
		expiry[role] = md.Signed.Expires
	}
	for version := uint64(1); ; version++ {
		if version > rootChainLimit {
			return nil, errors.New("root rotation chain exceeds the catalog limit")
		}
		md, err := m.readMetadata(root, fmt.Sprintf("metadata/%d.root.json", version), 8192, nil)
		if errors.Is(err, os.ErrNotExist) && version > 1 {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("root %d: %w", version, err)
		}
		if md.Signed.Version != version {
			return nil, fmt.Errorf("root %d: version differs from its name", version)
		}
		expiry["root"] = md.Signed.Expires
	}
	return expiry, nil
}

// expiryState classifies one role's remaining lifetime: "expired", "expiring"
// inside the role's warning window, or "" while nothing is due.
func expiryState(role string, expires, now time.Time) string {
	warning := onlineExpiryWarning
	if offlineRoles[role] {
		warning = offlineExpiryWarning
	}
	switch remaining := expires.Sub(now); {
	case remaining <= 0:
		return "expired"
	case remaining < warning:
		return "expiring"
	}
	return ""
}

// observeLifetimes records when every served role of each track expires and
// logs the roles that are expired or due for renewal. An unreadable catalog
// zeroes that track's gauge so a stale value cannot stand in for it.
func (m *Manager) observeLifetimes(log *slog.Logger) {
	now := m.now()
	for _, track := range []struct{ name, root string }{{"trusted", m.config.Repository}, {"open", m.config.OpenRepository}} {
		if track.root == "" {
			continue
		}
		expiry, err := m.lifetimes(filepath.Clean(track.root))
		if err != nil {
			log.Warn("update catalog lifetimes unreadable", "track", track.name, "error", err)
			for _, role := range catalogRoles {
				metadataExpiry.WithLabelValues(track.name, role).Set(0)
			}
			continue
		}
		for _, role := range catalogRoles {
			expires := expiry[role]
			metadataExpiry.WithLabelValues(track.name, role).Set(float64(expires.Unix()))
			switch expiryState(role, expires, now) {
			case "expired":
				log.Error("update metadata expired; devices refuse updates until it is renewed", "track", track.name, "role", role, "expired", expires.UTC())
			case "expiring":
				log.Warn("update metadata expires soon; plan its renewal", "track", track.name, "role", role, "expires", expires.UTC(), "days", int(expires.Sub(now).Hours()/24))
			}
		}
	}
}

// WatchLifetimes runs the expiry check at startup and every few hours until
// done closes. It is the collector-side alert ahead of each offline renewal.
func (m *Manager) WatchLifetimes(done <-chan struct{}, log *slog.Logger) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(lifetimeCheckEvery)
	defer ticker.Stop()
	for {
		m.observeLifetimes(log)
		select {
		case <-done:
			return
		case <-ticker.C:
		}
	}
}
func (m *Manager) choice(profile, channel string) (*Choice, error) {
	if channel != "stable" && channel != "canary" && channel != "lab" {
		return nil, errors.New("unknown channel")
	}
	root, err := m.repository(profile)
	if err != nil {
		return nil, err
	}
	timestamp, err := m.metadata(root, "timestamp", 4096, nil)
	if err != nil {
		return nil, err
	}
	ref := timestamp.Signed.Meta["snapshot.json"]
	snapshot, err := m.metadata(root, "snapshot", 8192, &ref)
	if err != nil {
		return nil, err
	}
	ref = snapshot.Signed.Meta[channel+".json"]
	md, err := m.metadata(root, channel, 12288, &ref)
	if err != nil {
		return nil, err
	}
	ref = md.Signed.Targets["channels/"+channel+".json"]
	if len(ref.Hashes["sha256"]) != 64 {
		return nil, errors.New("missing channel target")
	}
	data, err := m.object(root, "targets/channels/"+ref.Hashes["sha256"]+"."+channel+".json", 4096, &ref)
	if err != nil {
		return nil, err
	}
	var value struct {
		Generation uint64   `json:"generation"`
		Release    uint64   `json:"release_sequence"`
		Percentage int      `json:"percentage"`
		Withdrawn  []uint64 `json:"withdrawn"`
		Advisory   struct {
			Summary string `json:"summary"`
		} `json:"advisory"`
	}
	if err = json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	if value.Generation == 0 || value.Release == 0 || value.Percentage <= 0 || value.Percentage > 100 {
		return nil, errors.New("no selected release")
	}
	for _, release := range value.Withdrawn {
		if release == value.Release {
			return nil, errors.New("selected release withdrawn")
		}
	}
	return &Choice{value.Generation, value.Release, value.Percentage, value.Advisory.Summary}, nil
}
