package integrity

import (
	"math"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/geo"
	"github.com/ptudor/gnss/physconst"
)

// Position-domain check names and versions.
const (
	CheckStaticPosition     = "static_position"
	CheckStationaryVelocity = "stationary_velocity"
	CheckMotionBound        = "motion_bound"
	CheckPositionVelocity   = "position_velocity"
)

var (
	staticPositionInfo     = Info{Name: CheckStaticPosition, Version: 2, Domain: DomainPosition}
	stationaryVelocityInfo = Info{Name: CheckStationaryVelocity, Version: 1, Domain: DomainPosition}
	motionBoundInfo        = Info{Name: CheckMotionBound, Version: 1, Domain: DomainPosition}
	positionVelocityInfo   = Info{Name: CheckPositionVelocity, Version: 1, Domain: DomainPosition}
)

// Reason codes for the position checks.
const (
	ReasonNoSurveyedPosition = "no_surveyed_position"
	ReasonNo3DFix            = "no_3d_fix"
	ReasonNoMaxSpeed         = "no_max_speed"
	ReasonHorizontalError    = "horizontal_error"
	ReasonVerticalError      = "vertical_error"
	ReasonMeanOffset         = "mean_offset"
	ReasonSpeed              = "speed"
	ReasonMotionBound        = "motion_bound"
	ReasonReferenceReset     = "reference_reset"
	ReasonNoBaseEpoch        = "no_base_epoch"
	ReasonVelocityResidual   = "velocity_residual"
)

// ScaledBand is a threshold that scales with a reported accuracy:
// clamp(Sigmas × accuracy, Min, Max). Min keeps a receiver that reports an
// optimistic accuracy from tripping on ordinary noise; Max keeps one that reports a
// poor accuracy from widening the band without limit.
type ScaledBand struct {
	Sigmas float64 `json:"sigmas"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

func (b ScaledBand) at(accuracy float64) float64 {
	return math.Min(math.Max(b.Sigmas*accuracy, b.Min), b.Max)
}

// ScaledBands are the inconsistent and unassured thresholds of one measurement.
type ScaledBands struct {
	Inconsistent ScaledBand `json:"inconsistent"`
	Unassured    ScaledBand `json:"unassured"`
}

// classify returns the state for a value against both bands at an accuracy, and
// the two thresholds it applied.
func (b ScaledBands) classify(value, accuracy float64) (State, float64, float64) {
	inc, un := b.Inconsistent.at(accuracy), b.Unassured.at(accuracy)
	switch {
	case value > un:
		return Unassured, inc, un
	case value > inc:
		return Inconsistent, inc, un
	}
	return Assured, inc, un
}

// StaticPositionProfile holds the static_position operating points.
type StaticPositionProfile struct {
	// Horizontal and Vertical bands are in metres, scaled by the reported
	// horizontal and vertical accuracy.
	Horizontal ScaledBands `json:"horizontal"`
	Vertical   ScaledBands `json:"vertical"`
	// The mean offset over DriftWindow (needing at least DriftMinEpochs epochs)
	// beyond DriftHorizontalM/DriftVerticalM is inconsistent, and beyond the
	// unassured pair unassured: a slow drag the instantaneous bands allow. A
	// sustained mean is stronger evidence than one epoch, so its unassured bands
	// are tighter than the instantaneous ones.
	DriftWindow               time.Duration `json:"drift_window_ns"`
	DriftMinEpochs            int           `json:"drift_min_epochs"`
	DriftHorizontalM          float64       `json:"drift_horizontal_m"`
	DriftVerticalM            float64       `json:"drift_vertical_m"`
	DriftUnassuredHorizontalM float64       `json:"drift_unassured_horizontal_m"`
	DriftUnassuredVerticalM   float64       `json:"drift_unassured_vertical_m"`
}

// MotionBoundProfile holds the motion_bound operating points.
type MotionBoundProfile struct {
	// The distance from the reference epoch may not exceed
	// max speed × elapsed + Sigmas × (both 3D accuracies) + MarginM.
	Sigmas  float64 `json:"sigmas"`
	MarginM float64 `json:"margin_m"`
	// A reference older than MaxReferenceAge (receiver time) is replaced by the
	// current epoch, which is then unavailable.
	MaxReferenceAge time.Duration `json:"max_reference_age_ns"`
}

// PositionVelocityProfile holds the position_velocity operating points.
type PositionVelocityProfile struct {
	// Window is the position-difference interval; the base epoch may be up to
	// MaxSlack older than Window.
	Window   time.Duration `json:"window_ns"`
	MaxSlack time.Duration `json:"max_slack_ns"`
	// Bands are in m/s, scaled by the RMS speed accuracy over the window.
	Bands ScaledBands `json:"bands"`
}

// maxEpochGapMS bounds the receiver-time gap across which kinematic history is kept.
const maxEpochGapMS = 10_000

// epoch is one accepted 3D solution in the kinematic history.
type epoch struct {
	t      int64 // continuous receiver time, ms
	pos    gnss.ECEF
	vel    gnss.ECEF
	acc3D  float64
	sAcc   float64
	enu    [3]float64 // offset from the surveyed position, when one exists
	hasENU bool
}

// positionChecks evaluates the position domain from solution epochs.
type positionChecks struct {
	station     StationProfile
	surveyed    gnss.ECEF
	surveyedGeo geo.Geodetic

	lastTOW  uint32
	clock    int64 // continuous receiver time of the latest epoch, ms
	started  bool
	history  []epoch // accepted 3D epochs within the longest window, oldest first
	sumENU   [3]float64
	countENU int

	reference *epoch // motion_bound's last epoch that was within bounds
}

func newPositionChecks(station StationProfile) *positionChecks {
	p := &positionChecks{station: station}
	if station.Position != nil {
		p.surveyedGeo = geo.Geodetic{Lat: geo.Rad(station.Position.LatDeg), Lon: geo.Rad(station.Position.LonDeg), Height: station.Position.HeightM}
		p.surveyed = geo.GeodeticToECEF(p.surveyedGeo, physconst.WGS84)
	}
	return p
}

// infos lists the position checks that apply to the station's installation.
func (p *positionChecks) infos() []Info {
	switch p.station.Mode {
	case ModeFixed:
		return []Info{staticPositionInfo, stationaryVelocityInfo, positionVelocityInfo}
	case ModeMobile:
		return []Info{motionBoundInfo, positionVelocityInfo}
	}
	return []Info{positionVelocityInfo}
}

// accept advances the receiver clock with a new solution. It returns false for a
// duplicate or out-of-order epoch, which must not be evaluated again.
func (p *positionChecks) accept(s Solution) bool {
	if !p.started {
		p.started, p.lastTOW = true, s.TOW
		return true
	}
	d := towDelta(p.lastTOW, s.TOW)
	if d <= 0 {
		return false
	}
	p.lastTOW = s.TOW
	p.clock += d
	if d > maxEpochGapMS {
		p.reset()
	}
	return true
}

func (p *positionChecks) reset() {
	p.history, p.sumENU, p.countENU = p.history[:0], [3]float64{}, 0
}

// evaluate returns every applicable verdict for one accepted solution.
func (p *positionChecks) evaluate(s Solution, prof Profile) map[string]Verdict {
	out := map[string]Verdict{}
	usable := s.Usable3D()
	var e epoch
	if usable {
		e = epoch{t: p.clock, pos: s.ecef(), vel: s.velocityECEF(), acc3D: math.Hypot(s.HAccM, s.VAccM), sAcc: s.SAccMPS}
		if p.station.Position != nil {
			east, north, up := geo.ENU(e.pos.Sub(p.surveyed), p.surveyedGeo)
			e.enu, e.hasENU = [3]float64{east, north, up}, true
		}
	}
	window := prof.PositionVelocity.Window + prof.PositionVelocity.MaxSlack
	if prof.StaticPosition.DriftWindow > window {
		window = prof.StaticPosition.DriftWindow
	}
	p.trim(p.clock - window.Milliseconds())

	switch p.station.Mode {
	case ModeFixed:
		out[CheckStaticPosition] = p.staticPosition(s, e, usable, prof.StaticPosition)
		out[CheckStationaryVelocity] = stationaryVelocity(s, usable, prof.StationaryVelocity)
	case ModeMobile:
		out[CheckMotionBound] = p.motionBound(e, usable, prof.MotionBound)
	}
	out[CheckPositionVelocity] = p.positionVelocity(e, usable, prof.PositionVelocity)

	if usable {
		p.history = append(p.history, e)
		if e.hasENU {
			for i := range e.enu {
				p.sumENU[i] += e.enu[i]
			}
			p.countENU++
		}
	}
	return out
}

// trim drops history older than the cutoff (receiver time, ms).
func (p *positionChecks) trim(cutoff int64) {
	n := 0
	for n < len(p.history) && p.history[n].t < cutoff {
		if p.history[n].hasENU {
			for i := range p.history[n].enu {
				p.sumENU[i] -= p.history[n].enu[i]
			}
			p.countENU--
		}
		n++
	}
	p.history = p.history[n:]
}

func (p *positionChecks) staticPosition(s Solution, e epoch, usable bool, prof StaticPositionProfile) Verdict {
	switch {
	case p.station.Position == nil:
		return Verdict{State: Unavailable, Reasons: []string{ReasonNoSurveyedPosition}}
	case !usable:
		return Verdict{State: Unavailable, Reasons: []string{ReasonNo3DFix}}
	}
	h, v := math.Hypot(e.enu[0], e.enu[1]), math.Abs(e.enu[2])
	hs, hInc, hUn := prof.Horizontal.classify(h, s.HAccM)
	vs, vInc, vUn := prof.Vertical.classify(v, s.VAccM)
	verdict := Verdict{
		State: worse(hs, vs),
		Metrics: map[string]float64{
			"east_m": e.enu[0], "north_m": e.enu[1], "up_m": e.enu[2],
			"horizontal_m": h, "vertical_m": v, "h_acc_m": s.HAccM, "v_acc_m": s.VAccM,
		},
		Thresholds: map[string]float64{
			"horizontal_inconsistent_m": hInc, "horizontal_unassured_m": hUn,
			"vertical_inconsistent_m": vInc, "vertical_unassured_m": vUn,
			"mean_horizontal_inconsistent_m": prof.DriftHorizontalM, "mean_vertical_inconsistent_m": prof.DriftVerticalM,
			"mean_horizontal_unassured_m": prof.DriftUnassuredHorizontalM, "mean_vertical_unassured_m": prof.DriftUnassuredVerticalM,
		},
	}
	if hs != Assured {
		verdict.Reasons = append(verdict.Reasons, ReasonHorizontalError)
	}
	if vs != Assured {
		verdict.Reasons = append(verdict.Reasons, ReasonVerticalError)
	}
	// The mean includes this epoch; history holds the earlier ones in the window.
	n := p.countENU + 1
	verdict.Metrics["mean_epochs"] = float64(n)
	if n >= prof.DriftMinEpochs {
		me := (p.sumENU[0] + e.enu[0]) / float64(n)
		mn := (p.sumENU[1] + e.enu[1]) / float64(n)
		mu := (p.sumENU[2] + e.enu[2]) / float64(n)
		mh, mv := math.Hypot(me, mn), math.Abs(mu)
		verdict.Metrics["mean_horizontal_m"], verdict.Metrics["mean_vertical_m"] = mh, mv
		switch {
		case mh > prof.DriftUnassuredHorizontalM || mv > prof.DriftUnassuredVerticalM:
			verdict.State = worse(verdict.State, Unassured)
			verdict.Reasons = append(verdict.Reasons, ReasonMeanOffset)
		case mh > prof.DriftHorizontalM || mv > prof.DriftVerticalM:
			verdict.State = worse(verdict.State, Inconsistent)
			verdict.Reasons = append(verdict.Reasons, ReasonMeanOffset)
		}
	}
	return verdict
}

func stationaryVelocity(s Solution, usable bool, bands ScaledBands) Verdict {
	if !usable {
		return Verdict{State: Unavailable, Reasons: []string{ReasonNo3DFix}}
	}
	speed := math.Sqrt(s.VelN*s.VelN + s.VelE*s.VelE + s.VelD*s.VelD)
	state, inc, un := bands.classify(speed, s.SAccMPS)
	v := Verdict{
		State:      state,
		Metrics:    map[string]float64{"speed_mps": speed, "s_acc_mps": s.SAccMPS},
		Thresholds: map[string]float64{"speed_inconsistent_mps": inc, "speed_unassured_mps": un},
	}
	if state != Assured {
		v.Reasons = []string{ReasonSpeed}
	}
	return v
}

func (p *positionChecks) motionBound(e epoch, usable bool, prof MotionBoundProfile) Verdict {
	switch {
	case p.station.MaxSpeedMPS <= 0:
		return Verdict{State: Unavailable, Reasons: []string{ReasonNoMaxSpeed}}
	case !usable:
		return Verdict{State: Unavailable, Reasons: []string{ReasonNo3DFix}}
	case p.reference == nil || time.Duration(e.t-p.reference.t)*time.Millisecond > prof.MaxReferenceAge:
		ref := e
		p.reference = &ref
		return Verdict{State: Unavailable, Reasons: []string{ReasonReferenceReset}}
	}
	ref := p.reference
	elapsed := float64(e.t-ref.t) / 1000
	dist := e.pos.Sub(ref.pos).Norm()
	bound := p.station.MaxSpeedMPS*elapsed + prof.Sigmas*(e.acc3D+ref.acc3D) + prof.MarginM
	v := Verdict{
		State:      Assured,
		Metrics:    map[string]float64{"distance_m": dist, "elapsed_s": elapsed},
		Thresholds: map[string]float64{"bound_m": bound, "max_speed_mps": p.station.MaxSpeedMPS},
	}
	if dist > bound {
		v.State, v.Reasons = Unassured, []string{ReasonMotionBound}
		return v
	}
	cur := e
	p.reference = &cur
	return v
}

func (p *positionChecks) positionVelocity(e epoch, usable bool, prof PositionVelocityProfile) Verdict {
	if !usable {
		return Verdict{State: Unavailable, Reasons: []string{ReasonNo3DFix}}
	}
	window, slack := prof.Window.Milliseconds(), prof.MaxSlack.Milliseconds()
	base := -1
	for i := len(p.history) - 1; i >= 0; i-- {
		if e.t-p.history[i].t >= window {
			base = i
			break
		}
	}
	if base < 0 || e.t-p.history[base].t > window+slack {
		return Verdict{State: Unavailable, Reasons: []string{ReasonNoBaseEpoch}}
	}
	span := p.history[base:]
	dt := float64(e.t-span[0].t) / 1000
	derived := e.pos.Sub(span[0].pos).Scale(1 / dt)
	// The reported velocity's time integral over the same interval, by the
	// trapezoid rule over the accepted epochs.
	var integral gnss.ECEF
	var sumSAcc2 float64
	prev := span[0]
	for _, cur := range append(span[1:len(span):len(span)], e) {
		step := float64(cur.t-prev.t) / 1000
		integral = integral.Add(prev.vel.Add(cur.vel).Scale(step / 2))
		prev = cur
	}
	for _, x := range span {
		sumSAcc2 += x.sAcc * x.sAcc
	}
	sumSAcc2 += e.sAcc * e.sAcc
	reported := integral.Scale(1 / dt)
	residual := derived.Sub(reported).Norm()
	sAcc := math.Sqrt(sumSAcc2 / float64(len(span)+1))
	state, inc, un := prof.Bands.classify(residual, sAcc)
	v := Verdict{
		State: state,
		Metrics: map[string]float64{
			"residual_mps": residual, "derived_speed_mps": derived.Norm(), "reported_speed_mps": reported.Norm(),
			"interval_s": dt, "s_acc_mps": sAcc,
		},
		Thresholds: map[string]float64{"residual_inconsistent_mps": inc, "residual_unassured_mps": un},
	}
	if state != Assured {
		v.Reasons = []string{ReasonVelocityResidual}
	}
	return v
}
