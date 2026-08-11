import SwiftUI

struct StationDetailView: View {
    @Environment(AppController.self) private var controller
    let stationID: String

    private var observer: Observer? {
        controller.store.observers.first { $0.id == stationID }
    }

    private var stationEvents: [GNSSAPIEvent] {
        controller.store.events.filter { $0.stationID == stationID }
    }

    var body: some View {
        ZStack {
            InstrumentBackground()
            TimelineView(.periodic(from: .now, by: 1)) { _ in
                ScrollView {
                    VStack(alignment: .leading, spacing: 14) {
                        header
                        LazyVGrid(
                            columns: [GridItem(.adaptive(minimum: 300), alignment: .top)],
                            alignment: .leading,
                            spacing: 14
                        ) {
                            skyCard
                            rfCard
                            identityCard
                        }
                        signalsCard
                        timelineCard
                    }
                    .padding(16)
                    .frame(maxWidth: 1080)
                    .frame(maxWidth: .infinity)
                }
                .refreshable { await controller.refresh() }
            }
        }
        .navigationTitle(controller.settings.label(for: stationID) ?? observer?.remark ?? String(localized: "station.detail.title"))
    }

    private var header: some View {
        HStack(alignment: .top, spacing: 12) {
            VStack(alignment: .leading, spacing: 4) {
                HealthLabel(state: controller.store.health(for: stationID))
                Text(stationID)
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
            Spacer()
            VStack(alignment: .trailing, spacing: 3) {
                Text(StationFormat.age(seconds: controller.store.currentLastSeenAge(for: stationID)))
                    .font(.callout.monospacedDigit())
                Text(StationFormat.uptime(seconds: observer?.uptimeS))
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
        }
    }

    private var skyCard: some View {
        InstrumentCard("station.sky.title", systemImage: "scope") {
            SkyPlotView(signals: observer?.svs ?? [:])
        }
    }

    private var rfCard: some View {
        InstrumentCard("station.rf.title", systemImage: "antenna.radiowaves.left.and.right") {
            if let rf = observer?.rf {
                VStack(alignment: .leading, spacing: 10) {
                    rfVerdict
                    MetricRow(label: "station.rf.trust", value: formatRFTrust(rf.rfTrust))
                    MetricRow(
                        label: "station.rf.cn0_mean",
                        value: formatMeasurement(rf.cn0MeanDbHz, unit: String(localized: "unit.db_hz"))
                    )
                    ForEach(Array((rf.bands ?? []).enumerated()), id: \.offset) { _, band in
                        Divider()
                        Text(String(format: String(localized: "station.rf.band"), band.block.map(String.init) ?? StationFormat.unknown))
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(.secondary)
                        MetricRow(label: "station.rf.agc_departure", value: formatMeasurement(band.agcDeparture, unit: ""))
                        MetricRow(
                            label: "station.rf.cw_indicator",
                            value: band.cwSuppress.map {
                                String(format: String(localized: "station.rf.cw_value"), $0)
                            } ?? StationFormat.unknown
                        )
                        MetricRow(label: "station.rf.noise", value: band.noiseLevel.map(String.init) ?? StationFormat.unknown)
                        MetricRow(label: "station.rf.jam_state", value: jamStateName(band.jamState))
                    }
                }
            } else {
                Text("station.rf.not_reported")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
        }
    }

    private var rfVerdict: some View {
        let events = controller.store.activeEvents(for: stationID).filter {
            $0.type == "jamming_detected" || $0.type == "spoofing_suspected" || $0.type == "station_rf_degraded"
        }
        let severity = events.compactMap(\.severity).max { $0.rawValue < $1.rawValue }
        return Label {
            Text(String(localized: severity == nil ? "station.rf.no_confirmed_alert" : "station.rf.confirmed_alert"))
        } icon: {
            Image(systemName: severity == .critical ? "exclamationmark.octagon.fill" : severity == .warning ? "exclamationmark.triangle.fill" : "checkmark.shield")
        }
        .font(.subheadline.weight(.semibold))
        .foregroundStyle(StationPalette.severity(severity))
    }

    private var identityCard: some View {
        InstrumentCard("station.identity.title", systemImage: "shippingbox") {
            VStack(spacing: 8) {
                MetricRow(label: "station.identity.id", value: stationID, monospaced: true)
                MetricRow(label: "station.identity.vendor", value: observer?.vendor ?? StationFormat.unknown)
                MetricRow(label: "station.identity.hardware", value: observer?.hwVersion ?? StationFormat.unknown)
                MetricRow(label: "station.identity.software", value: observer?.swVersion ?? StationFormat.unknown)
                MetricRow(label: "station.identity.git", value: observer?.gitHash.map { String($0.prefix(8)) } ?? StationFormat.unknown, monospaced: true)
                MetricRow(label: "station.identity.serial", value: observer?.serialNo ?? StationFormat.unknown, monospaced: true)
                MetricRow(label: "station.identity.clock", value: StationFormat.clockDrift(nanoseconds: observer?.clockDriftNs))
                MetricRow(
                    label: "station.identity.accuracy",
                    value: formatMeasurement(observer?.accuracyM, unit: String(localized: "unit.meter"))
                )
                MetricRow(label: "station.identity.location", value: formatLocation(observer), monospaced: true)
            }
        }
    }

    private var signalsCard: some View {
        InstrumentCard("station.signals.title", systemImage: "chart.bar.xaxis") {
            let rows = (observer?.svs ?? [:]).map { SignalRow(key: $0.key, signal: $0.value) }
                .sorted {
                    if $0.signal.gnssid != $1.signal.gnssid {
                        return ($0.signal.gnssid ?? Int.max) < ($1.signal.gnssid ?? Int.max)
                    }
                    if $0.signal.svid != $1.signal.svid {
                        return ($0.signal.svid ?? Int.max) < ($1.signal.svid ?? Int.max)
                    }
                    return $0.key < $1.key
                }
            if rows.isEmpty {
                Text("stations.signals.not_reported")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            } else {
                LazyVGrid(columns: [GridItem(.adaptive(minimum: 230), spacing: 14)], spacing: 10) {
                    ForEach(rows) { row in SignalStrengthRow(row: row) }
                }
            }
        }
    }

    private var timelineCard: some View {
        InstrumentCard("station.timeline.title", systemImage: "clock.arrow.trianglehead.counterclockwise.rotate.90") {
            if stationEvents.isEmpty {
                Text("station.timeline.empty")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            } else {
                VStack(spacing: 0) {
                    ForEach(Array(stationEvents.prefix(12).enumerated()), id: \.offset) { index, event in
                        EventRowView(event: event, showsStation: false)
                        if index < min(stationEvents.count, 12) - 1 { Divider() }
                    }
                }
            }
        }
    }

    private func formatRFTrust(_ value: Double?) -> String {
        guard let value else { return StationFormat.unknown }
        return value.formatted(.percent.precision(.fractionLength(0)))
    }

    private func formatMeasurement(_ value: Double?, unit: String) -> String {
        guard let value else { return StationFormat.unknown }
        let number = value.formatted(.number.precision(.fractionLength(0...2)))
        return unit.isEmpty ? number : "\(number) \(unit)"
    }

    private func formatLocation(_ observer: Observer?) -> String {
        guard let latitude = observer?.latitudeDeg, let longitude = observer?.longitudeDeg else {
            return StationFormat.unknown
        }
        // Variable precision avoids padding a policy-thinned 0.1° coordinate
        // into a claim of false exactness (GROUPS-AND-FEDERATION.md §6.2).
        let style = FloatingPointFormatStyle<Double>.number.precision(.fractionLength(0...6))
        return "\(latitude.formatted(style))°, \(longitude.formatted(style))°"
    }

    private func jamStateName(_ state: Int?) -> String {
        switch state {
        case 0: String(localized: "station.rf.jam.unknown")
        case 1: String(localized: "station.rf.jam.clear")
        case 2: String(localized: "station.rf.jam.warning")
        case 3: String(localized: "station.rf.jam.critical")
        default: StationFormat.unknown
        }
    }
}

private struct SignalRow: Identifiable {
    let key: String
    let signal: StationSignal
    var id: String { key }
}

private struct SignalStrengthRow: View {
    @Environment(\.colorScheme) private var colorScheme
    let row: SignalRow

    var body: some View {
        VStack(alignment: .leading, spacing: 5) {
            HStack {
                Circle()
                    .fill(StationPalette.constellation(gnssid: row.signal.gnssid ?? -1, scheme: colorScheme))
                    .frame(width: 7, height: 7)
                Text(row.signal.name ?? row.key)
                    .font(.caption.monospaced().weight(.semibold))
                Spacer()
                Text(StationFormat.carrierToNoise(row.signal.cn0DbHz))
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
            ProgressView(value: min(1, max(0, Double(row.signal.cn0DbHz ?? 0) / 60)))
                .tint(StationPalette.constellation(gnssid: row.signal.gnssid ?? -1, scheme: colorScheme))
        }
    }
}
