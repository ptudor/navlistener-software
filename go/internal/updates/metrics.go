package updates

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/wire"
	"strconv"
	"time"
)

var (
	checks        = promauto.NewCounter(prometheus.CounterOpts{Name: "navlistener_update_checks_total", Help: "Authenticated device reports of completed metadata checks."})
	failures      = promauto.NewCounterVec(prometheus.CounterOpts{Name: "navlistener_update_failures_total", Help: "Transitions into reported updater errors, by stable error name."}, []string{"error"})
	rollbacks     = promauto.NewCounter(prometheus.CounterOpts{Name: "navlistener_update_rollbacks_total", Help: "Reported trial boot rollbacks."})
	securityDrift = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_security_drift", Help: "1 when an enrolled update device's reports contradict each other or what the collector verified: a trusted build that is not fully locked, an open or test build on a locked chip, or, on a collector that verifies hardware evidence, a device reporting the trusted track whose session did not verify as trusted hardware."}, []string{"observer"})
	trustProfile  = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_trust_profile_info", Help: "Device-reported update track per enrolled device; value is 1, superseded labels are removed. A report is a label, not evidence of the running firmware."}, []string{"observer", "profile"})
	hardwareTrust = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_hardware_trust_info", Help: "Hardware trust the collector verified for the reporting session of each enrolled update device (none, open, test, trusted); value is 1, superseded labels are removed."}, []string{"observer", "trust"})
	lastReport    = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_report_timestamp_seconds", Help: "Collector receipt time for the latest authenticated updater report."}, []string{"observer"})
	stagedAt      = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_staged_timestamp_seconds", Help: "First reported staging time; zero when no image is staged."}, []string{"observer"})
	waitingAt     = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_waiting_timestamp_seconds", Help: "First report waiting for a safe reboot; zero otherwise."}, []string{"observer"})
	adoption      = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_running_release_info", Help: "Current reported release per enrolled device; value is 1, superseded labels are removed."}, []string{"observer", "release"})
)

// drifted reports security flags that contradict the reported track. Open and
// test builds belong on unlocked chips, so an unlocked open device is expected
// rather than drifting; everything else must carry the complete locked profile.
func drifted(s wire.UpdateStatus) bool {
	if s.Profile == "open" || s.Profile == "test" {
		return s.Security&3 != 0
	}
	return s.Security != 31
}

// unverifiedTrusted reports a device that says it follows the trusted track on
// a session whose hardware evidence the collector did not verify as trusted:
// the case a report alone can never rule out, since firmware on unlocked
// hardware can report anything. It applies only where the collector verifies
// evidence at all; elsewhere no session is ever verified and the comparison
// would flag the whole fleet.
func unverifiedTrusted(s wire.UpdateStatus, trust string, verifies bool) bool {
	return verifies && s.Profile == "trusted" && trust != string(identity.HardwareTrustTrusted)
}

// verified holds the collector's own result for one report. oldTrust is the
// previous report's, so a changed value retires its label.
type verified struct {
	trust, oldTrust string
	verifies        bool
}

func observe(observer string, old *wire.UpdateStatus, s wire.UpdateStatus, v verified, now time.Time) {
	lastReport.WithLabelValues(observer).Set(float64(now.Unix()))
	securityDrift.WithLabelValues(observer).Set(0)
	if drifted(s) || unverifiedTrusted(s, v.trust, v.verifies) {
		securityDrift.WithLabelValues(observer).Set(1)
	}
	if v.oldTrust != "" && v.oldTrust != v.trust {
		hardwareTrust.DeleteLabelValues(observer, v.oldTrust)
	}
	hardwareTrust.WithLabelValues(observer, v.trust).Set(1)
	if old != nil && old.Profile != s.Profile {
		trustProfile.DeleteLabelValues(observer, old.Profile)
	}
	trustProfile.WithLabelValues(observer, s.Profile).Set(1)
	if s.LastCheck != 0 && (old == nil || s.LastCheck != old.LastCheck) {
		checks.Inc()
	}
	if s.ErrorDomain != 0 && (old == nil || s.Error != old.Error) {
		failures.WithLabelValues(wire.UpdateErrorName(s.ErrorDomain, s.ErrorReason)).Inc()
	}
	if s.State == "rolled-back" && (old == nil || old.State != s.State || old.Failed != s.Failed) {
		rollbacks.Inc()
	}
	if s.Staged == 0 {
		stagedAt.WithLabelValues(observer).Set(0)
	} else if old == nil || old.Staged != s.Staged {
		stagedAt.WithLabelValues(observer).Set(float64(now.Unix()))
	}
	if s.State != "waiting-safe" {
		waitingAt.WithLabelValues(observer).Set(0)
	} else if old == nil || old.State != s.State {
		waitingAt.WithLabelValues(observer).Set(float64(now.Unix()))
	}
	if old != nil && old.Running != s.Running {
		adoption.DeleteLabelValues(observer, strconv.FormatUint(old.Running, 10))
	}
	adoption.WithLabelValues(observer, strconv.FormatUint(s.Running, 10)).Set(1)
}
