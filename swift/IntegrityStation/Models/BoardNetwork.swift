import Foundation

struct NetworkDiagnosticExport: Codable, Sendable {
    var schema = "navlisten.network-diagnostics.v1"
    let observer: String?
    let sample: BoardSample
}

// Keep this immutable snapshot indirect. BoardDetails is embedded in several
// StationBoard samples; inlining another large value there can exhaust the
// JSON decoder's worker-thread stack even when the optional fields are absent.
final class BoardNetwork: Codable, Sendable {
    let sampledUptimeMS: UInt64?
    let defaultInterface: String?
    let wifi: NetworkWiFi?
    let ethernet: NetworkEthernet?
    let collector: NetworkCollector?
    let dnsIPv4: [String?]?
    let counters: [String: UInt32]?
    let eventTotal: UInt32?
    let eventsOverwritten: UInt32?
    let events: [NetworkEvent]?
    let lastError: NetworkEvent?
    let spoolRecords: UInt32?
    let spoolHighWaterRecords: UInt32?
    let spoolDroppedRecords: UInt64?
    let sampleFailures: UInt32?
    enum CodingKeys: String, CodingKey {
        case wifi, ethernet, collector, counters, events
        case sampledUptimeMS = "sampled_uptime_ms", defaultInterface = "default_interface", dnsIPv4 = "dns_ipv4"
        case eventTotal = "event_total", eventsOverwritten = "events_overwritten", lastError = "last_error"
        case spoolRecords = "spool_records", spoolHighWaterRecords = "spool_high_water_records"
        case spoolDroppedRecords = "spool_dropped_records", sampleFailures = "sample_failures"
    }
}
struct NetworkIPv4: Codable, Sendable {
    let address: String?
    let netmask: String?
    let gateway: String?
}
struct NetworkWiFi: Codable, Sendable {
    let configured: Bool?
    let associated: Bool?
    let hasIP: Bool?
    let rssiDBM: Int?
    let minimumRSSIDBM: Int?
    let channel: UInt8?
    let secondaryChannel: UInt8?
    let bssid: String?
    let ipv4: NetworkIPv4?
    let lastDisconnectReason: UInt16?
    let lastDisconnectUptimeMS: UInt64?
    let rssiSamples: UInt32?
    let weakRSSISamples: UInt32?
    let retryCount: UInt64?
    enum CodingKeys: String, CodingKey {
        case configured, associated, channel, bssid, ipv4
        case hasIP = "has_ip", rssiDBM = "rssi_dbm", minimumRSSIDBM = "minimum_rssi_dbm"
        case secondaryChannel = "secondary_channel", lastDisconnectReason = "last_disconnect_reason"
        case lastDisconnectUptimeMS = "last_disconnect_uptime_ms", rssiSamples = "rssi_samples"
        case weakRSSISamples = "weak_rssi_samples", retryCount = "retry_count"
    }
}
struct NetworkEthernet: Codable, Sendable {
    let present: Bool?
    let linkUp: Bool?
    let hasIP: Bool?
    let speedMbps: UInt16?
    let duplex: String?
    let autonegotiation: Bool?
    let ipv4: NetworkIPv4?
    let rxFrameErrors: UInt64?
    let txErrors: UInt64?
    enum CodingKeys: String, CodingKey {
        case present, duplex, autonegotiation, ipv4
        case linkUp = "link_up", hasIP = "has_ip", speedMbps = "speed_mbps", rxFrameErrors = "rx_frame_errors", txErrors = "tx_errors"
    }
}
struct NetworkCollector: Codable, Sendable {
    let connected: Bool?
    let viaTunnel: Bool?
    let socketInterface: String?
    let peerIPv4: String?
    let connectedSinceUptimeMS: UInt64?
    let lastACKProgressUptimeMS: UInt64?
    let maximumACKGapMS: UInt32?
    let lastConnectDurationMS: UInt32?
    let maximumConnectDurationMS: UInt32?
    let tlsWrittenBytes: UInt64?
    let tlsReadBytes: UInt64?
    let sentRecordsIncludingReplay: UInt64?
    enum CodingKeys: String, CodingKey {
        case connected
        case viaTunnel = "via_tunnel", socketInterface = "socket_interface", peerIPv4 = "peer_ipv4"
        case connectedSinceUptimeMS = "connected_since_uptime_ms", lastACKProgressUptimeMS = "last_ack_progress_uptime_ms"
        case maximumACKGapMS = "maximum_ack_gap_ms", lastConnectDurationMS = "last_connect_duration_ms"
        case maximumConnectDurationMS = "maximum_connect_duration_ms", tlsWrittenBytes = "tls_written_bytes"
        case tlsReadBytes = "tls_read_bytes", sentRecordsIncludingReplay = "sent_records_including_replay"
    }
}
struct NetworkEvent: Codable, Sendable {
    let sequence: UInt32?
    let uptimeMS: UInt64?
    let kind: String?
    let interface: String?
    let rssiDBM: Int?
    let flags: UInt8?
    let code: Int32?
    let detail: Int32?
    enum CodingKeys: String, CodingKey {
        case sequence, kind, interface, flags, code, detail
        case uptimeMS = "uptime_ms", rssiDBM = "rssi_dbm"
    }
    var title: String { kind?.replacingOccurrences(of: "_", with: " ").capitalized ?? StationFormat.unknown }
    func estimatedDate(in sample: BoardSample) -> Date? {
        guard let utc = WireDate.parse(sample.sampleTime), let reportMS = sample.details?.uptimeMS,
              let uptimeMS, uptimeMS <= reportMS else { return nil }
        return utc.addingTimeInterval(-Double(reportMS - uptimeMS) / 1_000)
    }
}
