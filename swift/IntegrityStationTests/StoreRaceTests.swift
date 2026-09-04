import Foundation
import Testing
@testable import IntegrityStation

private actor StoreNetworkState {
    var revoked = false
    var sentEvent = false
    var failPublicObservers = false
    var publicCursors: [String] = []
    func revoke() { revoked = true }
    func failPublic() { failPublicObservers = true }
    func response(_ request: URLRequest) throws -> Data {
        let privateRequest = request.value(forHTTPHeaderField: "Authorization") != nil
        if !privateRequest && failPublicObservers && request.url!.path.hasSuffix("/observers") { throw URLError(.notConnectedToInternet) }
        if request.url!.path.hasSuffix("/audiences") {
            return Data((privateRequest
                ? "{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"principal\":\"reader\",\"revision\":\"\(revoked ? "revoked" : "private-v1")\",\"audiences\":[\"public\",\"organization:org\"]}}"
                : #"{"ok":true,"data":{"schema":"2.0","revision":"public-v1","audiences":["public"]}}"#).utf8)
        }
        let audience = request.value(forHTTPHeaderField: "X-GNSS-Audience") ?? "public"
        if request.url!.path.hasSuffix("/gnss/events") {
            if !privateRequest, let cursor = request.value(forHTTPHeaderField: "Last-Event-ID") { publicCursors.append(cursor) }
            if privateRequest && !sentEvent {
                sentEvent = true
                return Data("id: 987\nevent: gnss\ndata: {\"id\":987,\"sv\":\"private-station\",\"type\":\"station_offline\",\"new_value\":\"offline\",\"severity\":2}\n\n".utf8)
            }
            return Data("event: status\ndata: {\"status\":\"connected\"}\n\n".utf8)
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
        loadingTask = Task {
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
    private func settle() async throws { try await Task.sleep(for: .milliseconds(60)) }

    @Test(arguments: ["public", "disconnect", "restart", "revoked"])
    func retiredCursorSaveCannotApplyAnOldEvent(transition: String) async throws {
        let gate = ConnectionBarrier()
        let directory = FileManager.default.temporaryDirectory.appending(path: "StoreRace.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory); StoreConnectionProtocol.handler = nil }
        let cache = SnapshotCache(directory: directory, beforeCursorSave: { await gate.holdOnce() })
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
        await gate.release(); try await settle()
        #expect(store.events.allSatisfy { $0.id != 987 })
        #expect(store.activeEvents(for: "private-station").isEmpty)
        #expect(try await cache.loadCursor(for: old.cacheKey) == nil)
        #expect(await network.publicCursors.isEmpty)
        await store.disconnect(clearCachedScope: true)
    }

    @Test func delayedObserverSaveCannotRecreateClearedPrivateCache() async throws {
        let gate = ConnectionBarrier()
        let directory = FileManager.default.temporaryDirectory.appending(path: "StoreRace.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory); StoreConnectionProtocol.handler = nil }
        let cache = SnapshotCache(directory: directory, beforeObserverSave: { await gate.holdOnce() })
        let network = StoreNetworkState(); let store = store(cache: cache, network: network)
        let old = try session(privateAudience: true)
        store.start(session: old, stationIDs: ["private-station"])
        await gate.waitUntilEntered()
        await store.disconnect(clearCachedScope: true)
        await network.failPublic()
        store.start(session: try session(privateAudience: false), stationIDs: ["public-station"])
        while store.errorMessage == nil { await Task.yield() }
        await gate.release(); try await settle()
        #expect(store.errorMessage != nil)
        #expect(store.observers.allSatisfy { $0.id != "private-station" })
        #expect(try await cache.loadObservers(for: old.cacheKey) == nil)
        #expect(!store.authorizationLost)
        await store.disconnect(clearCachedScope: true)
    }

    @Test func delayedCursorRestoreCannotChangePublicCursor() async throws {
        let gate = ConnectionBarrier()
        let directory = FileManager.default.temporaryDirectory.appending(path: "StoreRace.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory); StoreConnectionProtocol.handler = nil }
        let cache = SnapshotCache(directory: directory, beforeCursorLoad: { await gate.holdOnce() })
        let old = try session(privateAudience: true)
        try await cache.saveCursor("private-cursor", for: old.cacheKey)
        let network = StoreNetworkState(); let store = store(cache: cache, network: network)
        store.start(session: old, stationIDs: ["private-station"])
        await gate.waitUntilEntered()
        store.start(session: try session(privateAudience: false), stationIDs: ["public-station"])
        await gate.release(); try await settle()
        #expect(await network.publicCursors.isEmpty)
        #expect(store.observers.allSatisfy { $0.id != "private-station" })
        await store.disconnect(clearCachedScope: true)
    }
}
