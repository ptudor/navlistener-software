package integrity

import (
	"fmt"
	"math"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/geo"
	"github.com/ptudor/gnss/physconst"
)

type SurveyPolicy struct {
	Duration       time.Duration `toml:"duration" json:"duration_ns"`
	MinSamples     int           `toml:"min_samples" json:"min_samples"`
	MaxDispersionM float64       `toml:"max_dispersion_m" json:"max_dispersion_m"`
	MaxAccuracyM   float64       `toml:"max_accuracy_m" json:"max_accuracy_m"`
	MinSV          int           `toml:"min_sv" json:"min_sv"`
}

func (p SurveyPolicy) Validate() error {
	if p.Duration < time.Second || p.Duration > 24*time.Hour || p.MinSamples < 3 || p.MinSamples > 1000000 || p.MinSV < 4 || p.MinSV > 255 {
		return fmt.Errorf("integrity: survey requires duration 1s..24h, 3..1000000 samples and min_sv 4..255")
	}
	if err := positive("survey max_dispersion_m", p.MaxDispersionM); err != nil {
		return err
	}
	return positive("survey max_accuracy_m", p.MaxAccuracyM)
}

type SurveyCandidate struct {
	Position             Surveyed      `toml:"position" json:"position"`
	Source               string        `toml:"source" json:"source"`
	Independent          bool          `toml:"independent" json:"independent"`
	Duration             time.Duration `toml:"duration" json:"duration_ns"`
	Samples              int           `toml:"samples" json:"samples"`
	DispersionM          float64       `toml:"dispersion_m" json:"dispersion_m"`
	MaxReportedAccuracyM float64       `toml:"max_reported_accuracy_m" json:"max_reported_accuracy_m"`
	MinSV                int           `toml:"min_sv" json:"min_sv"`
	FixType              int           `toml:"fix_type" json:"fix_type"`
	FirstUTC             time.Time     `toml:"first_utc" json:"first_utc"`
	LastUTC              time.Time     `toml:"last_utc" json:"last_utc"`
}

func (c SurveyCandidate) Validate() error {
	if c.Source != "receiver_survey" || c.Independent || c.Duration < time.Second || c.Duration > 24*time.Hour || c.Samples < 3 || c.MinSV < 4 || c.FixType != Fix3D || !finite(c.DispersionM) || c.DispersionM < 0 || !finite(c.MaxReportedAccuracyM) || c.MaxReportedAccuracyM <= 0 || c.FirstUTC.IsZero() || c.LastUTC.Before(c.FirstUTC) {
		return fmt.Errorf("integrity: invalid receiver survey candidate evidence")
	}
	return (StationProfile{Mode: ModeFixed, Position: &c.Position}).Validate()
}

// Survey builds one bounded candidate then freezes it. A gap, poor quality or
// invalid UTC starts the collection again. It never changes a pinned reference.
type Survey struct {
	policy          SurveyPolicy
	first, last     Solution
	n               int
	mean            gnss.ECEF
	m2, maxAccuracy float64
	minSV           int
	ready           *SurveyCandidate
	elapsed         time.Duration
}

func NewSurvey(p SurveyPolicy) (*Survey, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &Survey{policy: p}, nil
}

func (a *Survey) Apply(s Solution) {
	if a.ready != nil {
		return
	}
	p := a.policy
	if !s.Usable3D() || s.FixType != Fix3D || !s.UTCValid || s.UTC.IsZero() || s.NumSV < p.MinSV || !finite(s.HAccM) || !finite(s.VAccM) || s.HAccM <= 0 || s.VAccM <= 0 || math.Max(s.HAccM, s.VAccM) > p.MaxAccuracyM || !validCoordinates(s) {
		*a = Survey{policy: p}
		return
	}
	if a.n > 0 {
		d := towDelta(a.last.TOW, s.TOW)
		if d == 0 || d < 0 && -d <= maxEpochGapMS {
			return
		}
		utcStep := s.UTC.Sub(a.last.UTC)
		if d < 0 || d > maxEpochGapMS || (utcStep-time.Duration(d)*time.Millisecond).Abs() > 250*time.Millisecond || s.UTC.Sub(a.first.UTC) > 24*time.Hour {
			*a = Survey{policy: p}
		} else {
			a.elapsed += time.Duration(d) * time.Millisecond
		}
	}
	if a.n == 0 {
		a.first = s
		a.minSV = s.NumSV
	}
	a.n++
	a.last = s
	delta := s.ecef().Sub(a.mean)
	a.mean = a.mean.Add(delta.Scale(1 / float64(a.n)))
	a.m2 += delta.Dot(s.ecef().Sub(a.mean))
	a.maxAccuracy = math.Max(a.maxAccuracy, math.Max(s.HAccM, s.VAccM))
	a.minSV = min(a.minSV, s.NumSV)
	disp := math.Sqrt(math.Max(0, a.m2) / float64(a.n))
	if disp > p.MaxDispersionM {
		*a = Survey{policy: p}
		return
	}
	if a.n >= p.MinSamples && a.elapsed >= p.Duration {
		g := geo.ECEFToGeodetic(a.mean, physconst.WGS84)
		a.ready = &SurveyCandidate{Position: Surveyed{LatDeg: geo.Deg(g.Lat), LonDeg: geo.Deg(g.Lon), HeightM: g.Height}, Source: "receiver_survey", Duration: a.elapsed, Samples: a.n, DispersionM: disp, MaxReportedAccuracyM: a.maxAccuracy, MinSV: a.minSV, FixType: Fix3D, FirstUTC: a.first.UTC, LastUTC: s.UTC}
	}
}

func (a *Survey) Candidate() *SurveyCandidate {
	if a == nil || a.ready == nil {
		return nil
	}
	c := *a.ready
	return &c
}

// Pin returns an operator-accepted reference; the caller explicitly installs it
// in configuration with a new reception site identity. No automatic acceptance.
func (c SurveyCandidate) Pin(operator, reason, siteID string) (*ReferenceEvidence, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if operator == "" || reason == "" || siteID == "" {
		return nil, fmt.Errorf("integrity: pin requires operator, reason and new site_id")
	}
	u := math.Max(c.MaxReportedAccuracyM, c.DispersionM)
	return &ReferenceEvidence{Source: c.Source, AcceptedBy: operator, Reason: reason, SiteID: siteID, HorizontalUncertaintyM: u, VerticalUncertaintyM: u, Candidate: &c}, nil
}

func validCoordinates(s Solution) bool {
	return finite(s.LatDeg) && finite(s.LonDeg) && finite(s.HeightM) && s.LatDeg >= -90 && s.LatDeg <= 90 && s.LonDeg >= -180 && s.LonDeg <= 180
}
