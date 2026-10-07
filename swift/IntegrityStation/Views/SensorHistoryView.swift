import Charts
import SwiftUI

struct SensorHistoryView: View {
    @Environment(AppController.self) private var controller
    @Environment(\.scenePhase) private var scenePhase
    let observerID: String
    @State private var metric: SensorMetric = .temperature
    @State private var hours = 1
    @State private var history = SensorHistoryStore()
    @State private var operation: Task<Void, Never>?

    var body: some View {
        InstrumentCard("history.title", systemImage: "chart.xyaxis.line") {
            ViewThatFits(in: .horizontal) {
                HStack { selectors }
                VStack(alignment: .leading) { selectors }
            }
            HStack {
                Button("action.refresh") { operation?.cancel(); operation = Task { await load() } }
                    .disabled(history.isLoading)
                if history.isLoading { ProgressView().controlSize(.small) }
                Spacer()
                Text(metric.unit).font(.caption.monospaced())
            }
            if let error = history.errorMessage {
                Text(error).font(.callout).foregroundStyle(StationPalette.warning)
            } else if let page = history.page {
                SensorHistoryChart(samples: history.samples, metric: metric, hours: hours)
                Text(String(format: String(localized: "history.count"), history.samples.count))
                    .font(.caption).foregroundStyle(.secondary)
                if page.historyLimited {
                    Text(String(format: String(localized: "history.policy"), BoardFormat.timestamp(page.visibleSince)))
                        .font(.caption).foregroundStyle(.secondary)
                }
                if page.paginationLimited || history.samples.count >= 5000 {
                    Text("history.narrow").font(.caption).foregroundStyle(.secondary)
                }
                if history.nextRequest != nil {
                    Button("history.more") { operation?.cancel(); operation = Task { await load(append: true) } }
                        .disabled(history.isLoading)
                }
                sampleTable
            } else if !history.isLoading {
                Text("history.waiting").font(.caption).foregroundStyle(.secondary)
            }
            Text("history.note").font(.caption).foregroundStyle(.secondary)
            if metric == .phase { Text("history.phase_note").font(.caption).foregroundStyle(.secondary) }
        }
        .task(id: Selection(observer: observerID, metric: metric, hours: hours,
                            session: controller.store.activeSession, active: scenePhase == .active,
                            clockAvailable: controller.store.collectorTime != nil)) {
            operation?.cancel()
            await load()
        }
        .onDisappear { operation?.cancel(); history.reset() }
    }

    @ViewBuilder private var selectors: some View {
        Picker("history.measurement", selection: $metric) {
            ForEach(SensorMetric.allCases) { Text($0.title).tag($0) }
        }
        Picker("history.window", selection: $hours) {
            Text("history.hour").tag(1); Text("history.six_hours").tag(6)
            Text("history.day").tag(24); Text("history.week").tag(168)
        }
    }

    private var sampleTable: some View {
        DisclosureGroup("history.samples") {
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 10) {
                    ForEach(Array(history.samples.enumerated()), id: \.offset) { _, sample in
                        VStack(alignment: .leading, spacing: 3) {
                            HStack {
                                Text(sample.receivedAt).font(.caption.monospaced())
                                Spacer()
                                Text(BoardFormat.measurement(metric.value(in: sample), unit: metric.unit))
                            }
                            Text(String(format: String(localized: "history.source"),
                                        sample.sampleTime ?? String(localized: "board.freshness.unknown")))
                            Text(String(format: String(localized: "history.session"), sample.session ?? StationFormat.unknown))
                            Text(BoardFormat.hardwareTrust(sample.hardwareTrust))
                        }
                        .font(.caption).textSelection(.enabled)
                        Divider()
                    }
                }
            }.frame(maxHeight: 300)
        }
    }

    @MainActor private func load(append: Bool = false) async {
        guard scenePhase == .active, let session = controller.store.activeSession, session.audience.isPrivate,
              let clock = controller.store.collectorTime else { history.reset(); return }
        do {
            let request: SensorHistoryRequest
            if append {
                guard let next = history.nextRequest else { return }
                request = next
            } else {
                request = try SensorHistoryRequest(observer: observerID, metric: metric, hours: hours, now: clock)
            }
            try await history.load(session: session, request: request, append: append) { session, request in
                try await controller.feedClient.fetchSensorHistory(session: session, request: request)
            }
        } catch {
            await controller.store.handleExternalReadFailure(error, session: session)
        }
    }

    private struct Selection: Hashable {
        let observer: String
        let metric: SensorMetric
        let hours: Int
        let session: ReadSession?
        let active: Bool
        let clockAvailable: Bool
    }
}

struct SensorHistoryChart: View {
    let samples: [SensorHistorySample]
    let metric: SensorMetric
    let hours: Int

    var body: some View {
        let points = SensorHistoryPoint.make(samples: samples, metric: metric)
        let counts = Dictionary(grouping: points, by: \.segment).mapValues(\.count)
        return Group {
            if points.isEmpty {
                Text("history.empty").frame(maxWidth: .infinity, minHeight: 180)
            } else {
                Chart(points) { point in
                    if counts[point.segment] == 1 {
                        PointMark(x: .value("UTC", point.time), y: .value(metric.unit, point.value))
                            .foregroundStyle(StationPalette.accent)
                    } else {
                        LineMark(x: .value("UTC", point.time), y: .value(metric.unit, point.value),
                                 series: .value("Segment", point.segment))
                            .foregroundStyle(StationPalette.accent)
                            .interpolationMethod(.linear)
                    }
                }
                .chartXAxis {
                    AxisMarks { value in
                        AxisGridLine(); AxisTick()
                        AxisValueLabel {
                            if let time = value.as(Date.self) {
                                Text(String(time.ISO8601Format().dropFirst(hours >= 24 ? 5 : 11).prefix(hours >= 24 ? 11 : 5)))
                            }
                        }
                    }
                }
                .frame(height: 220)
                .accessibilityLabel(Text("history.chart"))
            }
        }
    }
}
