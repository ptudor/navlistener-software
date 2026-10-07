import Foundation
import Testing
@testable import IntegrityStation

private actor StoreNetworkState {
    var revoked = false
    var sentEvent = false
    var failPublicObservers = false
    var publicCursors: [String] = []
    var conditionValue: String?
    var conditionRequests = 0
    var observerRows: String?
    func setObserverRows(_ rows: String?) { observerRows = rows }
    func setCondition(_ value: String) { conditionValue = value }

    func revoke() { revoked = true }
    func failPublic() { failPublicObservers = true }
    func response(_ request: URLRequest) throws -> Data {
        let privateRequest = request.value(forHTTPHeaderField: "Authorization") != nil
        if !privateRequest && failPublicObservers && request.url!.path.hasSuffix("/observers") { throw URLError(.notConnectedToInternet) }
        if request.url!.path.hasSuffix("/audiences") {
            // Revocation drops the audience from the new discovery; a revision
            // that merely moved would re-discover instead of signing out.
            return Data((privateRequest
                ? "{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"principal\":\"reader\",\"revision\":\"\(revoked ? "revoked" : "private-v1")\",\"audiences\":[\"public\"\(revoked ? "" : ",\"organization:org\"")]}}"
                : #"{"ok":true,"data":{"schema":"2.0","revision":"public-v1","audiences":["public"]}}"#).utf8)
        }
        let audience = request.value(forHTTPHeaderField: "X-GNSS-Audience") ?? "public"
        if request.url!.path.hasSuffix("/observers"), let observerRows {
            return Data("{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"audience\":\"\(audience)\",\"observers\":\(observerRows)}}".utf8)
        }
        if request.url!.path.hasSuffix("/conditions") {
            conditionRequests += 1
            let entries = conditionValue.map { "[{\"id\":\($0 == "ok" ? 300 : 1),\"time\":\"2026-08-01T00:00:00Z\",\"sv\":\"public-station\",\"type\":\"jamming_detected\",\"new_value\":\"\($0)\",\"severity\":1}]" } ?? "[]"
            return Data("{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"audience\":\"\(audience)\",\"complete\":true,\"epoch\":\"a\",\"cursor\":300,\"events\":\(entries)}}".utf8)
        }
        if request.url!.path.hasSuffix("/gnss/events") {
            if !privateRequest, let cursor = request.value(forHTTPHeaderField: "Last-Event-ID") { publicCursors.append(cursor) }
            if privateRequest && !sentEvent {
                sentEvent = true
                return Data("id: 987\nevent: gnss\ndata: {\"id\":987,\"sv\":\"private-station\",\"type\":\"station_offline\",\"new_value\":\"offline\",\"severity\":2}\n\n".utf8)
            }
            return Data("event: status\ndata: {\"status\":\"replay_gap\"}\n\nevent: status\ndata: {\"status\":\"connected\"}\n\n".utf8)
        }
        return Data("{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"audience\":\"\(audience)\",\"observers\":[{\"id\":\"\(privateRequest ? "private-station" : "public-station")\",\"last_seen_s\":1}],\"events\":[]}}".utf8)
    }
}

private final class StoreConnectionProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest) async throws -> Data)?
    private var loadingTask: Task<Void, Never>?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        loadingTask = Task { @Sendable [self] in
            do {
                guard let handler = Self.handler else { throw URLError(.badServerResponse) }
                let data = try await handler(request)
                client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: nil)!, cacheStoragePolicy: .notAllowed)
                client?.urlProtocol(self, didLoad: data)
                client?.urlProtocolDidFinishLoading(self)
            } catch { client?.urlProtocol(self, didFailWithError: error) }
        }
    }
    override func stopLoading() { loadingTask?.cancel() }
}

@Suite(.serialized) @MainActor
struct StoreRaceTests {
    private func session(privateAudience: Bool) throws -> ReadSession {
        try #require(ReadSession(baseURL: URL(string: "https://store.invalid")!, principalID: privateAudience ? "reader" : nil, audience: privateAudience ? ReadAudience("organization:org")! : .publicAudience, authorizationRevision: privateAudience ? "private-v1" : "public-v1", token: privateAudience ? "token" : nil))
    }
    private func store(cache: SnapshotCache, network: StoreNetworkState) -> StationStore {
        StoreConnectionProtocol.handler = { try await network.response($0) }
        let config = URLSessionConfiguration.ephemeral; config.protocolClasses = [StoreConnectionProtocol.self]
        let networkSession = URLSession(configuration: config)
        return StationStore(feedClient: FeedClient(session: networkSession), eventStream: EventStream(session: networkSession), cache: cache)
    }
    /// A held cache operation was released and has run to completion, so
    /// what it could or could not write is final.
    private func completed(_ counter: CompletionCounter, atLeast count: Int = 1) async throws {
        #expect(try await eventually { await counter.value >= count })
    }

    @Test func stationAddRetainsLabelsAndRejectsRetiredAudience() async throws {
        let directory = FileManager.default.temporaryDirectory.appending(path: "AddScope.\(UUID())")
        let suite = "AddScope.\(UUID())"
        let defaults = try #require(UserDefaults(suiteName: suite))
        defer { try? FileManager.default.removeItem(at: directory); StoreConnectionProtocol.handler = nil; defaults.removePersistentDomain(forName: suite) }
        let network = StoreNetworkState()
        let store = store(cache: SnapshotCache(directory: directory), network: network)
        let settings = AppSettings(defaults: defaults)
        let controller = AppController(store: store, settings: settings)
        let old = try session(privateAudience: true)
        settings.activateScope(old.cacheKey);store.start(session: old, stationIDs: [])
        try controller.addStation(id: "roof_1", for: old)
        controller.setLabel("My roof", for: "roof_1")
        try controller.addStation(id: "roof:1", for: old)
        try controller.addStation(id: "manual:third", for: old)
        try controller.addStation(id: "roof_1", for: old)
        #expect(settings.stationIDs == ["roof_1", "roof:1", "manual:third"])
        #expect(settings.label(for: "roof_1") == "My roof")
        controller.removeStation(id: "roof:1")
        try controller.addStation(id: "roof:1", for: old)
        #expect(settings.stationIDs == ["roof_1", "manual:third", "roof:1"])
        let next = try session(privateAudience: false)
        settings.activateScope(next.cacheKey);store.start(session: next, stationIDs: [])
        #expect(throws: FeedError.audienceLost) { try controller.addStation(id: "old-sheet", for: old) }
        #expect(settings.stationIDs.isEmpty)
        // Empty discovery still permits an exact manual ID in the current scope.
        try controller.addStation(id: "manual:new", for: next)
        #expect(settings.stationIDs == ["manual:new"])
        settings.activateScope(old.cacheKey)
        #expect(settings.label(for: "roof_1") == "My roof")
        await store.disconnect(clearCachedScope: true)
    }

    @Test func duplicateNetworkSnapshotPreservesValidDisplayAndCache() async throws {
        let directory = FileManager.default.temporaryDirectory.appending(path: "DuplicateNetwork.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory); StoreConnectionProtocol.handler = nil }
        let cache = SnapshotCache(directory: directory)
        let network = StoreNetworkState()
        let session = try session(privateAudience: false)
        let store = store(cache: cache, network: network)
        store.start(session: session, stationIDs: ["public-station"])
        for _ in 0..<100 where store.observers.isEmpty || store.isRefreshing { try await Task.sleep(for: .milliseconds(10)) }
        for rows in [#"[{"id":"duplicate","last_seen_s":1},{"id":"duplicate","last_seen_s":2}]"#, #"[{"id":"duplicate"},{"id":"duplicate"}]"#] {
            await network.setObserverRows(rows)
            await store.refresh()
            #expect(store.errorMessage != nil)
            #expect(store.observers.map(\.id) == ["public-station"])
            #expect(store.currentLastSeenAge(for: "duplicate") == nil)
            #expect(try await cache.loadObservers(for: session.cacheKey)?.payload.observers?.map(\.id) == ["public-station"])
        }
        await network.setObserverRows(nil)
        await store.refresh()
        #expect(store.errorMessage == nil)
        await store.disconnect(clearCachedScope: true)
    }

    @Test func restartingAndReplayGapReconcileOldConditions() async throws {
        let directory = FileManager.default.temporaryDirectory.appending(path: "ConditionRestart.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory); StoreConnectionProtocol.handler = nil }
        let cache = SnapshotCache(directory: directory)
        let network = StoreNetworkState()
        await network.setCondition("jammed")
        let session = try session(privateAudience: false)
        try await cache.saveCursor("2", for: session.cacheKey)
        for _ in 0..<2 {
            let store = store(cache: cache, network: network)
            store.start(session: session, stationIDs: ["public-station"])
            for _ in 0..<100 where store.activeEvents(for: "public-station").isEmpty { try await Task.sleep(for: .milliseconds(10)) }
            #expect(store.activeEvents(for: "public-station").first?.id == 1)
            await store.disconnect(clearCachedScope: false)
        }
        await network.setCondition("ok")
        let store = store(cache: cache, network: network)
        store.start(session: session, stationIDs: ["public-station"])
        await store.refresh()
        // The explicit refresh may yield to the poll's own refresh already in
        // flight; either one reconciles against the resolved condition.
        #expect(try await eventually {
            let requests = await network.conditionRequests
            return store.activeEvents(for: "public-station").isEmpty && requests >= 3
        })
        #expect(store.activeEvents(for: "public-station").isEmpty)
        #expect(await network.conditionRequests >= 3)
        await store.disconnect(clearCachedScope: true)
    }

    @Test(arguments: ["public", "disconnect", "restart", "revoked"])
    func retiredCursorSaveCannotApplyAnOldEvent(transition: String) async throws {
        let gate = ConnectionBarrier(), saves = CompletionCounter()
        let directory = FileManager.default.temporaryDirectory.appending(path: "StoreRace.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory); StoreConnectionProtocol.handler = nil }
        let cache = SnapshotCache(directory: directory, beforeCursorSave: { await gate.holdOnce() },
                                  afterCursorSave: { await saves.increment() })
        let network = StoreNetworkState(); let store = store(cache: cache, network: network)
        let old = try session(privateAudience: true)
        store.start(session: old, stationIDs: ["private-station"])
        await gate.waitUntilEntered()
        while store.isRefreshing { await Task.yield() }
        switch transition {
        case "public": store.start(session: try session(privateAudience: false), stationIDs: ["public-station"])
        case "restart": store.start(session: old, stationIDs: ["private-station"])
        case "revoked": await network.revoke(); await store.refresh(); #expect(store.authorizationLost)
        default: await store.disconnect(clearCachedScope: true)
        }
        await gate.release(); try await completed(saves)
        #expect(store.events.allSatisfy { $0.id != 987 })
        #expect(store.activeEvents(for: "private-station").isEmpty)
        #expect(try await cache.loadCursor(for: old.cacheKey) == nil)
        #expect(await network.publicCursors.isEmpty)
        await store.disconnect(clearCachedScope: true)
    }

    @Test func delayedObserverSaveCannotRecreateClearedPrivateCache() async throws {
        let gate = ConnectionBarrier(), saves = CompletionCounter()
        let directory = FileManager.default.temporaryDirectory.appending(path: "StoreRace.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory); StoreConnectionProtocol.handler = nil }
        let cache = SnapshotCache(directory: directory, beforeObserverSave: { await gate.holdOnce() },
                                  afterObserverSave: { await saves.increment() })
        let network = StoreNetworkState(); let store = store(cache: cache, network: network)
        let old = try session(privateAudience: true)
        store.start(session: old, stationIDs: ["private-station"])
        await gate.waitUntilEntered()
        await store.disconnect(clearCachedScope: true)
        await network.failPublic()
        store.start(session: try session(privateAudience: false), stationIDs: ["public-station"])
        while store.errorMessage == nil { await Task.yield() }
        await gate.release(); try await completed(saves)
        #expect(store.errorMessage != nil)
        #expect(store.observers.allSatisfy { $0.id != "private-station" })
        #expect(try await cache.loadObservers(for: old.cacheKey) == nil)
        #expect(!store.authorizationLost)
        await store.disconnect(clearCachedScope: true)
    }

    @Test func delayedCursorRestoreCannotChangePublicCursor() async throws {
        let gate = ConnectionBarrier(), loads = CompletionCounter()
        let directory = FileManager.default.temporaryDirectory.appending(path: "StoreRace.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory); StoreConnectionProtocol.handler = nil }
        let cache = SnapshotCache(directory: directory, beforeCursorLoad: { await gate.holdOnce() },
                                  afterCursorLoad: { await loads.increment() })
        let old = try session(privateAudience: true)
        try await cache.saveCursor("private-cursor", for: old.cacheKey)
        let network = StoreNetworkState(); let store = store(cache: cache, network: network)
        store.start(session: old, stationIDs: ["private-station"])
        await gate.waitUntilEntered()
        store.start(session: try session(privateAudience: false), stationIDs: ["public-station"])
        // Both the held private restore and the public session's restore load
        // a cursor; wait for the two of them.
        await gate.release(); try await completed(loads, atLeast: 2)
        #expect(await network.publicCursors.isEmpty)
        #expect(store.observers.allSatisfy { $0.id != "private-station" })
        await store.disconnect(clearCachedScope: true)
    }
}
