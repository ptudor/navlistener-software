import Foundation
import Testing
@testable import IntegrityStation

private actor GenerationNet {
    var gate: ConnectionBarrier?
    var rows = #"[{"id":"live","last_seen_s":1}]"#
    var fail = false
    func hold(_ g: ConnectionBarrier) { gate = g }
    func setRows(_ r: String) { rows = r }
    func setFail(_ f: Bool) { fail = f }
    func response(_ request: URLRequest) async throws -> Data {
        let path = request.url!.path
        if path.hasSuffix("/observers") {
            // Decide this request's outcome BEFORE suspending, so a held
            // response still carries the payload it was issued for.
            let body = Data("{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"audience\":\"public\",\"observers\":\(rows)}}".utf8)
            let failing = fail
            if let gate { self.gate = nil; await gate.hold() }
            if failing { throw URLError(.notConnectedToInternet) }
            return body
        }
        if path.hasSuffix("/audiences") {
            return Data(#"{"ok":true,"data":{"schema":"2.0","revision":"public-v1","audiences":["public"]}}"#.utf8)
        }
        if path.hasSuffix("/conditions") {
            return Data(#"{"ok":true,"data":{"schema":"2.0","audience":"public","complete":true,"epoch":"a","cursor":10,"events":[]}}"#.utf8)
        }
        if path.hasSuffix("/gnss/events") { return Data("event: status\ndata: {\"status\":\"connected\"}\n\n".utf8) }
        return Data(#"{"ok":true,"data":{"schema":"2.0","audience":"public","events":[]}}"#.utf8)
    }
}

private final class GenerationProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest) async throws -> Data)?
    private var t: Task<Void, Never>?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        t = Task {
            do {
                guard let handler = Self.handler else { throw URLError(.badServerResponse) }
                let data = try await handler(request)
                client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: nil)!, cacheStoragePolicy: .notAllowed)
                client?.urlProtocol(self, didLoad: data)
                client?.urlProtocolDidFinishLoading(self)
            } catch { client?.urlProtocol(self, didFailWithError: error) }
        }
    }
    override func stopLoading() { t?.cancel() }
}

// regression fix regression (astra-6 verification): a user-initiated refresh
// (.refreshable / setForeground) runs on a task the
// store never cancels. Only the start/stop generation can retire it.
@Suite(.serialized) @MainActor
struct StoreGenerationTests {
    @Test func userInitiatedRefreshCannotOutliveAnEqualSessionRestart() async throws {
        let dir = FileManager.default.temporaryDirectory.appending(path: "StoreGen.\(UUID())")
        defer { try? FileManager.default.removeItem(at: dir); GenerationProtocol.handler = nil }
        let net = GenerationNet()
        GenerationProtocol.handler = { try await net.response($0) }
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [GenerationProtocol.self]
        let urlSession = URLSession(configuration: config)
        let store = StationStore(feedClient: FeedClient(session: urlSession),
                                 eventStream: EventStream(session: urlSession),
                                 cache: SnapshotCache(directory: dir))
        let session = try #require(ReadSession(baseURL: URL(string: "https://verify.invalid")!, principalID: nil,
                                               audience: .publicAudience, authorizationRevision: "public-v1", token: nil))
        store.start(session: session, stationIDs: ["live"])
        for _ in 0..<200 where store.observers.isEmpty || store.isRefreshing { try await Task.sleep(for: .milliseconds(10)) }
        #expect(store.observers.map(\.id) == ["live"])

        // A pull-to-refresh whose response is held mid-flight.
        let gate = ConnectionBarrier()
        await net.hold(gate)
        await net.setRows(#"[{"id":"stale","last_seen_s":1}]"#)
        let pull = Task { await store.refresh() }
        await gate.waitUntilEntered()

        // The same ReadSession is restarted; presentation is reset and the
        // network is then unavailable, so nothing legitimate can repopulate it.
        await net.setFail(true)
        store.start(session: session, stationIDs: ["live"])
        while store.errorMessage == nil { await Task.yield() }
        await gate.release()
        await pull.value
        try await Task.sleep(for: .milliseconds(60))
        #expect(store.observers.allSatisfy { $0.id != "stale" })
        await store.disconnect(clearCachedScope: true)
    }
}
