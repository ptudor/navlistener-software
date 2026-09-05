import Foundation
import Testing
@testable import IntegrityStation

private final class LimitProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var mode = "line"
    private static let cancellationLock = NSLock()
    nonisolated(unsafe) private static var wasCancelled = false
    static var cancelled: Bool {
        get { cancellationLock.withLock { wasCancelled } }
        set { cancellationLock.withLock { wasCancelled = newValue } }
    }
    private var producer: Task<Void, Never>?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let mode = Self.mode
        producer = Task {
            let headers = mode == "json" ? ["Content-Length": "\(NetworkLimits.responseBytes+1)"] : [:]
            client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: mode == "error" ? 500 : 200, httpVersion: "HTTP/1.1", headerFields: headers)!, cacheStoragePolicy: .notAllowed)
            let chunk: Data
            switch mode {
            case "line", "json", "error": chunk = Data(repeating: 120, count: 4096)
            case "frame": chunk = Data("data: \(String(repeating: "x", count: 4000))\n".utf8)
            default: chunk = Data("id: 1\nevent: gnss\ndata: {\"id\":1,\"sv\":\"roof\",\"type\":\"jamming_detected\",\"new_value\":\"jammed\",\"severity\":1}\n\n".utf8)
            }
            do {
                for _ in 0..<2000 {
                    try Task.checkCancellation()
                    client?.urlProtocol(self, didLoad: chunk)
                    try await Task.sleep(for: .milliseconds(1))
                }
                client?.urlProtocolDidFinishLoading(self)
            } catch {}
        }
    }
    override func stopLoading() { Self.cancelled = true; producer?.cancel() }
}

private actor LimitFailure {
    var failed = false
    func mark() { failed = true }
}

@Suite(.serialized)
struct NetworkLimitTests {
    private func session() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [LimitProtocol.self]
        return URLSession(configuration: config)
    }
    private func readSession() throws -> ReadSession {
        try #require(ReadSession(baseURL: URL(string: "https://limits.invalid")!, principalID: nil, audience: .publicAudience, authorizationRevision: "a", token: nil))
    }
    @Test(arguments: ["line", "frame", "queue"])
    func streamLimitsCancelAndSurfaceFailure(mode: String) async throws {
        LimitProtocol.mode = mode; LimitProtocol.cancelled = false
        let network = session(); defer { network.invalidateAndCancel() }
        let failure = LimitFailure()
        let updates = try EventStream(session: network).updates(session: readSession(), lastEventID: nil, onFailure: { await failure.mark() })
        if mode == "queue" {
            // Simulate the consumer waiting on cursor storage. Failure must
            // notify the store before it can drain even one pending event.
            for _ in 0..<200 where !(await failure.failed) { try await Task.sleep(for: .milliseconds(5)) }
            #expect(await failure.failed)
        }
        var retained = 0
        do {
            for try await _ in updates { retained += 1 }
            Issue.record("Expected input bound failure")
        } catch let error as FeedError { #expect(error == .inputLimit) }
        #expect(retained <= NetworkLimits.pendingEvents)
        #expect(await failure.failed)
        for _ in 0..<100 where !LimitProtocol.cancelled { try await Task.sleep(for: .milliseconds(2)) }
        #expect(LimitProtocol.cancelled)
        // Reconciliation after overflow replaces the bounded prefix with the
        // durable resolution, without replaying an unresolved activation.
        var state = ConditionState()
        let event = try JSONDecoder().decode(GNSSAPIEvent.self, from: Data(#"{"id":100,"sv":"roof","type":"jamming_detected","new_value":"ok","severity":0}"#.utf8))
        #expect(try state.install(ConditionsPayload(schema: "2.0", audience: "public", complete: true, epoch: "a", cursor: 100, events: [event])))
        #expect(state.active.isEmpty)
    }
    @Test(arguments: ["json", "error"])
    func oversizedBodiesAreCancelled(mode: String) async throws {
        LimitProtocol.mode = mode; LimitProtocol.cancelled = false
        let network = session(); defer { network.invalidateAndCancel() }
        do {
            _ = try await FeedClient(session: network).fetchObservers(session: readSession())
            Issue.record("Expected response limit")
        } catch let error as FeedError { #expect(error == .inputLimit) }
        for _ in 0..<100 where !LimitProtocol.cancelled { try await Task.sleep(for: .milliseconds(2)) }
        #expect(LimitProtocol.cancelled)
    }
    @Test func receivedByteLimitDoesNotTrustHeaders() async throws {
        LimitProtocol.mode = "line"; LimitProtocol.cancelled = false
        let network = session(); defer { network.invalidateAndCancel() }
        let (bytes, _) = try await network.bytes(from: URL(string: "https://limits.invalid")!)
        defer { bytes.task.cancel() }
        do { _ = try await NetworkLimits.body(bytes, maximum: 1024); Issue.record("Expected byte limit") }
        catch let error as FeedError { #expect(error == .inputLimit) }
    }
    @Test func accumulatorRetainsAtMostOneBoundedFrame() throws {
        var parser = SSEAccumulator()
        do {
            for _ in 0..<NetworkLimits.frameBytes { _ = try parser.consume("data:") }
            Issue.record("Expected frame limit")
        } catch let error as FeedError { #expect(error == .inputLimit) }
        #expect(parser.retainedBytes <= NetworkLimits.frameBytes)
    }
}
