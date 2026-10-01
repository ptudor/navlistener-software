import Foundation

enum SensorMetric: String, CaseIterable, Identifiable, Sendable {
    case temperature, humidity, pressure, phase
    var id: String { rawValue }
    var kind: String { self == .phase ? "timing" : "environment" }
    var unit: String {
        switch self { case .temperature: "°C"; case .humidity: "%"; case .pressure: "Pa"; case .phase: "ns" }
    }
    var title: String {
        switch self {
        case .temperature: String(localized: "history.temperature")
        case .humidity: String(localized: "history.humidity")
        case .pressure: String(localized: "history.pressure")
        case .phase: String(localized: "history.phase")
        }
    }
    var gapSeconds: Double { self == .phase ? 5 : 660 }
    func value(in sample: SensorHistorySample) -> Double? {
        let value: Double? = switch self {
        case .temperature: sample.details?.environment?.mcp9808C
        case .humidity: sample.details?.environment?.humidityPercent
        case .pressure: sample.details?.environment?.pressurePa.map(Double.init)
        case .phase: sample.details?.timing?.rtcMinusGNSSPhaseNS
        }
        return value.flatMap { $0.isFinite ? $0 : nil }
    }
}

struct SensorHistorySample: Codable, Sendable {
    let receivedAt: String
    let sampleTime: String?
    let session: String?
    let sequence: String?
    let hardwareTrust: String?
    let details: BoardDetails?
    enum CodingKeys: String, CodingKey {
        case session, sequence, details
        case receivedAt = "received_at", sampleTime = "sample_time", hardwareTrust = "hardware_trust"
    }
}

struct SensorHistoryRequest: Equatable, Sendable {
    let observer: String
    let metric: SensorMetric
    let since: String
    let until: String
    var offset = 0
    var revision: String? = nil

    init(observer: String, metric: SensorMetric, hours: Int, now: Date) throws {
        guard !observer.isEmpty, !observer.contains("\0"), observer.utf8.count <= 4096,
              [1, 6, 24, 168].contains(hours), now.timeIntervalSince1970.isFinite
        else { throw FeedError.invalidResponse }
        self.observer = observer
        self.metric = metric
        since = now.addingTimeInterval(-Double(hours) * 3600).ISO8601Format(.init(includingFractionalSeconds: true))
        until = now.ISO8601Format(.init(includingFractionalSeconds: true))
    }

    private init(observer: String, metric: SensorMetric, page: SensorHistoryPage, offset: Int) {
        self.observer = observer; self.metric = metric
        since = page.since; until = page.until; self.offset = offset; revision = page.revision
    }

    func continuation(_ page: SensorHistoryPage) -> Self? {
        guard page.hasMore, let offset = page.nextOffset else { return nil }
        return Self(observer: observer, metric: metric, page: page, offset: offset)
    }

    var query: [URLQueryItem] {
        var result = [URLQueryItem(name: "observer", value: observer),
                      URLQueryItem(name: "kind", value: metric.kind),
                      URLQueryItem(name: "since", value: since), URLQueryItem(name: "until", value: until),
                      URLQueryItem(name: "limit", value: "500"), URLQueryItem(name: "offset", value: String(offset))]
        if let revision { result.append(URLQueryItem(name: "revision", value: revision)) }
        return result
    }
}

struct SensorHistoryPage: Codable, Sendable {
    let schema: String
    let audience: String
    let observer: String
    let kind: String
    let since: String
    let until: String
    let visibleSince: String
    let effectiveSince: String
    let historyLimited: Bool
    let revision: String
    let offset: Int
    let limit: Int
    let nextOffset: Int?
    let hasMore: Bool
    let paginationLimited: Bool
    let samples: [SensorHistorySample]
    enum CodingKeys: String, CodingKey {
        case schema, audience, observer, kind, since, until, revision, offset, limit, samples
        case visibleSince = "visible_since", effectiveSince = "effective_since", historyLimited = "history_limited"
        case nextOffset = "next_offset", hasMore = "has_more", paginationLimited = "pagination_limited"
    }

    func validate(request: SensorHistoryRequest, session: ReadSession) throws {
        guard schema == "2.0", audience == session.audience.rawValue, observer == request.observer,
              kind == request.metric.kind, offset == request.offset, limit == 500, samples.count <= limit,
              !revision.isEmpty, revision.utf8.count <= 256,
              request.revision == nil || request.revision == revision,
              let start = WireDate.parse(since), let end = WireDate.parse(until),
              let requestedEnd = WireDate.parse(request.until), start == WireDate.parse(request.since),
              start <= end, end <= requestedEnd,
              let visible = WireDate.parse(visibleSince), let effective = WireDate.parse(effectiveSince),
              effective == max(start, visible), historyLimited == (effective > start)
        else { throw FeedError.invalidResponse }
        if hasMore {
            guard !samples.isEmpty else { throw FeedError.invalidResponse }
            if paginationLimited {
                guard nextOffset == nil, offset + samples.count > 100_000 else { throw FeedError.invalidResponse }
            } else {
                guard nextOffset == offset + samples.count, (nextOffset ?? Int.max) <= 100_000
                else { throw FeedError.invalidResponse }
            }
        } else if nextOffset != nil || paginationLimited { throw FeedError.invalidResponse }
        var previous: Date?
        for sample in samples {
            guard let receipt = WireDate.parse(sample.receivedAt), receipt >= effective, receipt <= end,
                  previous == nil || receipt >= previous! else { throw FeedError.invalidResponse }
            if let sequence = sample.sequence {
                guard !sequence.isEmpty, sequence.utf8.allSatisfy({ (48...57).contains($0) }), UInt64(sequence) != nil
                else { throw FeedError.invalidResponse }
            }
            previous = receipt
        }
    }
}

struct SensorHistoryPoint: Identifiable, Sendable {
    let id: Int
    let time: Date
    let value: Double
    let segment: Int

    static func make(samples: [SensorHistorySample], metric: SensorMetric) -> [Self] {
        var result: [Self] = [], segment = 0
        var previous: SensorHistorySample?
        for (index, sample) in samples.enumerated() {
            guard let time = WireDate.parse(sample.receivedAt), let value = metric.value(in: sample) else {
                segment += 1; previous = nil; continue
            }
            if let previous, previous.session != sample.session ||
                time.timeIntervalSince(WireDate.parse(previous.receivedAt) ?? .distantPast) > metric.gapSeconds {
                segment += 1
            }
            result.append(Self(id: index, time: time, value: value, segment: segment))
            previous = sample
        }
        return result
    }
}
