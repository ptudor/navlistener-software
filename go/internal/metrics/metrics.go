// Package metrics defines the Prometheus collectors for navlistener.
//
// Collectors are package-level via promauto so any pipeline stage (ingest,
// decode, state) can record without plumbing a registry through — the same
// shape as the radiolistener sibling. Labels stay low-cardinality: source name,
// numeric gnssId (0..7), signal id, and short kinds — never a per-SV identifier.
//
// One bounded exception to the numeric gnssId domain : the regression fix
// defense-in-depth gate in state.Apply rejects a frame whose gnssId is outside
// 0..7-minus-IMES and records it as DecodeErrorsTotal{gnssid="out_of_range",
// kind="gnssid_range"} — the raw byte is deliberately NOT printed, since an
// attacker-supplied id would otherwise mint an unbounded label set. So the
// gnssid label's domain is the numeric ids PLUS that one fixed sentinel;
// cardinality stays bounded. A query written against `gnssid=~"[0-7]"` silently
// drops the very series that flags id corruption — match the sentinel too.
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

	// SourceLastFrameTimestamp is the Unix time each dial source last emitted a
	// decoded frame. Alert on time() - this while source_up == 1: a
	// receiver streaming bytes that never frame (an F9 reset to NMEA-only output,
	// a mis-pointed TCP port) keeps source_up at 1 and frames_total frozen — byte
	// silence trips the idle timeout, frame silence trips the watchdog and shows
	// here. Timestamp, not age: the Prometheus idiom (age is computed at query
	// time and cannot go stale between scrapes), mirroring
	// serve_feed_refresh_timestamp_seconds. regression fix alert-threshold caveat: the
	// regression fix connect-time seed means every watchdog-driven re-dial resets this
	// gauge, capping the observable age near max_frame_silence + backoff — an
	// age alert only fires reliably for thresholds BELOW max_frame_silence; the
	// frame_silence error counter is the unconditional signal for longer
	// outages.
	SourceLastFrameTimestamp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "navlistener_source_last_frame_timestamp_seconds",
		Help: "Unix time of the last decoded frame per dial source.",
	}, []string{"source"})
	SourceSecurityDegraded = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "navlistener_source_security_degraded",
		Help: "1 when a source is explicitly using a degraded unauthenticated transport.",
	}, []string{"source", "reason"})

	// NavCRCFailTotal counts frames dropped for a failed parity/CRC check
	// (docs/CONSTELLATIONS.md §2). When the historian is enabled, raw bytes are
	// enqueued before live-state decode and remain available for forensics even
	// when this check rejects the frame.
	NavCRCFailTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_nav_crc_fail_total",
		Help: "Nav frames dropped for failed parity/CRC, by gnssId, sigId, and source.",
		// the source label makes a single noisy push/federation link
		// visible — the GLONASS Hamming check is deliberately detection-only
		// (no rule-(b) correction), so an elevated per-source reject rate is
		// the operational signal that trade relies on. Cardinality is bounded
		// by the fleet size.
	}, []string{"gnssid", "sigid", "source"})

	// DecodeTotal counts nav frames decoded into a typed message, by
	// constellation and GNF1 message type.
	DecodeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_decode_total",
		Help: "Nav frames decoded into a typed message, by gnssId and msg type.",
	}, []string{"gnssid", "msg_type"})

	// DecodeErrorsTotal counts frames the dispatcher/decoder rejected for a
	// reason other than CRC (unknown type, out-of-range field, short frame).
	// this is the one collector whose gnssid label can carry the
	// non-numeric "out_of_range" sentinel — see the package doc above.
	DecodeErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_decode_errors_total",
		Help: `Nav frames rejected by decode (non-CRC), by gnssId and kind. gnssId is numeric (0..7) except for the clamped "out_of_range" sentinel emitted when the id itself is outside the domain.`,
	}, []string{"gnssid", "kind"})

	// DecodePanicsTotal counts frames dropped by decodeLoop's per-frame recover
	//. A decoder that panics on a specific broadcast bit pattern
	// panics on every recurrence — the same SV re-transmits it every few
	// seconds, or crafted frames replay it deliberately (this is a PNT-defense
	// product; malformed frames are the threat model) — so this is the
	// alertable signal for the failure class decode_errors_total (which counts
	// *rejections*, never panics) can structurally never see. Label bounded:
	// gnssId 0..7.
	DecodePanicsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_decode_panics_total",
		Help: "Nav frames dropped after a recovered decode panic, by gnssId.",
	}, []string{"gnssid"})
	RawObsInvalidTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_raw_observation_invalid_total",
		Help: "RAWX observations rejected before estimator mutation, by source and field.",
	}, []string{"source", "field"})
	CapturedOnlyTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_captured_only_frames_total",
		Help: "Valid raw frames persisted but intentionally not decoded into live state.",
	}, []string{"source", "format"})

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

	// SpoofGatesWired / SpoofGateQuorumGauge  publish the spoofing
	// detector's coverage honestly: spoofing_suspected needs quorum-many
	// independent physics gates agreeing, and while wired < quorum the event is
	// arithmetically unreachable — a deliberate conservative posture that would
	// otherwise be invisible (a permanently silent spoofing channel reads the
	// same as "no spoofing observed"). Set once at startup from the detect
	// constants; alert on wired < quorum to surface the dormancy.
	SpoofGatesWired = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "navlistener_spoof_gates_wired",
		Help: "Independent spoofing physics gates implemented (max the fusion can count).",
	})
	SpoofGateQuorumGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "navlistener_spoof_gate_quorum",
		Help: "Gates that must agree before spoofing_suspected fires; wired < quorum means the detector is dormant.",
	})

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
	PushServerCertNotAfterSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "navlistener_push_server_cert_not_after_seconds",
		Help: "Unix timestamp at which the configured push TLS server certificate expires.",
	})

	// PushAuthFailuresTotal counts rejected feeder handshakes.
	PushAuthFailuresTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_push_auth_failures_total",
		Help: "Feeder handshakes rejected for a bad token or feed grant.",
	})
	PushAuthFailuresByReasonTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_push_auth_failures_by_reason_total",
		Help: "Feeder handshakes rejected, by bounded authentication failure reason.",
	}, []string{"reason"})

	// PushErrorsTotal counts per-connection push errors, by observer and kind.
	PushErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_push_errors_total",
		Help: "Push connection errors, by observer and kind.",
	}, []string{"observer", "kind"})

	// Serve-tier self-observability. The read tier is the daemon's
	// reason to exist, and before these collectors it emitted nothing: a feed
	// frozen on stale cache bytes by a marshal failure, or an SSE consumer being
	// buffer-overflow-kicked in a reconnect loop, was invisible for months.
	// Labels bounded: feed ∈ the five feed-name constants; SSE metrics unlabeled.

	// ServeFeedMarshalErrorsTotal counts feed refreshes whose envelope failed to
	// marshal — the cache then keeps serving the previous (stale) bytes.
	ServeFeedMarshalErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_serve_feed_marshal_errors_total",
		Help: "Feed refreshes whose envelope failed to marshal (cache left serving stale bytes), by feed.",
	}, []string{"feed"})

	// ServeFeedRefreshTimestamp is the Unix time of each feed's last successful
	// refresh; alert on time() - this exceeding a few refresh intervals. (The
	// review sketched an age gauge; a timestamp is the Prometheus idiom — age
	// is computed at query time and cannot go stale between scrapes.)
	ServeFeedRefreshTimestamp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "navlistener_serve_feed_refresh_timestamp_seconds",
		Help: "Unix time of the last successful refresh of each served feed.",
	}, []string{"feed"})

	// SSEClients is the number of currently-connected SSE event streams (out of
	// the sseMaxClients cap — also the signal for an attacker parking streams).
	SSEClients = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "navlistener_sse_clients",
		Help: "SSE event-stream clients currently connected.",
	})

	// SSEEventsDroppedTotal counts events dropped toward a slow client (whose
	// stream is then kicked so EventSource reconnects and replays the gap). A
	// steadily-climbing value is a consumer (intsat) in a kick/reconnect loop.
	SSEEventsDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_sse_events_dropped_total",
		Help: "Events dropped toward a slow SSE client (client kicked to reconnect+replay).",
	})

	// SSESubscribeRejectedTotal counts subscriptions refused at the stream cap.
	SSESubscribeRejectedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_sse_subscribe_rejected_total",
		Help: "SSE subscriptions rejected at the concurrent-stream cap.",
	})

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

	// StoreEmptyRawTotal counts frames dropped at Enqueue for having an empty Raw
	// (would otherwise trip nav_frames.raw's NOT NULL constraint, regression fix).
	StoreEmptyRawTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_store_empty_raw_total",
		Help: "Nav frames dropped at enqueue for having empty Raw bytes (would violate raw NOT NULL).",
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

// Admission-boundary counters added by the astra-6 verification pass. None
// carries an audience, organization or collection label: /metrics is public,
// and regression fix forbids leaking private scope ids through label cardinality.
// `source` is the observer id, which existing decode counters already expose.
var (
	// DurableReceiptsRejectedTotal counts DATA records the durable tracker
	// refused before decoder handoff (the stream then closes for replay). A
	// nonzero rate is the regression fix budget doing its job — or a crash-looping
	// feeder / historian outage consuming it; the reason says which.
	DurableReceiptsRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_durable_receipts_rejected_total",
		Help: "Sequenced push records refused by the durable ACK tracker, by budget reason (sessions, observer_sessions, outstanding, session_outstanding).",
	}, []string{"reason"})
	// DurableHolesAbandonedTotal counts unresolved received sequences dropped
	// from tracking because their session was silent for durableHoleAbandonAfter
	// — far past every feeder's ACK-stall/reconnect window, so no replay is
	// coming. They were never acknowledged; this is bounded forgetting, loudly.
	DurableHolesAbandonedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_durable_holes_abandoned_total",
		Help: "Unresolved received sequences abandoned after their push session stayed silent past the abandonment window, by observer.",
	}, []string{"source"})
	// PushAdmissionRefusedTotal counts sessions refused AFTER a successful
	// WELCOME by the policy admission step, by reason, so a
	// feeder that sees handshake-then-close is explicable from the collector.
	PushAdmissionRefusedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "navlistener_push_admission_refused_total",
		Help: "Push sessions refused by policy admission after WELCOME, by reason (observer_ceiling, canceled, stale_lookup, reset_blocked).",
	}, []string{"reason"})
	// SSEPublishRejectedTotal counts committed events refused at SSE broker
	// admission because the audience's policy generation advanced between the
	// pre-write guard and admission. Rare and expected around a
	// policy reset; a sustained rate means a detector is racing invalidation.
	SSEPublishRejectedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "navlistener_sse_publish_rejected_total",
		Help: "Events refused at SSE broker admission because the audience policy generation had advanced.",
	})
)
