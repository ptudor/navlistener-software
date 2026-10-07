import Foundation
import Testing
@testable import IntegrityStation

/// Answers feed requests the way an HTTP cache in front of the collector would:
/// every document is served with `Cache-Control: public, max-age=30` and kept,
/// and a later request whose policy permits a cached answer is served from
/// that copy without counting as a network request. A request that reloads
/// ignoring the cache always reaches the "origin".
private final class CachingFeedProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var origin: [URLRequest] = []
    nonisolated(unsafe) private static var stored: [URL: (HTTPURLResponse, Data)] = [:]
    nonisolated(unsafe) private static var ageHeader: String?

    static var originRequests: [URLRequest] { lock.withLock { origin } }
    static func reset(age: String? = nil) {
        lock.withLock { origin = []; stored = [:]; ageHeader = age }
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let url = request.url!
        if request.cachePolicy == .useProtocolCachePolicy,
           let (response, data) = Self.lock.withLock({ Self.stored[url] }) {
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
            return
        }
        let age = Self.lock.withLock { Self.origin.append(request); return Self.ageHeader }
        var headers = ["Content-Type": "application/json", "Cache-Control": "public, max-age=30"]
        if let age { headers["Age"] = age }
        let response = HTTPURLResponse(url: url, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: headers)!
        let data = Self.document(for: request)
        Self.lock.withLock { Self.stored[url] = (response, data) }
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: data)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}

    private static func document(for request: URLRequest) -> Data {
        let path = request.url!.path
        if path.hasSuffix("/audiences") {
            return Data(#"{"ok":true,"data":{"schema":"2.0","revision":"public-v1","audiences":["public"]}}"#.utf8)
        }
        if path.hasSuffix("/conditions") {
            return Data(#"{"ok":true,"data":{"schema":"2.0","audience":"public","complete":true,"epoch":"a","cursor":1,"events":[]}}"#.utf8)
        }
        if path.hasSuffix("/gnss/events") {
            return Data("event: status\ndata: {\"status\":\"connected\"}\n\n".utf8)
        }
        if path.hasSuffix("/observers") {
            return Data(#"{"ok":true,"time":"2026-10-07T12:00:00Z","data":{"schema":"2.0","audience":"public","observers":[{"id":"roof","last_seen_s":1}]}}"#.utf8)
        }
        return Data(#"{"ok":true,"data":{"schema":"2.0","audience":"public","events":[]}}"#.utf8)
    }
}

private let ageHeaderCases: [(String?, TimeInterval)] = [
    (nil, 0), ("20", 20), (" 7 ", 7), ("0", 0), ("-5", 0), ("abc", 0), ("1.5", 0),
    ("3000000000", 2_147_483_648), ("99999999999999999999999", 2_147_483_648)
]

/// The public feed is served with `max-age=30`, so a poll answered from a cache
/// would be an older document stamped with the current fetch time. Every poll
/// therefore bypasses the local cache, and a shared cache's `Age` is folded
/// into the snapshot's residence.
@Suite(.serialized) @MainActor
struct FeedTransportTests {
    private func publicSession() throws -> ReadSession {
        try #require(ReadSession(baseURL: URL(string: "https://cached.invalid")!, principalID: nil,
                                 audience: .publicAudience, authorizationRevision: "public-v1", token: nil))
    }

    private func network() -> URLSession {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [CachingFeedProtocol.self]
        return URLSession(configuration: configuration)
    }

    @discardableResult
    private func settle(until condition: @MainActor () async throws -> Bool) async throws -> Bool {
        for _ in 0..<500 {
            if try await condition() { return true }
            try await Task.sleep(for: .milliseconds(10))
        }
        return try await condition()
    }

    @Test func everyPublicPollReachesTheOriginDespiteACacheableAnswer() async throws {
        CachingFeedProtocol.reset()
        let directory = FileManager.default.temporaryDirectory.appending(path: "FeedTransport.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory); CachingFeedProtocol.reset() }
        let network = network()
        let store = StationStore(feedClient: FeedClient(session: network), eventStream: EventStream(session: network),
                                 cache: SnapshotCache(directory: directory))
        store.start(session: try publicSession(), stationIDs: ["roof"])
        #expect(try await settle { !store.observers.isEmpty && !store.isRefreshing })

        await store.refresh()
        await store.refresh()

        let polls = CachingFeedProtocol.originRequests.filter { $0.url!.path.hasSuffix("/observers") }
        #expect(polls.count == 3)
        #expect(CachingFeedProtocol.originRequests.allSatisfy { $0.cachePolicy == .reloadIgnoringLocalCacheData })
        await store.disconnect(clearCachedScope: true)
    }

    @Test func productionSessionNeverUsesTheProtocolCachePolicy() {
        let configuration = FeedClient.failFastSession().configuration
        #expect(configuration.requestCachePolicy == .reloadIgnoringLocalCacheData)
    }

    @Test func sharedCacheAgeIsReadFromTheResponse() async throws {
        CachingFeedProtocol.reset(age: "20")
        defer { CachingFeedProtocol.reset() }
        let envelope = try await FeedClient(session: network()).fetchObservers(session: publicSession())
        #expect(envelope.cacheAge == 20)
        #expect(envelope.time == "2026-10-07T12:00:00Z")
    }

    @Test(arguments: ageHeaderCases)
    func ageHeaderIsDeltaSecondsBoundedPerRFC9111(value: String?, expected: TimeInterval) throws {
        var headers: [String: String] = [:]
        if let value { headers["Age"] = value }
        let response = try #require(HTTPURLResponse(url: URL(string: "https://cached.invalid")!, statusCode: 200,
                                                    httpVersion: "HTTP/1.1", headerFields: headers))
        #expect(FeedClient.cacheAge(of: response) == expected)
    }

    @Test func cacheResidenceAgesTheLiveSnapshot() throws {
        let key = AudienceCacheKey(server: "https://cached.invalid", principal: AudienceCacheKey.anonymousPrincipal,
                                   audience: .publicAudience, authorizationRevision: "public-v1")
        let payload = try JSONDecoder().decode(ObserversPayload.self, from: Data(#"{"schema":"2.0","audience":"public","observers":[{"id":"roof","last_seen_s":1}]}"#.utf8))
        let served = "2026-10-07T12:00:00Z"
        let fresh = StationStore()
        try fresh.apply(ObserversSnapshot(receivedAt: Date(), scope: key, serverTime: served, payload: payload), cached: false)
        let freshAge = try #require(fresh.currentLastSeenAge(for: "roof"))
        #expect(freshAge >= 1 && freshAge < 2)

        let stale = StationStore()
        try stale.apply(ObserversSnapshot(receivedAt: Date(), scope: key, serverTime: served, payload: payload, cacheAge: 20), cached: false)
        let staleAge = try #require(stale.currentLastSeenAge(for: "roof"))
        #expect(staleAge >= 21 && staleAge < 22)
        let collectorTime = try #require(stale.collectorTime)
        let expectedClock = try #require(WireDate.parse(served)).addingTimeInterval(21)
        #expect(abs(collectorTime.timeIntervalSince(expectedClock)) < 1)

        // A tampered or absurd residence cannot push the clock off the scale.
        for invalid in [-5.0, .nan, .infinity, 1e300] {
            let store = StationStore()
            try store.apply(ObserversSnapshot(receivedAt: Date(), scope: key, serverTime: served, payload: payload, cacheAge: invalid), cached: false)
            let age = try #require(store.currentLastSeenAge(for: "roof"))
            #expect(age >= 1 && age <= FeedClient.maximumCacheAge + 2)
        }
    }
}
