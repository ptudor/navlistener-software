import SwiftUI

struct NetworkDiagnosticsView: View {
    let network: BoardNetwork
    let sample: BoardSample
    let observerID: String?
    var showHistory = true

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            MetricRow(label: "network.connected", value: BoardFormat.flag(network.collector?.connected))
            MetricRow(label: "network.path", value: network.collector?.socketInterface ?? StationFormat.unknown)
            MetricRow(label: "network.route", value: network.defaultInterface ?? StationFormat.unknown)
            if let wifi = network.wifi, wifi.configured == true {
                MetricRow(label: "network.wifi_link", value: BoardFormat.flag(wifi.associated))
                MetricRow(label: "network.rssi", value: dbm(wifi.rssiDBM))
                MetricRow(label: "network.minimum", value: dbm(wifi.minimumRSSIDBM))
                MetricRow(label: "network.channel", value: BoardFormat.count(wifi.channel))
                MetricRow(label: "network.wifi_drops", value: BoardFormat.count(network.counters?["wifi_disconnects"]))
                MetricRow(label: "network.wifi_reason", value: BoardFormat.count(wifi.lastDisconnectReason))
            }
            if let eth = network.ethernet, eth.present == true {
                MetricRow(label: "network.ethernet_link", value: BoardFormat.flag(eth.linkUp))
                MetricRow(label: "network.speed", value: eth.speedMbps.map { "\($0) Mbps" } ?? StationFormat.unknown)
                MetricRow(label: "network.duplex", value: eth.duplex ?? StationFormat.unknown)
                MetricRow(label: "network.autoneg", value: BoardFormat.flag(eth.autonegotiation))
            }
            MetricRow(label: "network.connections", value: BoardFormat.count(network.counters?["collector_connections"]))
            MetricRow(label: "network.ack_stalls", value: BoardFormat.count(network.counters?["ack_stalls"]))
            MetricRow(label: "network.queue_peak", value: BoardFormat.count(network.spoolHighWaterRecords))
            MetricRow(label: "board.resources.dropped", value: BoardFormat.count(network.spoolDroppedRecords))
            if let error = network.lastError {
                DisclosureGroup("network.last_error") { event(error) }
            }
            DisclosureGroup("network.events") {
                ForEach(Array((network.events ?? []).enumerated().reversed()), id: \.offset) { _, item in
                    event(item)
                    Divider()
                }
                MetricRow(label: "network.overwritten", value: BoardFormat.count(network.eventsOverwritten))
            }
            DisclosureGroup("network.addresses") {
                if let wifi = network.wifi {
                    MetricRow(label: "network.bssid", value: wifi.bssid ?? StationFormat.unknown, monospaced: true)
                    address(wifi.ipv4, label: "Wi-Fi")
                }
                address(network.ethernet?.ipv4, label: "Ethernet")
                MetricRow(label: "network.dns", value: dnsServers, monospaced: true)
                MetricRow(label: "network.peer", value: network.collector?.peerIPv4 ?? StationFormat.unknown, monospaced: true)
            }
            DisclosureGroup("network.counters") {
                ForEach((network.counters ?? [:]).keys.sorted(), id: \.self) { name in
                    LabeledContent(name.replacingOccurrences(of: "_", with: " ").capitalized,
                                   value: BoardFormat.count(network.counters?[name]))
                }
                MetricRow(label: "network.ack_gap", value: milliseconds(network.collector?.maximumACKGapMS))
                MetricRow(label: "network.connect_time", value: milliseconds(network.collector?.lastConnectDurationMS))
                MetricRow(label: "network.connect_max", value: milliseconds(network.collector?.maximumConnectDurationMS))
                MetricRow(label: "network.tx", value: BoardFormat.count(network.collector?.tlsWrittenBytes))
                MetricRow(label: "network.rx", value: BoardFormat.count(network.collector?.tlsReadBytes))
                MetricRow(label: "network.samples", value: BoardFormat.count(network.wifi?.rssiSamples))
                MetricRow(label: "network.weak_samples", value: BoardFormat.count(network.wifi?.weakRSSISamples))
                MetricRow(label: "network.sample_failures", value: BoardFormat.count(network.sampleFailures))
            }
            Text("network.note").font(.caption).foregroundStyle(.secondary)
            if showHistory, let observerID {
                NavigationLink {
                    SensorHistoryView(observerID: observerID, initialMetric: network.wifi?.configured == true ? .networkRSSI : .networkConnections)
                } label: { Label("network.history", systemImage: "chart.xyaxis.line") }
            }
            if let json = snapshotJSON {
                ShareLink(item: json) { Label("network.export", systemImage: "square.and.arrow.up") }
            }
        }
    }

    private func event(_ event: NetworkEvent) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(event.title).font(.callout.weight(.medium))
            if let date = event.estimatedDate(in: sample) {
                Text(date.formatted(date: .abbreviated, time: .standard)).font(.caption)
            }
            Text("+\(event.uptimeMS.map { String($0 / 1_000) } ?? "?") s · \(event.interface ?? "?") · \(event.code ?? 0) / \(event.detail ?? 0)")
                .font(.caption.monospaced()).textSelection(.enabled)
            if let rssi = event.rssiDBM { Text(dbm(rssi)).font(.caption) }
        }.frame(maxWidth: .infinity, alignment: .leading)
    }
    private func address(_ ip: NetworkIPv4?, label: String) -> some View {
        VStack(alignment: .leading) {
            Text(label).font(.caption.weight(.medium))
            MetricRow(label: "network.ip", value: ip?.address ?? StationFormat.unknown, monospaced: true)
            MetricRow(label: "network.mask", value: ip?.netmask ?? StationFormat.unknown, monospaced: true)
            MetricRow(label: "network.gateway", value: ip?.gateway ?? StationFormat.unknown, monospaced: true)
        }
    }
    private func dbm(_ value: Int?) -> String { value.map { "\($0) dBm" } ?? StationFormat.unknown }
    private func milliseconds(_ value: UInt32?) -> String { value.map { "\($0) ms" } ?? StationFormat.unknown }
    private var dnsServers: String {
        let addresses = (network.dnsIPv4 ?? []).compactMap { $0 }
        return addresses.isEmpty ? StationFormat.unknown : addresses.joined(separator: ", ")
    }
    private var snapshotJSON: String? {
        let encoder = JSONEncoder(); encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        guard let data = try? encoder.encode(NetworkDiagnosticExport(observer: observerID, sample: sample)) else { return nil }
        return String(data: data, encoding: .utf8)
    }
}
