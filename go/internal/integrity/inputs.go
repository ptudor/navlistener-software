package integrity

import (
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/geo"
	"github.com/ptudor/gnss/physconst"
)

// weekMS is one GPS week in milliseconds, the period of the time-of-week fields.
const weekMS = 7 * 24 * 3600 * 1000

// towDelta returns b − a in milliseconds for two GPS times of week, choosing the
// representative within half a week so a week rollover between them is continuous.
func towDelta(a, b uint32) int64 {
	d := int64(b) - int64(a)
	switch {
	case d > weekMS/2:
		d -= weekMS
	case d < -weekMS/2:
		d += weekMS
	}
	return d
}

// Fix types (receiver-neutral; docs/proposals/STATION-ASSURANCE.md §2).
const (
	FixNone         = 0
	FixDeadReckon   = 1
	Fix2D           = 2
	Fix3D           = 3
	FixGNSSAndDR    = 4
	FixTimeOnlyMode = 5
)

// Solution is one epoch of the receiver's own navigation solution.
type Solution struct {
	// Received is the collector-local instant the record arrived (live) or was
	// stored (replay). Hysteresis and staleness run on this clock.
	Received time.Time
	// TOW is the epoch's GPS time of week in milliseconds. Kinematic intervals
	// run on this clock, which is the receiver's own.
	TOW uint32
	// UTC is the receiver's UTC for the epoch; meaningful only when UTCValid
	// (date valid, time valid and fully resolved).
	UTC      time.Time
	UTCValid bool
	// HostStamp is an independent wall-clock stamp of the record from the
	// observer or the collector host, zero when no trustworthy stamp exists.
	HostStamp time.Time

	FixType int
	FixOK   bool
	// InvalidLLH is the receiver's own flag that the coordinates are not valid.
	InvalidLLH bool
	NumSV      int

	LatDeg, LonDeg, HeightM float64
	HAccM, VAccM            float64
	// VelN, VelE, VelD are north, east, down velocity in m/s.
	VelN, VelE, VelD float64
	SAccMPS          float64
}

// usable3D reports whether the solution is a valid three-dimensional fix.
func (s Solution) usable3D() bool {
	return s.FixOK && !s.InvalidLLH && (s.FixType == Fix3D || s.FixType == FixGNSSAndDR)
}

func (s Solution) geodetic() geo.Geodetic {
	return geo.Geodetic{Lat: geo.Rad(s.LatDeg), Lon: geo.Rad(s.LonDeg), Height: s.HeightM}
}

// ecef returns the position in WGS-84 ECEF metres.
func (s Solution) ecef() gnss.ECEF { return geo.GeodeticToECEF(s.geodetic(), physconst.WGS84) }

// velocityECEF rotates the north/east/down velocity into ECEF.
func (s Solution) velocityECEF() gnss.ECEF {
	east, north, up := geo.ENUBasis(s.geodetic())
	return north.Scale(s.VelN).Add(east.Scale(s.VelE)).Add(up.Scale(-s.VelD))
}

// ClockSample is the receiver's clock solution for one epoch.
type ClockSample struct {
	Received time.Time
	TOW      uint32
	// BiasNs is the receiver clock bias and DriftNsPerS its drift.
	BiasNs      float64
	DriftNsPerS float64
	TAccNs      float64
	FAccPsPerS  float64
}

// Receiver spoofing states (u-blox NAV-STATUS spoofDetState and its equivalents).
const (
	SpoofUnknown   = 0 // unknown or deactivated
	SpoofNone      = 1 // no spoofing indicated
	SpoofIndicated = 2
	SpoofMultiple  = 3 // multiple spoofing indications
)

// ReceiverStatus is the receiver's own status for one epoch.
type ReceiverStatus struct {
	Received   time.Time
	SpoofState int
	// SinceStartMS is milliseconds since the receiver started; a decrease means
	// the receiver restarted. Zero when unknown.
	SinceStartMS uint32
	HaveStart    bool
}

// TimingSample is one GNSS PPS and RTC pulse comparison from the observer board.
type TimingSample struct {
	Received time.Time
	// UptimeMS is the observer's boot-relative clock for the sample; a decrease
	// means the observer restarted.
	UptimeMS uint64
	// PhaseNs is the RTC-minus-GNSS pulse phase in [−0.5 s, +0.5 s), nil when
	// either pulse is not currently valid.
	PhaseNs *float64
	// RTCRunning reports the RTC square wave enabled at 1 Hz.
	RTCRunning bool
	// RTCTrim is the RTC's raw trim or aging register. A digital trim inserts
	// deliberate phase steps, so a nonzero value disables the phase test.
	RTCTrim uint8
}

// Cn0Fit is the C/N₀-vs-elevation fit for one NAV-SAT epoch, aggregate and per
// constellation (SBAS excluded per constellation).
type Cn0Fit struct {
	Received         time.Time
	Mean, ResidVar   float64
	NumSats          int
	PerConstellation map[int]Cn0Group
}

// Cn0Group is one constellation's fit.
type Cn0Group struct {
	Mean, ResidVar float64
	NumSats        int
}

// RFBand is one front-end path with its departure from the learned AGC baseline.
type RFBand struct {
	Block int
	// Departure is the learned baseline minus the current AGC, nil while no
	// baseline exists.
	Departure  *float64
	CWSuppress int
	JamState   int
	AntStatus  int
}

// RFSample is one front-end report.
type RFSample struct {
	Received time.Time
	Bands    []RFBand
}
