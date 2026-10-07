import Foundation

/// Answers collector requests from a responder registered per host, so suites
/// running in parallel never see each other's traffic. A `/gnss/events`
/// response is held open after its body, as a live SSE connection is, until
/// the task is cancelled; every other response finishes at once.
///
/// URLSession batches a protocol's first small chunk and hands it to the
/// reader only once more data or the end of the load arrives (measured: a
/// 50-byte body reached the stream at end-of-load, 1 KiB streamed at once).
/// A stream body is therefore preceded by an SSE comment of 1 KiB, which the
/// accumulator ignores, so frames reach the store while the stream is open.
final class StubCollectorProtocol: URLProtocol, @unchecked Sendable {
    typealias Responder = @Sendable (URLRequest) -> Data

    static let streamPreamble = Data((": " + String(repeating: "x", count: 1024) + "\n\n").utf8)

    private static let lock = NSLock()
    nonisolated(unsafe) private static var responders: [String: Responder] = [:]
    private var producer: Task<Void, Never>?

    static func register(host: String, _ responder: @escaping Responder) {
        lock.withLock { responders[host] = responder }
    }

    static func unregister(host: String) {
        lock.withLock { responders[host] = nil }
    }

    static func session() -> URLSession {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [StubCollectorProtocol.self]
        return URLSession(configuration: configuration)
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let host = request.url?.host ?? ""
        guard let responder = Self.lock.withLock({ Self.responders[host] }) else {
            client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost))
            return
        }
        let streaming = request.url?.path.hasSuffix("/gnss/events") == true
        let body = (streaming ? Self.streamPreamble : Data()) + responder(request)
        producer = Task { @Sendable [self] in
            client?.urlProtocol(self, didReceive: HTTPURLResponse(
                url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: [:])!,
                cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: body)
            if streaming {
                do {
                    for _ in 0..<12_000 {
                        try Task.checkCancellation()
                        try await Task.sleep(for: .milliseconds(5))
                    }
                } catch { return }
            }
            client?.urlProtocolDidFinishLoading(self)
        }
    }

    override func stopLoading() { producer?.cancel() }
}
