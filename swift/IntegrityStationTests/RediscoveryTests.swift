import Foundation
import Testing
@testable import IntegrityStation

/// A collector whose discovery revision can move (restart, policy change) and
/// whose private grant can be withdrawn, independently.
private actor RediscoveryNetwork {
    private(set) var revision = "v1"
    private(set) var withdrawn = false
    private(set) var discoveryRequests = 0
    func setRevision(_ value: String) { revision = value }
    func withdraw() { withdrawn = true }

    func response(_ request: URLRequest) -> Data {
        let authenticated = request.value(forHTTPHeaderField: "Authorization") != nil
        let path = request.url!.path
        if path.hasSuffix("/audiences") {
            discoveryRequests += 1
            let grants = withdrawn ? "[\"public\"]" : "[\"public\",\"organization:org\"]"
            return Data((authenticated
                ? "{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"principal\":\"reader\",\"revision\":\"private-\(revision)\",\"audiences\":\(grants)}}"
                : "{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"revision\":\"public-\(revision)\",\"audiences\":[\"public\"]}}").utf8)
        }
        let audience = request.value(forHTTPHeaderField: "X-GNSS-Audience") ?? "public"
        if path.hasSuffix("/gnss/events") {
            return Data("event: status\ndata: {\"status\":\"connected\"}\n\n".utf8)
        }
        if path.hasSuffix("/conditions") {
            return Data("{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"audience\":\"\(audience)\",\"complete\":true,\"epoch\":\"a\",\"cursor\":1,\"events\":[]}}".utf8)
        }
        return Data("{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"audience\":\"\(audience)\",\"observers\":[{\"id\":\"roof\",\"last_seen_s\":1}],\"events\":[]}}".utf8)
    }
}

private final class RediscoveryProtocol: URLProtocol, @unchecked Sendable {
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

/// docs/OUTPUT.md §0.1: a changed discovery revision erases the partition and
/// requires re-discovery. The app must do that by itself; only a withdrawn
/// grant or a rejected credential may end in the dead authorization-lost state.
@Suite(.serialized) @MainActor
struct RediscoveryTests {
    private func fixture(network: RediscoveryNetwork) throws -> (app: AppController, cache: SnapshotCache, directory: URL, suite: String) {
        RediscoveryProtocol.handler = { await network.response($0) }
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [RediscoveryProtocol.self]
        let session = URLSession(configuration: config)
        let suite = "RediscoveryTests.\(UUID())"
        let defaults = try #require(UserDefaults(suiteName: suite))
        let directory = FileManager.default.temporaryDirectory.appending(path: suite)
        let cache = SnapshotCache(directory: directory)
        let store = StationStore(feedClient: FeedClient(session: session), eventStream: EventStream(session: session), cache: cache)
        let app = AppController(store: store, settings: AppSettings(defaults: defaults),
                                secureStore: RaceCredentials(), feedClient: FeedClient(session: session))
        return (app, cache, directory, suite)
    }

    private func cleanUp(_ directory: URL, _ suite: String) {
        RediscoveryProtocol.handler = nil
        try? FileManager.default.removeItem(at: directory)
        UserDefaults(suiteName: suite)?.removePersistentDomain(forName: suite)
    }

    @discardableResult
    private func settle(until condition: @MainActor () async throws -> Bool) async throws -> Bool {
        for _ in 0..<500 {
            if try await condition() { return true }
            try await Task.sleep(for: .milliseconds(10))
        }
        return try await condition()
    }

    @Test(arguments: [true, false])
    func movedRevisionRediscoversWithoutLosingAuthorization(privateAudience: Bool) async throws {
        let network = RediscoveryNetwork()
        let (app, cache, directory, suite) = try fixture(network: network)
        defer { cleanUp(directory, suite) }
        try await app.connect(to: "https://rediscover.invalid", readToken: privateAudience ? "token" : nil)
        let old = try #require(app.store.activeSession)
        let prefix = privateAudience ? "private-" : "public-"
        #expect(old.authorizationRevision == prefix + "v1")
        // The first poll writes the old partition before the revision moves.
        #expect(try await settle { try await cache.loadObservers(for: old.cacheKey) != nil })
        #expect(try await settle { !app.store.isRefreshing })

        await network.setRevision("v2")
        await app.refresh()

        // Within one poll: a fresh session under the new revision, the same
        // audience and credential, nothing lost, and the old partition gone.
        #expect(try await settle { app.store.activeSession?.authorizationRevision == prefix + "v2" })
        let renewed = try #require(app.store.activeSession)
        #expect(renewed.audience == old.audience)
        #expect(renewed.principalID == old.principalID)
        #expect(renewed.token == old.token)
        #expect(renewed.baseURL == old.baseURL)
        #expect(!app.store.authorizationLost)
        #expect(app.connectionError == nil)
        #expect(app.authorizationRevision == renewed.authorizationRevision)
        #expect(app.selectedAudience == old.audience)
        #expect(try await cache.loadObservers(for: old.cacheKey) == nil)
        #expect(try await cache.loadCursor(for: old.cacheKey) == nil)
        #expect(try await settle { !app.store.observers.isEmpty })
        #expect(app.store.observers.map(\.id) == ["roof"])
        await app.store.disconnect(clearCachedScope: true)
    }

    @Test func withdrawnGrantStillEndsInAuthorizationLoss() async throws {
        let network = RediscoveryNetwork()
        let (app, cache, directory, suite) = try fixture(network: network)
        defer { cleanUp(directory, suite) }
        try await app.connect(to: "https://rediscover.invalid", readToken: "token")
        let old = try #require(app.store.activeSession)
        #expect(old.audience.isPrivate)
        #expect(try await settle { try await cache.loadObservers(for: old.cacheKey) != nil })
        #expect(try await settle { !app.store.isRefreshing })

        await network.setRevision("v2")
        await network.withdraw()
        let discoveries = await network.discoveryRequests
        await app.refresh()

        #expect(app.store.authorizationLost)
        #expect(app.store.activeSession == nil)
        #expect(app.store.errorMessage != nil)
        #expect(try await cache.loadObservers(for: old.cacheKey) == nil)
        // No automatic re-discovery follows a withdrawn grant: that is the
        // user's decision (sign in again or choose another audience).
        try await Task.sleep(for: .milliseconds(150))
        #expect(app.store.activeSession == nil)
        #expect(app.store.authorizationLost)
        #expect(await network.discoveryRequests == discoveries + 1)
        await app.store.disconnect(clearCachedScope: true)
    }
}
