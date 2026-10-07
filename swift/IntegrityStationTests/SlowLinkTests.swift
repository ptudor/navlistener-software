import Foundation
import Testing
@testable import IntegrityStation

/// Streams one observers document of a chosen size at a chosen rate, the way
/// a large fleet arrives over a slow link, so the production session's
/// resource timeout is exercised against a transfer that outlives it.
private final class SlowLinkProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var totalBytes = 0
    nonisolated(unsafe) static var bytesPerSecond = 0
    private var producer: Task<Void, Never>?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let body = Self.document(bytes: Self.totalBytes)
        let rate = Self.bytesPerSecond
        producer = Task { @Sendable [self] in
            client?.urlProtocol(self, didReceive: HTTPURLResponse(
                url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1",
                headerFields: ["Content-Type": "application/json"])!, cacheStoragePolicy: .notAllowed)
            var offset = 0
            do {
                while offset < body.count {
                    try Task.checkCancellation()
                    let end = min(offset + rate, body.count)
                    client?.urlProtocol(self, didLoad: body.subdata(in: offset..<end))
                    offset = end
                    if offset < body.count { try await Task.sleep(for: .seconds(1)) }
                }
                client?.urlProtocolDidFinishLoading(self)
            } catch {}
        }
    }

    override func stopLoading() { producer?.cancel() }

    private static let head = #"{"ok":true,"time":"2026-10-07T12:00:00Z","data":{"schema":"2.0","audience":"public","observers":[{"id":"fleet","remark":""#
    private static let tail = #""}]}}"#
    /// Bytes of envelope around the padding remark.
    static let overhead = head.utf8.count + tail.utf8.count

    /// A valid envelope padded to exactly `bytes` with an operator remark.
    static func document(bytes: Int) -> Data {
        Data((head + String(repeating: "x", count: max(0, bytes - overhead)) + tail).utf8)
    }
}

/// The response bound and the session's total-time bound must be consistent:
/// a document the client is willing to accept must be able to arrive over a
/// slow link. A 4 MiB fleet at 200 KiB/s takes about 21 s, beyond the 15 s
/// the session used to allow.
@Suite(.serialized)
struct SlowLinkTests {
    @Test func productionSessionsBoundTotalTimeConsistentlyWithTheByteLimits() {
        let feed = FeedClient.failFastSession().configuration
        #expect(feed.timeoutIntervalForRequest == FeedClient.idleTimeout)
        #expect(feed.timeoutIntervalForResource == FeedClient.resourceTimeout)
        #expect(FeedClient.idleTimeout == 10)
        #expect(FeedClient.resourceTimeout >= 120)
        #expect(!feed.waitsForConnectivity)

        let portal = OwnerPortalClient().session.configuration
        #expect(portal.timeoutIntervalForRequest == OwnerPortalClient.idleTimeout)
        #expect(portal.timeoutIntervalForResource == OwnerPortalClient.resourceTimeout)
        #expect(OwnerPortalClient.resourceTimeout >= 120)
        #expect(OwnerPortalClient.responseBytes == 8 * 1024 * 1024)
    }

    @Test(.timeLimit(.minutes(2)))
    func fourMebibytesAtTwoHundredKibibytesPerSecondCompletes() async throws {
        SlowLinkProtocol.totalBytes = 4 * 1024 * 1024
        SlowLinkProtocol.bytesPerSecond = 200 * 1024
        let configuration = FeedClient.failFastSession().configuration
        configuration.protocolClasses = [SlowLinkProtocol.self]
        let network = URLSession(configuration: configuration)
        defer { network.invalidateAndCancel() }
        let session = try #require(ReadSession(baseURL: URL(string: "https://slow.invalid")!, principalID: nil,
                                               audience: .publicAudience, authorizationRevision: "public-v1", token: nil))
        let started = ContinuousClock.now
        let envelope = try await FeedClient(session: network).fetchObservers(session: session)
        let elapsed = started.duration(to: .now)
        #expect(elapsed > .seconds(15), "transfer must outlive the former 15 s resource bound: \(elapsed)")
        let fleet = try #require(envelope.data?.observers?.first)
        #expect(fleet.id == "fleet")
        #expect(fleet.remark?.utf8.count == 4 * 1024 * 1024 - SlowLinkProtocol.overhead)
    }

    @Test func bodyAccumulatesInChunksAndStillBoundsReceivedBytes() async throws {
        SlowLinkProtocol.totalBytes = 3 * NetworkLimits.chunkBytes + 17
        SlowLinkProtocol.bytesPerSecond = SlowLinkProtocol.totalBytes
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [SlowLinkProtocol.self]
        let network = URLSession(configuration: configuration)
        defer { network.invalidateAndCancel() }
        let request = URLRequest(url: URL(string: "https://slow.invalid/gnss/api/v2/observers")!)

        let (exact, _) = try await network.bytes(for: request)
        let body = try await NetworkLimits.body(exact, maximum: SlowLinkProtocol.totalBytes)
        #expect(body == SlowLinkProtocol.document(bytes: SlowLinkProtocol.totalBytes))

        let (bounded, _) = try await network.bytes(for: request)
        await #expect(throws: FeedError.inputLimit) {
            _ = try await NetworkLimits.body(bounded, maximum: SlowLinkProtocol.totalBytes - 1)
        }
    }
}
