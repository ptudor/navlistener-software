// Package metrics defines the Prometheus collectors for navlistener.
//
// Collectors are package-level via promauto so any pipeline stage (ingest,
// decode, state) can record without plumbing a registry through — the same
// shape as the radiolistener sibling. Labels stay low-cardinality: source name,
// numeric gnssId (0..7), signal id, and short kinds — never a per-SV identifier.
//
// The reusable github.com/ptudor/gnss library never imports this package; the
// daemon records metrics from the typed results and errors the library returns.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/ptudor/navlistener/internal/version"
)

var startTime = time.Now()

var (
	// BuildInfo is a constant 1 carrying version labels.
	BuildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "navlistener_build_info",
		Help: "Build identity; value is always 1.",
	}, []string{"version", "build_time"})

	// FramesTotal counts raw nav frames ingested, by source and constellation.
	FramesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_frames_total",
		Help: "Raw nav frames ingested, by source and gnssId.",
	}, []string{"source", "gnssid"})

	// IngestErrorsTotal counts connection/framing errors per source.
	IngestErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_ingest_errors_total",
		Help: "Ingest connection/framing errors, by source and kind.",
	}, []string{"source", "kind"})

	// SourceUp is 1 when a source's connection is currently established.
	SourceUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "navlistener_source_up",
		Help: "1 if the ingest source connection is up, 0 otherwise.",
	}, []string{"source", "type"})

	// SourceConnectsTotal counts successful (re)connections per source.
	SourceConnectsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_source_connects_total",
		Help: "Connection attempts that succeeded, by source.",
	}, []string{"source"})

	// NavCRCFailTotal counts frames dropped for a failed parity/CRC check
	// (docs/CONSTELLATIONS.md §2). The raw bytes are still preserved for the
	// forensic record once the persist stage lands.
	NavCRCFailTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_nav_crc_fail_total",
		Help: "Nav frames dropped for failed parity/CRC, by gnssId and sigId.",
	}, []string{"gnssid", "sigid"})

	// DecodeTotal counts nav frames decoded into a typed message, by
	// constellation and GNF1 message type.
	DecodeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_decode_total",
		Help: "Nav frames decoded into a typed message, by gnssId and msg type.",
	}, []string{"gnssid", "msg_type"})

	// DecodeErrorsTotal counts frames the dispatcher/decoder rejected for a
	// reason other than CRC (unknown type, out-of-range field, short frame).
	DecodeErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_decode_errors_total",
		Help: "Nav frames rejected by decode (non-CRC), by gnssId and kind.",
	}, []string{"gnssid", "kind"})

	// LiveSVs is the current count of SVs held in live state, by constellation.
	LiveSVs = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "navlistener_live_svs",
		Help: "SVs currently held in live state, by constellation.",
	}, []string{"constellation"})

	// SVsExpiredTotal counts SVs aged out of live state.
	SVsExpiredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_svs_expired_total",
		Help: "SVs expired from live state after their TTL elapsed.",
	})

	// EventsTotal counts confirmed integrity events emitted, by type and severity.
	EventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_events_total",
		Help: "Confirmed integrity events emitted, by type and severity.",
	}, []string{"type", "severity"})

	// EventWriteErrorsTotal counts integrity events that failed to persist.
	EventWriteErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_event_write_errors_total",
		Help: "Integrity events that failed to persist to the historian.",
	})

	// PushConnectsTotal counts authenticated feeder connections, by observer.
	PushConnectsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_push_connects_total",
		Help: "Authenticated feeder connections accepted, by observer.",
	}, []string{"observer"})

	// PushObserversUp is the number of feeder connections currently established, by observer.
	PushObserversUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "navlistener_push_observers_up",
		Help: "Feeder connections currently established, by observer.",
	}, []string{"observer"})

	// PushAuthFailuresTotal counts rejected feeder handshakes.
	PushAuthFailuresTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_push_auth_failures_total",
		Help: "Feeder handshakes rejected for a bad token or feed grant.",
	})

	// PushErrorsTotal counts per-connection push errors, by observer and kind.
	PushErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_push_errors_total",
		Help: "Push connection errors, by observer and kind.",
	}, []string{"observer", "kind"})

	// StoreRowsTotal counts raw nav frames persisted to the historian.
	StoreRowsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_store_rows_total",
		Help: "Raw nav frames written to TimescaleDB.",
	})

	// StoreDroppedTotal counts frames dropped because the writer queue was full.
	StoreDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_store_dropped_total",
		Help: "Nav frames dropped under DB backpressure (queue full).",
	})

	// StoreErrorsTotal counts failed batch-flush attempts (each retry increments).
	StoreErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_store_errors_total",
		Help: "Failed historian batch-flush attempts (includes retries).",
	})

	// StoreQuarantinedTotal counts frames permanently dropped after retries were
	// exhausted or a poison row was quarantined.
	StoreQuarantinedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_store_quarantined_total",
		Help: "Nav frames dropped after flush retries exhausted or a poison row was quarantined.",
	})
)

// Init records build identity and registers the uptime gauge. Call once at startup.
func Init() {
	BuildInfo.WithLabelValues(version.Version, version.BuildTime).Set(1)
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "navlistener_uptime_seconds",
		Help: "Seconds since process start.",
	}, func() float64 { return time.Since(startTime).Seconds() })
}
