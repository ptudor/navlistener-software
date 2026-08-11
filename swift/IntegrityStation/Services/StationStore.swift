import Foundation
import Observation

@MainActor
@Observable
final class StationStore {
    private(set) var observers: [Observer] = []
    private(set) var events: [GNSSAPIEvent] = []
    private(set) var errorMessage: String?
    private(set) var eventStreamMessage: String?
    private(set) var isRefreshing = false
    private(set) var isEventStreamConnected = false
    private(set) var lastUpdated: Date?
    private(set) var isShowingCachedSnapshot = false

    var selectedStationIDs: [String] = []

    private let feedClient: FeedClient
    private let eventStream: EventStream
    private let cache: SnapshotCache
    private var pollTask: Task<Void, Never>?
    private var streamTask: Task<Void, Never>?
    private var ageAtFetch: [String: TimeInterval] = [:]
    private var fetchedAt: ContinuousClock.Instant?
    private var activeEvents: [String: GNSSAPIEvent] = [:]
    private var lastEventID: String?

    init(
        feedClient: FeedClient = FeedClient(),
        eventStream: EventStream = EventStream(),
        cache: SnapshotCache = SnapshotCache()
    ) {
        self.feedClient = feedClient
        self.eventStream = eventStream
        self.cache = cache
    }

    var selectedObservers: [Observer] {
        let selected = Set(selectedStationIDs)
        return observers.filter { selected.contains($0.id) }
    }

    func activeEvents(for stationID: String) -> [GNSSAPIEvent] {
        activeEvents.values.filter { $0.stationID == stationID }
    }

    var rollupHealth: HealthState {
        HealthState.rollup(selectedStationIDs.map(health(for:)))
    }

    func health(for stationID: String) -> HealthState {
        let severities = activeEvents.values.compactMap { event -> EventSeverity? in
            guard event.stationID == stationID else { return nil }
            return event.severity
        }
        return HealthState.station(
            lastSeenSeconds: currentLastSeenAge(for: stationID),
            activeEventSeverities: severities
        )
    }

    func currentLastSeenAge(for stationID: String) -> TimeInterval? {
        guard let base = ageAtFetch[stationID] else { return nil }
        guard let fetchedAt else { return base }
        return base + fetchedAt.duration(to: .now).timeInterval
    }

    func start(baseURL: URL, stationIDs: [String]) {
        stop()
        selectedStationIDs = stationIDs

        pollTask = Task { [weak self] in
            guard let self else { return }
            await restoreCache(baseURL: baseURL)
            while !Task.isCancelled {
                await refresh(baseURL: baseURL)
                do {
                    // docs/OUTPUT.md §5 gives observers a 30-second cache cadence.
                    try await Task.sleep(for: .seconds(30))
                } catch { return }
            }
        }

        streamTask = Task { [weak self] in
            await self?.runEventStream(baseURL: baseURL)
        }
    }

    func stop() {
        pollTask?.cancel()
        streamTask?.cancel()
        pollTask = nil
        streamTask = nil
        isEventStreamConnected = false
    }

    func refresh(baseURL: URL) async {
        guard !isRefreshing else { return }
        isRefreshing = true
        defer { isRefreshing = false }

        do {
            let envelope = try await feedClient.fetchObservers(baseURL: baseURL)
            guard let payload = envelope.data else { throw FeedError.missingData }
            let snapshot = ObserversSnapshot(
                receivedAt: Date(),
                serverBaseURL: baseURL.absoluteString,
                serverTime: envelope.time,
                payload: payload
            )
            apply(snapshot, cached: false)
            try? await cache.saveObservers(snapshot)
            errorMessage = nil
        } catch is CancellationError {
            return
        } catch {
            errorMessage = error.localizedDescription
        }

        do {
            let envelope = try await feedClient.fetchEvents(baseURL: baseURL)
            mergeEvents(envelope.data?.events ?? [])
            eventStreamMessage = nil
        } catch is CancellationError {
            return
        } catch {
            // Event history can be unavailable while the live observers feed is
            // healthy; keep that failure separate from the main error banner.
            eventStreamMessage = error.localizedDescription
        }
    }

    private func restoreCache(baseURL: URL) async {
        guard observers.isEmpty else { return }
        do {
            if let snapshot = try await cache.loadObservers(),
               snapshot.serverBaseURL == baseURL.absoluteString {
                apply(snapshot, cached: true)
            }
        } catch {
            // A corrupt cache is not data. The first live fetch will replace it.
        }
    }

    private func apply(_ snapshot: ObserversSnapshot, cached: Bool) {
        observers = snapshot.payload.observers ?? []
        lastUpdated = snapshot.receivedAt
        isShowingCachedSnapshot = cached
        fetchedAt = .now

        let servedAt = WireDate.parse(snapshot.serverTime) ?? snapshot.receivedAt
        ageAtFetch = Dictionary(uniqueKeysWithValues: observers.compactMap { observer in
            if let age = observer.lastSeenS {
                return (observer.id, max(0, age))
            }
            if let epoch = observer.lastSeen {
                return (observer.id, max(0, servedAt.timeIntervalSince1970 - epoch))
            }
            if let epoch = observer.rf?.lastSeen {
                return (observer.id, max(0, servedAt.timeIntervalSince1970 - epoch))
            }
            return nil
        })
    }

    private func runEventStream(baseURL: URL) async {
        var retrySeconds = 1
        while !Task.isCancelled {
            do {
                let updates = try eventStream.updates(baseURL: baseURL, lastEventID: lastEventID)
                for try await update in updates {
                    try Task.checkCancellation()
                    switch update {
                    case .event(let event, let cursor):
                        if let cursor { lastEventID = cursor }
                        applyLiveEvent(event)
                    case .resolved(let event, let cursor):
                        if let cursor { lastEventID = cursor }
                        resolve(event)
                    case .status:
                        isEventStreamConnected = true
                        eventStreamMessage = nil
                    }
                    retrySeconds = 1
                }
            } catch is CancellationError {
                return
            } catch {
                isEventStreamConnected = false
                eventStreamMessage = error.localizedDescription
            }

            do {
                try await Task.sleep(for: .seconds(retrySeconds))
            } catch { return }
            retrySeconds = min(retrySeconds * 2, 30)
        }
    }

    private func mergeEvents(_ incoming: [GNSSAPIEvent]) {
        // Query results are newest-first. Replaying oldest-first reconstructs
        // the latest server-classified condition state without client thresholds.
        for event in incoming.reversed() {
            updateActiveCondition(with: event)
        }

        var byID = Dictionary(uniqueKeysWithValues: events.compactMap { event in
            event.id.map { ($0, event) }
        })
        for event in incoming {
            if let id = event.id { byID[id] = event }
        }
        events = byID.values.sorted(by: Self.isNewer).prefix(200).map { $0 }
    }

    private func applyLiveEvent(_ event: GNSSAPIEvent) {
        updateActiveCondition(with: event)
        if let id = event.id, events.contains(where: { $0.id == id }) { return }
        events.insert(event, at: 0)
        if events.count > 200 { events.removeLast(events.count - 200) }
    }

    private func updateActiveCondition(with event: GNSSAPIEvent) {
        guard let key = event.conditionKey,
              let isActive = event.isActiveStationCondition
        else { return }
        if isActive { activeEvents[key] = event }
        else { activeEvents.removeValue(forKey: key) }
    }

    private func resolve(_ event: GNSSAPIEvent) {
        if let key = event.conditionKey {
            activeEvents.removeValue(forKey: key)
        } else if let id = event.id {
            activeEvents = activeEvents.filter { $0.value.id != id }
        }
    }

    private static func isNewer(_ lhs: GNSSAPIEvent, _ rhs: GNSSAPIEvent) -> Bool {
        if let lhsID = lhs.id, let rhsID = rhs.id, lhsID != rhsID { return lhsID > rhsID }
        return (WireDate.parse(lhs.time) ?? .distantPast) > (WireDate.parse(rhs.time) ?? .distantPast)
    }
}

private extension Duration {
    var timeInterval: TimeInterval {
        let components = self.components
        return TimeInterval(components.seconds) + TimeInterval(components.attoseconds) / 1e18
    }
}
