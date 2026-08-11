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
    private(set) var authorizationLost = false
    private(set) var activeSession: ReadSession?

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

    func start(session: ReadSession, stationIDs: [String]) {
        stop()
        resetPresentation()
        activeSession = session
        selectedStationIDs = stationIDs

        pollTask = Task { [weak self] in
            guard let self else { return }
            await restoreCache(session: session)
            guard activeSession == session, !Task.isCancelled else { return }
            streamTask = Task { [weak self] in
                await self?.runEventStream(session: session)
            }
            while !Task.isCancelled {
                await refresh(session: session)
                do {
                    // docs/OUTPUT.md §5 gives observers a 30-second cache cadence.
                    try await Task.sleep(for: .seconds(30))
                } catch { return }
            }
        }
    }

    func stop() {
        pollTask?.cancel()
        streamTask?.cancel()
        pollTask = nil
        streamTask = nil
        isEventStreamConnected = false
    }

    func disconnect(clearCachedScope: Bool) async {
        let key = activeSession?.cacheKey
        stop()
        activeSession = nil
        resetPresentation()
        if clearCachedScope, let key {
            try? await cache.clear(key)
        }
    }

    func clearPrivateCaches(forServer server: String, principal: String? = nil) async {
        try? await cache.clearPrivate(forServer: server, principal: principal)
    }

    func refresh() async {
        guard let activeSession else { return }
        await refresh(session: activeSession)
    }

    private func refresh(session: ReadSession) async {
        guard activeSession == session, !isRefreshing else { return }
        isRefreshing = true
        defer { isRefreshing = false }

        do {
            try await validateAuthorization(session: session)
            let envelope = try await feedClient.fetchObservers(session: session)
            guard let payload = envelope.data else { throw FeedError.missingData }
            guard payload.schema == "2.0",
                  payload.audience == session.audience.rawValue
            else { throw FeedError.invalidResponse }
            guard activeSession == session else { return }
            let snapshot = ObserversSnapshot(
                receivedAt: Date(),
                scope: session.cacheKey,
                serverTime: envelope.time,
                payload: payload
            )
            apply(snapshot, cached: false)
            try? await cache.saveObservers(snapshot, for: session.cacheKey)
            errorMessage = nil
            authorizationLost = false
        } catch is CancellationError {
            return
        } catch {
            if await handleAuthorizationLoss(error, session: session) { return }
            errorMessage = error.localizedDescription
        }

        do {
            let envelope = try await feedClient.fetchEvents(session: session)
            guard let payload = envelope.data else { throw FeedError.missingData }
            guard payload.schema == "2.0",
                  payload.audience == session.audience.rawValue
            else { throw FeedError.invalidResponse }
            guard activeSession == session else { return }
            mergeEvents(payload.events ?? [])
            eventStreamMessage = nil
        } catch is CancellationError {
            return
        } catch {
            if await handleAuthorizationLoss(error, session: session) { return }
            // Event history can be unavailable while the live observers feed is
            // healthy; keep that failure separate from the main error banner.
            eventStreamMessage = error.localizedDescription
        }
    }

    private func restoreCache(session: ReadSession) async {
        guard observers.isEmpty else { return }
        do {
            lastEventID = try await cache.loadCursor(for: session.cacheKey)
            if let snapshot = try await cache.loadObservers(for: session.cacheKey),
               snapshot.scope == session.cacheKey,
               snapshot.payload.schema == "2.0",
               snapshot.payload.audience == session.audience.rawValue,
               activeSession == session {
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

    private func runEventStream(session: ReadSession) async {
        var retrySeconds = 1
        while !Task.isCancelled, activeSession == session {
            do {
                let updates = try eventStream.updates(session: session, lastEventID: lastEventID)
                for try await update in updates {
                    try Task.checkCancellation()
                    guard activeSession == session else { return }
                    switch update {
                    case .event(let event, let cursor):
                        await record(cursor: cursor, session: session)
                        applyLiveEvent(event)
                    case .resolved(let event, let cursor):
                        await record(cursor: cursor, session: session)
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
                if await handleAuthorizationLoss(error, session: session) { return }
                isEventStreamConnected = false
                eventStreamMessage = error.localizedDescription
            }

            do {
                try await Task.sleep(for: .seconds(retrySeconds))
            } catch { return }
            retrySeconds = min(retrySeconds * 2, 30)
        }
    }

    private func validateAuthorization(session: ReadSession) async throws {
        let envelope = try await feedClient.fetchAudiences(
            baseURL: session.baseURL,
            token: session.token
        )
        guard let discovery = envelope.data?.validated(),
              discovery.audiences.contains(session.audience)
        else { throw FeedError.audienceLost }
        guard discovery.authorizationRevision == session.authorizationRevision else {
            throw FeedError.audienceLost
        }
        if session.audience.isPrivate, discovery.principalID != session.principalID {
            throw FeedError.audienceLost
        }
    }

    private func record(cursor: String?, session: ReadSession) async {
        guard let cursor, activeSession == session else { return }
        lastEventID = cursor
        try? await cache.saveCursor(cursor, for: session.cacheKey)
    }

    private func handleAuthorizationLoss(_ error: Error, session: ReadSession) async -> Bool {
        guard activeSession == session,
              let feedError = error as? FeedError,
              feedError.isAuthorizationLoss
        else { return false }

        stop()
        activeSession = nil
        resetPresentation()
        authorizationLost = true
        errorMessage = feedError.localizedDescription
        eventStreamMessage = feedError.localizedDescription
        if session.audience.isPrivate {
            try? await cache.clearPrivate(
                forServer: session.baseURL.absoluteString,
                principal: session.principalID
            )
        } else {
            try? await cache.clear(session.cacheKey)
        }
        return true
    }

    private func resetPresentation() {
        observers = []
        events = []
        activeEvents = [:]
        ageAtFetch = [:]
        fetchedAt = nil
        lastEventID = nil
        lastUpdated = nil
        isShowingCachedSnapshot = false
        isEventStreamConnected = false
        errorMessage = nil
        eventStreamMessage = nil
        authorizationLost = false
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
