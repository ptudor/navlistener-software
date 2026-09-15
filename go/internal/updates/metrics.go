package updates

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/ptudor/navlistener/internal/wire"
	"strconv"
	"time"
)

var (
	checks        = promauto.NewCounter(prometheus.CounterOpts{Name: "navlistener_update_checks_total", Help: "Authenticated device reports of completed metadata checks."})
	failures      = promauto.NewCounterVec(prometheus.CounterOpts{Name: "navlistener_update_failures_total", Help: "Transitions into reported updater errors, by stable error name."}, []string{"error"})
	rollbacks     = promauto.NewCounter(prometheus.CounterOpts{Name: "navlistener_update_rollbacks_total", Help: "Reported trial boot rollbacks."})
	securityDrift = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_security_drift", Help: "1 when an explicitly enrolled update device lacks the production security profile."}, []string{"observer"})
	lastReport    = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_report_timestamp_seconds", Help: "Collector receipt time for the latest authenticated updater report."}, []string{"observer"})
	stagedAt      = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_staged_timestamp_seconds", Help: "First reported staging time; zero when no image is staged."}, []string{"observer"})
	waitingAt     = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_waiting_timestamp_seconds", Help: "First report waiting for a safe reboot; zero otherwise."}, []string{"observer"})
	adoption      = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "navlistener_update_running_release_info", Help: "Current reported release per enrolled device; value is 1, superseded labels are removed."}, []string{"observer", "release"})
)

func observe(observer string, old *wire.UpdateStatus, s wire.UpdateStatus, now time.Time) {
	lastReport.WithLabelValues(observer).Set(float64(now.Unix()))
	securityDrift.WithLabelValues(observer).Set(0)
	if s.Security != 31 {
		securityDrift.WithLabelValues(observer).Set(1)
	}
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
