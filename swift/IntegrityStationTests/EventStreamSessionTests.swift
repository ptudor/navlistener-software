import Foundation
import Testing
@testable import IntegrityStation

/// Records the request the stream opens, then serves one connected status and
/// ends, so the stream's transport settings can be inspected.
private final class RecordingSSEProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var recorded: [URLRequest] = []
    static var requests: [URLRequest] {
        get { lock.withLock { recorded } }
        set { lock.withLock { recorded = newValue } }
    }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        Self.requests.append(request)
        client?.urlProtocol(self, didReceive: HTTPURLResponse(
            url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: [:])!,
            cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data("event: status\ndata: {\"status\":\"connected\"}\n\n".utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

/// The SSE session must not bound a healthy stream's lifetime. The resource
/// timeout caps a task's total age, not its idle time, so a 90 s value cut
/// every connection on a timer; the 75 s idle watchdog (server heartbeat
/// 60 s plus slack) is the only timeout a long-lived stream should carry.
@Suite(.serialized)
struct EventStreamSessionTests {
    @Test func productionSessionDoesNotCapStreamLifetime() {
        let configuration = EventStream.failFastSession().configuration
        #expect(configuration.timeoutIntervalForResource > 3600)
        #expect(configuration.timeoutIntervalForRequest == EventStream.idleTimeout)
        #expect(EventStream.idleTimeout == 75)
        #expect(!configuration.waitsForConnectivity)
    }

    @Test func streamRequestCarriesTheIdleWatchdogOnly() async throws {
        RecordingSSEProtocol.requests = []
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [RecordingSSEProtocol.self]
        let network = URLSession(configuration: configuration)
        defer { network.invalidateAndCancel() }
        let readSession = try #require(ReadSession(baseURL: URL(string: "https://stream.invalid")!,
                                                   principalID: nil, audience: .publicAudience,
                                                   authorizationRevision: "a", token: nil))
        var statuses: [String] = []
        do {
            for try await update in try EventStream(session: network).updates(session: readSession, lastEventID: nil) {
                if case .status(let status) = update { statuses.append(status) }
            }
        } catch let error as FeedError {
            #expect(error == .streamEnded)
        }
        #expect(statuses == ["connected"])
        let request = try #require(RecordingSSEProtocol.requests.first)
        #expect(request.timeoutInterval == EventStream.idleTimeout)
        #expect(request.value(forHTTPHeaderField: "Accept") == "text/event-stream")
    }
}
