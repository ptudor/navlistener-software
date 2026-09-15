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
	"os"
	"path/filepath"
	"strings"
	"time"
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

func (m *Manager) object(path string, limit int64, ref *reference) ([]byte, error) {
	if !filepath.IsLocal(path) || strings.Contains(path, "..") {
		return nil, errors.New("invalid catalog path")
	}
	root := filepath.Clean(m.config.Repository)
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
func (m *Manager) metadata(name string, limit int64, ref *reference) (catalogMetadata, error) {
	var md catalogMetadata
	path := "metadata/" + name + ".json"
	if ref != nil {
		path = fmt.Sprintf("metadata/%d.%s.json", ref.Version, name)
	}
	data, err := m.object(path, limit, ref)
	if err != nil {
		return md, err
	}
	if err = json.Unmarshal(data, &md); err != nil {
		return md, err
	}
	if md.Signed.Version == 0 || !md.Signed.Expires.After(m.now()) || (ref != nil && ref.Version != md.Signed.Version) {
		return md, errors.New("catalog metadata expired or version differs")
	}
	return md, nil
}
func (m *Manager) choice(channel string) (*Choice, error) {
	if channel != "stable" && channel != "canary" && channel != "lab" {
		return nil, errors.New("unknown channel")
	}
	timestamp, err := m.metadata("timestamp", 4096, nil)
	if err != nil {
		return nil, err
	}
	ref := timestamp.Signed.Meta["snapshot.json"]
	snapshot, err := m.metadata("snapshot", 8192, &ref)
	if err != nil {
		return nil, err
	}
	ref = snapshot.Signed.Meta[channel+".json"]
	md, err := m.metadata(channel, 12288, &ref)
	if err != nil {
		return nil, err
	}
	ref = md.Signed.Targets["channels/"+channel+".json"]
	if len(ref.Hashes["sha256"]) != 64 {
		return nil, errors.New("missing channel target")
	}
	data, err := m.object("targets/channels/"+ref.Hashes["sha256"]+"."+channel+".json", 4096, &ref)
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
