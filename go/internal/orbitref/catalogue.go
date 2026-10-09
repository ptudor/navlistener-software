package orbitref

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const Source = "BKG merged broadcast navigation"
const sourceRoot = "https://igs.bkg.bund.de/root_ftp/IGS/BRDC"
const maxDownload = 8 << 20
const maxExpanded = 64 << 20

type Satellite struct {
	Name     string      `json:"name"`
	GNSS     int         `json:"gnssid"`
	Epoch    time.Time   `json:"orbit_epoch"`
	Position *[3]float64 `json:"ecef_m,omitempty"`
	// Extrapolated marks a position propagated past its orbit's fit window
	// while downloads are delayed.
	Extrapolated bool `json:"extrapolated,omitempty"`
}

type Snapshot struct {
	Source     string      `json:"source"`
	Status     string      `json:"status"` // unavailable, current, delayed
	FetchedAt  *time.Time  `json:"fetched_at,omitempty"`
	Satellites []Satellite `json:"satellites"`
}

type diskCatalogue struct {
	Orbits    map[string]orbit `json:"orbits"`
	FetchedAt time.Time        `json:"fetched_at"`
}

// Catalogue retains the expected roster independently of receiver liveness.
// Losing an orbit drops its coordinates, not its identity. It can therefore
// never turn a missing satellite into apparent complete monitoring.
type Catalogue struct {
	mu        sync.RWMutex
	data      diskCatalogue
	client    *http.Client
	root      string
	cachePath string
	leaps     int
}

func New(cachePath string, leapSeconds int) *Catalogue {
	if leapSeconds == 0 {
		leapSeconds = 18
	}
	return &Catalogue{data: diskCatalogue{Orbits: map[string]orbit{}}, client: &http.Client{Timeout: 45 * time.Second}, root: sourceRoot, cachePath: cachePath, leaps: leapSeconds}
}

// Run performs bounded downloads on one collector goroutine, never
// on a browser request. Yesterday bootstraps the roster across UTC midnight.
func (c *Catalogue) Run(ctx context.Context, log *slog.Logger) {
	if err := c.load(); err != nil {
		log.Warn("map orbit cache unavailable", "error", err)
	}
	lastDay := ""
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		now := time.Now().UTC()
		day := now.Format("2006-01-02")
		if day != lastDay {
			if err := c.fetch(ctx, now.AddDate(0, 0, -1), now); err != nil {
				log.Warn("map previous-day orbit reference unavailable", "error", err)
			}
			lastDay = day
		}
		if err := c.fetch(ctx, now, now); err != nil {
			log.Warn("map orbit reference unavailable", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Catalogue) fetch(ctx context.Context, day, now time.Time) error {
	y, d := day.UTC().Year(), day.UTC().YearDay()
	u := fmt.Sprintf("%s/%d/%03d/BRDC00WRD_R_%d%03d0000_01D_MN.rnx.gz", c.root, y, d, y, d)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "NavListen-orbit-map/1.0")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("orbit reference HTTP %d", resp.StatusCode)
	}
	compressed := &io.LimitedReader{R: resp.Body, N: maxDownload + 1}
	zr, err := gzip.NewReader(compressed)
	if err != nil {
		return err
	}
	defer zr.Close()
	expanded := &io.LimitedReader{R: zr, N: maxExpanded + 1}
	orbits, err := parseRINEX(expanded, c.leaps)
	if err != nil {
		return err
	}
	if compressed.N <= 0 || expanded.N <= 0 {
		return fmt.Errorf("orbit reference exceeds size limit")
	}
	for _, o := range orbits {
		if o.Epoch.After(now.Add(2*time.Hour)) || o.Epoch.Before(day.UTC().Truncate(24*time.Hour).Add(-24*time.Hour)) {
			return fmt.Errorf("orbit reference epoch outside requested day")
		}
	}
	c.mu.Lock()
	for name, o := range orbits {
		if prev, ok := c.data.Orbits[name]; !ok || o.Epoch.After(prev.Epoch) {
			c.data.Orbits[name] = o
		}
	}
	c.data.FetchedAt = now
	c.mu.Unlock()
	return c.save()
}

func (c *Catalogue) Snapshot(now time.Time) Snapshot {
	c.mu.RLock()
	data := c.data
	orbits := make([]orbit, 0, len(data.Orbits))
	for _, o := range data.Orbits {
		orbits = append(orbits, o)
	}
	c.mu.RUnlock()
	s := Snapshot{Source: Source, Status: "unavailable", Satellites: []Satellite{}}
	if !data.FetchedAt.IsZero() {
		at := data.FetchedAt
		s.FetchedAt = &at
		s.Status = "delayed"
		if age := now.Sub(at); age >= 0 && age <= 35*time.Minute {
			s.Status = "current"
		}
	}
	// A delayed reference keeps placing satellites from their last orbits so an
	// upstream outage does not erase known gaps from the map. A current
	// reference never extrapolates: an orbit it has stopped refreshing belongs
	// to a satellite whose geometry is genuinely unknown.
	coast := s.Status == "delayed"
	sort.Slice(orbits, func(i, j int) bool { return orbits[i].Name < orbits[j].Name })
	for _, o := range orbits {
		sv := Satellite{Name: o.Name, GNSS: int(o.GNSS), Epoch: o.Epoch}
		p, ok := o.position(now)
		if !ok && coast {
			p, ok = o.extrapolate(now)
			sv.Extrapolated = ok
		}
		if ok {
			sv.Position = &[3]float64{p.X, p.Y, p.Z}
		}
		s.Satellites = append(s.Satellites, sv)
	}
	return s
}

func (c *Catalogue) load() error {
	if c.cachePath == "" {
		return nil
	}
	f, err := os.Open(c.cachePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	var data diskCatalogue
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&data); err != nil {
		return err
	}
	if len(data.Orbits) > 180 || data.FetchedAt.After(time.Now().Add(time.Minute)) {
		return fmt.Errorf("invalid orbit cache")
	}
	for name, o := range data.Orbits {
		g, known := systems[nameByte(name)]
		if !known || name != o.Name || len(name) != 3 || o.GNSS != g {
			return fmt.Errorf("invalid cached satellite")
		}
		if _, ok := o.position(o.Epoch); !ok {
			return fmt.Errorf("invalid cached orbit")
		}
	}
	if data.Orbits == nil {
		data.Orbits = map[string]orbit{}
	}
	c.mu.Lock()
	c.data = data
	c.mu.Unlock()
	return nil
}

func nameByte(name string) byte {
	if len(name) == 0 {
		return 0
	}
	return name[0]
}

func (c *Catalogue) save() error {
	if c.cachePath == "" {
		return nil
	}
	c.mu.RLock()
	b, err := json.Marshal(c.data)
	c.mu.RUnlock()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.cachePath), ".orbit-cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), c.cachePath)
}
