import Foundation
import Testing
@testable import IntegrityStation

private actor BoardMonitorNetwork {
    var observerRequests = 0
    var observerGate: ConnectionBarrier?
    func hold(_ gate: ConnectionBarrier) { observerGate = gate }
    func response(_ request: URLRequest) async throws -> Data {
        let path = request.url!.path
        if path.hasSuffix("/audiences") {
            return Data(#"{"ok":true,"data":{"schema":"2.0","principal":"reader","revision":"v1","audiences":["public","organization:example"]}}"#.utf8)
        }
        if path.hasSuffix("/observers") {
            observerRequests += 1
            if let gate = observerGate { observerGate = nil; await gate.hold() }
            return Data("{\"ok\":true,\"time\":\"2026-09-14T12:00:00Z\",\"data\":\(BoardFixture.json)}".utf8)
        }
        if path.hasSuffix("/conditions") {
            return Data(#"{"ok":true,"data":{"schema":"2.0","audience":"organization:example","complete":true,"epoch":"a","cursor":1,"events":[]}}"#.utf8)
        }
        if path.hasSuffix("/gnss/events") { return Data("event: status\ndata: {\"status\":\"connected\"}\n\n".utf8) }
        return Data(#"{"ok":true,"data":{"schema":"2.0","audience":"organization:example","events":[]}}"#.utf8)
    }
}

private final class BoardMonitorProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest) async throws -> Data)?
    private var loading: Task<Void, Never>?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        loading = Task { @Sendable [self] in
            do {
                guard let handler = Self.handler else { throw URLError(.badServerResponse) }
                let data = try await handler(request)
                client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: 200,
                    httpVersion: "HTTP/1.1", headerFields: nil)!, cacheStoragePolicy: .notAllowed)
                client?.urlProtocol(self, didLoad: data)
                client?.urlProtocolDidFinishLoading(self)
            } catch { client?.urlProtocol(self, didFailWithError: error) }
        }
    }
    override func stopLoading() { loading?.cancel() }
}

@MainActor @Suite(.serialized) struct BoardMonitorTests {
    private func makeStore(_ network: BoardMonitorNetwork, directory: URL) throws -> StationStore {
        BoardMonitorProtocol.handler = { try await network.response($0) }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [BoardMonitorProtocol.self]
        let session = URLSession(configuration: configuration)
        let store = StationStore(feedClient: FeedClient(session: session), eventStream: EventStream(session: session),
                                 cache: SnapshotCache(directory: directory))
        store.start(session: try #require(ReadSession(baseURL: URL(string: "https://collector.invalid")!,
            principalID: "reader", audience: ReadAudience("organization:example")!,
            authorizationRevision: "v1", token: "read-token")), stationIDs: ["observer-s3"])
        return store
    }

    @Test func detailMonitoringRefreshesTimingAndStopsOnCancellation() async throws {
        let directory = FileManager.default.temporaryDirectory.appending(path: "BoardMonitor.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory); BoardMonitorProtocol.handler = nil }
        let network = BoardMonitorNetwork()
        let store = try makeStore(network, directory: directory)
        for _ in 0..<200 where store.observers.isEmpty || store.isRefreshing { try await Task.sleep(for: .milliseconds(10)) }
        #expect(store.observers.first?.board?.timing != nil)
        let monitor = Task { await store.monitorBoard(for: "observer-s3") }
        try await Task.sleep(for: .milliseconds(1200))
        #expect(await network.observerRequests >= 3)
        monitor.cancel()
        await monitor.value
        let stopped = await network.observerRequests
        try await Task.sleep(for: .milliseconds(1100))
        #expect(await network.observerRequests == stopped)
        await store.disconnect(clearCachedScope: true)
    }

    @Test func aRetiredDetailRequestCannotRepopulatePrivatePresentation() async throws {
        let directory = FileManager.default.temporaryDirectory.appending(path: "BoardMonitorRace.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory); BoardMonitorProtocol.handler = nil }
        let network = BoardMonitorNetwork()
        let store = try makeStore(network, directory: directory)
        for _ in 0..<200 where store.observers.isEmpty || store.isRefreshing { try await Task.sleep(for: .milliseconds(10)) }
        #expect(store.observers.first?.board?.timing != nil)
        let gate = ConnectionBarrier()
        await network.hold(gate)
        let monitor = Task { await store.monitorBoard(for: "observer-s3") }
        await gate.waitUntilEntered()
        await store.disconnect(clearCachedScope: true)
        await gate.release()
        await monitor.value
        #expect(store.observers.isEmpty)
        #expect(store.activeSession == nil)
    }
}
