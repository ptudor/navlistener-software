import Charts
import SwiftUI

struct OwnerPortalView: View {
    @Environment(\.scenePhase) private var phase
    @State private var portal: OwnerPortalController
    @State private var browser = PortalBrowserSignIn()
    @State private var address = "https://stations.intsat.net/stations/"
    @State private var signIn: Task<Void, Never>?

    init(portal: OwnerPortalController = OwnerPortalController()) { _portal = State(initialValue: portal) }

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    if let identity = portal.identity, portal.connection != nil {
                        Text(identity.user.name.isEmpty ? identity.user.email : identity.user.name).font(.largeTitle.bold())
                        Text(identity.user.email).foregroundStyle(.secondary)
                        Text(portal.connection!.installation.url.absoluteString).font(.caption).textSelection(.enabled)
                        ForEach(identity.fleets) { fleet in
                            NavigationLink { PortalFleetView(fleet: fleet).environment(portal) } label: {
                                InstrumentCard {
                                    HStack { Image(systemName: fleet.kind == "collection" ? "person.3" : "building.2"); Text(fleet.name).font(.headline); Spacer(); Image(systemName: "chevron.right") }
                                    Text(fleet.role.capitalized).font(.caption).foregroundStyle(.secondary)
                                }
                            }.buttonStyle(.plain)
                        }
                        if identity.fleets.isEmpty { Text("portal.no_fleets") }
                        if let connection = portal.connection {
                            Link("portal.account", destination: connection.installation.endpoint("accounts/"))
                        }
                        Button("portal.sign_out", role: .destructive) { Task { await portal.signOut() } }
                    } else if portal.connection != nil {
                        ProgressView("portal.reconnecting")
                        Button("action.refresh") { Task { await portal.refresh() } }
                        Button("portal.sign_out", role: .destructive) { Task { await portal.signOut() } }
                    } else {
                        InstrumentCard("portal.title", systemImage: "person.2.badge.key") {
                            Text("portal.intro")
                            TextField(String(localized: "portal.address"), text: $address)
                                .textFieldStyle(.roundedBorder).accessibilityIdentifier("portal.address")
                                #if os(iOS)
                                .textInputAutocapitalization(.never).keyboardType(.URL)
                                #endif
                            Button("portal.sign_in") {
                                signIn?.cancel()
                                signIn = Task { await portal.connect(to: address) { try await browser.signIn(at: $0) } }
                            }.buttonStyle(.borderedProminent).disabled(portal.isConnecting)
                            if portal.isConnecting {
                                ProgressView()
                                Button("portal.cancel") { signIn?.cancel(); browser.cancel() }
                            }
                            Text("portal.on_prem").font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    if let error = portal.error { FeedErrorBanner(message: error) }
                }.padding(20).frame(maxWidth: 900).frame(maxWidth: .infinity)
            }.background { InstrumentBackground() }.navigationTitle("portal.title")
        }
        .id(portal.epoch)
        .tint(StationPalette.accent)
        .task(id: phase) {
            guard phase == .active else { portal.suspend(); return }
            await portal.restore()
            while !Task.isCancelled {
                await portal.refresh()
                do { try await Task.sleep(for: .seconds(30)) } catch { return }
            }
        }
        .onDisappear { signIn?.cancel(); browser.cancel(); portal.suspend() }
    }
}

struct PortalFleetView: View {
    @Environment(OwnerPortalController.self) private var portal
    let fleet: PortalFleet
    @State private var stations: [PortalStation] = []
    @State private var readings: [UUID: PortalReading] = [:]
    @State private var nextOffset: Int?
    @State private var revision: String?
    @State private var message: String?
    @State private var loading = false
    @State private var search = ""
    @State private var operation: Task<Void, Never>?
    @State private var run = UUID()

    var body: some View {
        List {
            if let message { Text(message).foregroundStyle(StationPalette.warning) }
            if loading { ProgressView() }
            ForEach(stations.filter { search.isEmpty || [$0.label, $0.site, $0.observerId, $0.model].contains(where: { $0.localizedCaseInsensitiveContains(search) }) }) { station in
                NavigationLink { PortalStationView(fleet: fleet, station: station).environment(portal) } label: {
                    VStack(alignment: .leading, spacing: 5) {
                        HStack { Text(station.label).font(.headline); Spacer(); if let reading = readings[station.id] { HealthLabel(state: reading.health) } }
                        Text([station.site, station.model, station.collectorName].filter { !$0.isEmpty }.joined(separator: " · ")).font(.caption).foregroundStyle(.secondary)
                        if let reading = readings[station.id] { Text(reading.statusText).font(.caption) }
                        else { Text(String(format: String(localized: "portal.last_contact"), BoardFormat.timestamp(station.lastContact))).font(.caption) }
                    }
                }
            }
            if nextOffset != nil { Button("portal.more_stations") { operation?.cancel(); operation = Task { await load(append: true) } }.disabled(loading) }
            if stations.isEmpty && !loading && message == nil { Text("portal.no_stations") }
        }
        .searchable(text: $search, prompt: "portal.search")
        .navigationTitle(fleet.name)
        .toolbar { Button("action.refresh", systemImage: "arrow.clockwise") { operation?.cancel(); operation = Task { await load() } }.disabled(loading) }
        .task {
            await load()
            while !Task.isCancelled {
                do { try await Task.sleep(for: .seconds(30)) } catch { return }
                await refreshReadings()
            }
        }
        .onDisappear { operation?.cancel(); run = UUID(); readings = [:] }
    }
    @MainActor private func load(append: Bool = false) async {
        guard let connection = portal.connection else { return }
        let epoch = portal.epoch, id = UUID(); run = id; loading = true; message = nil
        func check() throws { try Task.checkCancellation(); guard run == id, portal.connection == connection, portal.epoch == epoch else { throw CancellationError() } }
        defer { if run == id { loading = false } }
        do {
            var query: [URLQueryItem] = []
            if append, let nextOffset, let revision {
                query = [URLQueryItem(name: "offset", value: String(nextOffset)), URLQueryItem(name: "revision", value: revision)]
            } else { stations = []; readings = [:]; nextOffset = nil; revision = nil }
            let page = try await portal.client.get(PortalInventory.self, connection: connection, path: fleet.path + "inventory/", query: query)
            try check()
            guard page.stations.count <= 500, !page.revision.isEmpty,
                  !append || page.revision == revision,
                  page.nextOffset == nil || page.nextOffset == (append ? nextOffset ?? 0 : 0) + 500,
                  Set(page.stations.map(\.id)).count == page.stations.count,
                  !append || Set(stations.map(\.id)).isDisjoint(with: page.stations.map(\.id))
            else { throw FeedError.invalidResponse }
            stations = append ? stations + page.stations : page.stations; revision = page.revision; nextOffset = page.nextOffset
            for collector in Set(page.stations.map(\.collectorId)) {
                do {
                    let result = try await portal.client.get(PortalReadings.self, connection: connection, path: fleet.path,
                        query: [URLQueryItem(name: "collector", value: collector.uuidString.lowercased())])
                    try check()
                    let allowed = Set(stations.filter { $0.collectorId == collector }.map(\.id))
                    for reading in result.stations where allowed.contains(reading.id) { readings[reading.id] = reading }
                } catch let error as FeedError where !error.isAuthorizationLoss {
                    try check(); message = String(localized: "portal.live_unavailable")
                }
            }
        } catch is CancellationError {} catch {
            guard run == id else { return }; stations = []; readings = [:]; nextOffset = nil
            message = PortalErrorMessage.describe(error); await portal.failed(error)
        }
    }

    @MainActor private func refreshReadings() async {
        guard !loading, let connection = portal.connection else { return }
        let epoch = portal.epoch, id = run
        do {
            for collector in Set(stations.map(\.collectorId)) {
                let response = try await portal.client.get(PortalReadings.self, connection: connection, path: fleet.path,
                    query: [URLQueryItem(name: "collector", value: collector.uuidString.lowercased())])
                try Task.checkCancellation()
                guard run == id, portal.connection == connection, portal.epoch == epoch else { return }
                let allowed = Set(stations.filter { $0.collectorId == collector }.map(\.id))
                readings = readings.filter { !allowed.contains($0.key) }
                for reading in response.stations where allowed.contains(reading.id) { readings[reading.id] = reading }
            }
        } catch is CancellationError {} catch {
            guard run == id else { return }; readings = [:]; message = PortalErrorMessage.describe(error)
            await portal.failed(error)
        }
    }
}

struct PortalStationView: View {
    @Environment(OwnerPortalController.self) private var portal
    let fleet: PortalFleet
    @State var station: PortalStation
    @State private var reading: PortalReading?
    @State private var reference: Date?
    @State private var received: ContinuousClock.Instant?
    @State private var error: String?
    @State private var run = UUID()
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                Text(station.label).font(.largeTitle.bold())
                Text(station.site).foregroundStyle(.secondary)
                if let error { FeedErrorBanner(message: error) }
                TimelineView(.periodic(from: .now, by: 1)) { _ in
                    if let reading {
                        let duration = received?.duration(to: .now).components
                        let elapsed = duration.map { Double($0.seconds) + Double($0.attoseconds) / 1e18 } ?? 0
                        InstrumentCard("portal.receiver_status", systemImage: "antenna.radiowaves.left.and.right") {
                            let offline = reading.lastSeenS.map { $0 + elapsed > 300 } ?? false
                            HStack { Text(offline && reading.status == "reporting" ? String(localized: "health.offline") : reading.statusText); Spacer(); HealthLabel(state: offline ? .offline : reading.health) }
                            Text(BoardFormat.freshness(reading.freshness(at: reference, elapsed: elapsed))).font(.caption).foregroundStyle(.secondary)
                            LabeledContent(String(localized: "history.temperature"), value: BoardFormat.measurement(reading.temperature, unit: "°C"))
                            LabeledContent(String(localized: "history.humidity"), value: BoardFormat.measurement(reading.humidity, unit: "%"))
                            LabeledContent(String(localized: "history.pressure"), value: BoardFormat.measurement(reading.pressure, unit: "Pa"))
                            LabeledContent(String(localized: "board.trust.title"), value: BoardFormat.hardwareTrust(reading.hardwareTrust))
                            let timing = reading.freshness(at: reference, elapsed: elapsed, component: "timing")
                            LabeledContent(String(localized: "history.phase"), value: BoardFormat.measurement(timing == .current || timing == .receiptOnly ? reading.board["timing"]["details"]["timing"]["rtc_minus_gnss_phase_ns"].number : nil, unit: "ns"))
                            Text(BoardFormat.freshness(timing)).font(.caption).foregroundStyle(.secondary)
                        }
                    }
                }
                InstrumentCard("portal.inventory", systemImage: "cpu") { PortalFacts(rows: station.facts) }
                if let reading {
                    InstrumentCard("portal.conditions", systemImage: "waveform.path.ecg") {
                        if !reading.conditionsKnown { Text("portal.conditions_unknown") }
                        else if reading.conditions.isEmpty { Text("portal.conditions_clear") }
                        ForEach(Array(reading.conditions.enumerated()), id: \.offset) { _, condition in
                            Text(condition.message).foregroundStyle(StationPalette.severity(EventSeverity(rawValue: condition.severity)))
                        }
                    }
                    InstrumentCard("portal.diagnostics", systemImage: "sensor") {
                        PortalFacts(rows: reading.details.facts)
                        DisclosureGroup("portal.reception") { PortalFacts(rows: reading.board["reception"].facts) }
                    }
                }
                PortalHistoryView(fleet: fleet, station: station).environment(portal)
            }.padding(20).frame(maxWidth: 950).frame(maxWidth: .infinity)
        }.background { InstrumentBackground() }
        .navigationTitle(station.label)
        .task {
            guard let connection = portal.connection else { return }
            let epoch = portal.epoch, id = UUID(); run = id
            @MainActor func check() throws { try Task.checkCancellation(); guard run == id, portal.connection == connection, portal.epoch == epoch else { throw CancellationError() } }
            while !Task.isCancelled {
                do {
                    try check()
                    let record = try await portal.client.get(PortalStationRecord.self, connection: connection,
                        path: fleet.path + "inventory/\(station.id.uuidString.lowercased())/")
                    try check(); guard record.station.id == station.id else { throw FeedError.invalidResponse }; station = record.station
                    let response = try await portal.client.get(PortalStationResponse.self, connection: connection, path: fleet.path + station.path)
                    try check(); guard response.station.id == station.id else { throw FeedError.invalidResponse }
                    reading = response.station; reference = WireDate.parse(response.collectorTime); received = .now; error = nil
                } catch is CancellationError { return } catch {
                    guard run == id else { return }; reading = nil; reference = nil; received = nil
                    self.error = PortalErrorMessage.describe(error); await portal.failed(error)
                    if (error as? FeedError)?.isAuthorizationLoss == true { return }
                }
                let fast = reading?.board["timing"].object != nil || reading?.board["reception"].object != nil
                do { try await Task.sleep(for: .seconds(fast ? 2 : 30)) } catch { return }
            }
        }
        .onDisappear { run = UUID(); reading = nil; reference = nil; received = nil }
    }
}

private struct PortalFacts: View {
    let rows: [(String, String)]
    var body: some View {
        ForEach(Array(rows.enumerated()), id: \.offset) { _, row in
            LabeledContent(row.0, value: row.1.isEmpty ? StationFormat.unknown : row.1).font(.callout).textSelection(.enabled)
        }
    }
}

private struct PortalHistoryView: View {
    @Environment(OwnerPortalController.self) private var portal
    let fleet: PortalFleet, station: PortalStation
    @State private var metric: SensorMetric = .temperature
    @State private var hours = 1
    @State private var history: PortalHistory?
    @State private var points: [PortalHistory.Point] = []
    @State private var message: String?
    @State private var loading = false
    @State private var operation: Task<Void, Never>?
    @State private var run = UUID()
    var body: some View {
        InstrumentCard("history.title", systemImage: "chart.xyaxis.line") {
            Picker("history.measurement", selection: $metric) { ForEach(SensorMetric.allCases) { Text($0.title).tag($0) } }
            Picker("history.window", selection: $hours) { Text("history.hour").tag(1); Text("history.six_hours").tag(6); Text("history.day").tag(24); Text("history.week").tag(168) }
            Button("action.refresh") { operation?.cancel(); operation = Task { await load() } }.disabled(loading)
            if loading { ProgressView() }
            if let message { Text(message).foregroundStyle(StationPalette.warning) }
            if let history {
                let chart = PortalHistory(points: points, metric: history.metric, label: history.label, unit: history.unit,
                                          accessRevision: history.accessRevision, page: history.page).chartPoints(metric: metric)
                Chart(chart) { point in
                    LineMark(x: .value("UTC", point.time), y: .value(metric.unit, point.value), series: .value("Segment", point.segment))
                        .foregroundStyle(StationPalette.accent)
                    PointMark(x: .value("UTC", point.time), y: .value(metric.unit, point.value)).symbolSize(8).foregroundStyle(StationPalette.accent)
                }
                .chartXAxis { AxisMarks { value in
                    AxisGridLine(); AxisTick()
                    AxisValueLabel { if let date = value.as(Date.self) { Text(String(date.ISO8601Format().dropFirst(hours >= 24 ? 5 : 11).prefix(hours >= 24 ? 11 : 5))) } }
                }}
                .frame(height: 220)
                Text("history.note").font(.caption).foregroundStyle(.secondary)
                if metric == .phase { Text("history.phase_note").font(.caption) }
                if points.isEmpty { Text("history.empty") }
                if history.page.historyLimited { Text(String(format: String(localized: "history.policy"), BoardFormat.timestamp(history.page.visibleSince))).font(.caption) }
                if history.page.hasMore && points.count < 5000 && history.page.nextOffset != nil {
                    Button("history.more") { operation?.cancel(); operation = Task { await load(append: true) } }.disabled(loading)
                }
                if points.count >= 5000 || history.page.paginationLimited { Text("history.narrow").font(.caption) }
                DisclosureGroup("history.samples") {
                    ScrollView { LazyVStack(alignment: .leading) {
                        ForEach(Array(points.enumerated()), id: \.offset) { _, sample in
                            VStack(alignment: .leading) {
                                Text(sample.time).font(.caption.monospaced())
                                Text(BoardFormat.measurement(sample.value, unit: metric.unit))
                                Text(String(format: String(localized: "history.source"), sample.sampleTime ?? StationFormat.unknown))
                                Text(String(format: String(localized: "history.session"), sample.session ?? StationFormat.unknown))
                                Text(BoardFormat.hardwareTrust(sample.hardwareTrust))
                            }.font(.caption).textSelection(.enabled)
                            Divider()
                        }
                    }}.frame(maxHeight: 280)
                }
            }
        }
        .task(id: "\(metric.rawValue):\(hours)") { operation?.cancel(); await load() }
        .onDisappear { operation?.cancel(); run = UUID(); points = []; history = nil }
    }
    @MainActor private func load(append: Bool = false) async {
        guard let connection = portal.connection else { return }
        let epoch = portal.epoch, id = UUID(); run = id; loading = true; message = nil
        defer { if run == id { loading = false } }
        do {
            var query = [URLQueryItem(name: "metric", value: metric.rawValue), URLQueryItem(name: "hours", value: String(hours))]
            if append, let history, let offset = history.page.nextOffset {
                query += [URLQueryItem(name: "since", value: history.page.since), URLQueryItem(name: "until", value: history.page.until),
                          URLQueryItem(name: "offset", value: String(offset)), URLQueryItem(name: "revision", value: history.page.revision)]
            } else { history = nil; points = [] }
            let response = try await portal.client.get(PortalHistory.self, connection: connection, path: fleet.path + station.path + "history/", query: query)
            try Task.checkCancellation()
            guard run == id, portal.connection == connection, portal.epoch == epoch else { return }
            guard response.metric == metric.rawValue, response.points.count <= 500,
                  !append || (response.accessRevision == history?.accessRevision && response.page.revision == history?.page.revision && response.page.since == history?.page.since && response.page.until == history?.page.until),
                  response.page.nextOffset == nil || response.page.nextOffset == (append ? points.count : 0) + response.points.count
            else { throw FeedError.invalidResponse }
            history = response; points = append ? points + response.points : response.points
        } catch is CancellationError {} catch {
            guard run == id else { return }; history = nil; points = []; message = PortalErrorMessage.describe(error); await portal.failed(error)
        }
    }
}
