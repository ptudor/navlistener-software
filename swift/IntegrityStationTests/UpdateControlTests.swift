import Foundation
import Testing
@testable import IntegrityStation

/// Answers the update-control endpoint the way the collector's handler does:
/// outside the v2 envelope, with http.Error's plain-text reasons.
private final class UpdateStubProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var status = 200
    nonisolated(unsafe) private static var body = Data()
    nonisolated(unsafe) private static var contentType: String?
    nonisolated(unsafe) private static var seen: [URLRequest] = []

    static var requests: [URLRequest] { lock.withLock { seen } }
    static func respond(status: Int, body: String, contentType: String? = "text/plain; charset=utf-8") {
        lock.withLock { self.status = status; self.body = Data(body.utf8); self.contentType = contentType; seen = [] }
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let (status, body, contentType) = Self.lock.withLock { Self.seen.append(request); return (Self.status, Self.body, Self.contentType) }
        var headers: [String: String] = [:]
        if let contentType { headers["Content-Type"] = contentType }
        client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1",
                                                            headerFields: headers)!, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

private let rejectionCases: [(Int, String)] = [
    (409, "a command is already pending for this observer"),
    (503, "update controls are not configured"),
    (400, "request_id must be 32 lowercase hex characters and all five fields are required"),
    (405, "unsupported request")
]

/// The updates endpoint does not use the v2 envelope, so its reasons used to
/// collapse into "Collector returned HTTP 409" and the initial access query
/// into silence. The collector's text now reaches the user, and a denied
/// update grant is its own error, never a read-authorization loss.
@Suite(.serialized)
struct UpdateControlTests {
    private func client() -> FeedClient {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [UpdateStubProtocol.self]
        return FeedClient(session: URLSession(configuration: configuration))
    }

    private func session() throws -> ReadSession {
        try #require(ReadSession(baseURL: URL(string: "https://updates.invalid")!, principalID: "viewer-a",
                                 audience: ReadAudience("organization:customer-a")!, authorizationRevision: "v1",
                                 token: "test-token"))
    }

    @Test(arguments: [401, 403])
    func deniedGrantCarriesTheCollectorReasonAndIsNotAnAuthorizationLoss(status: Int) async throws {
        let reason = "update grant required for this enrolled observer"
        UpdateStubProtocol.respond(status: status, body: reason + "\n")
        do {
            _ = try await client().updateAccess(session: session(), observer: "rx+1")
            Issue.record("Expected a denied update grant")
        } catch let error as FeedError {
            #expect(error == .updateDenied(reason))
            #expect(!error.isAuthorizationLoss)
            #expect(!CollectorEndpoint.canRetry(error))
            #expect(error.localizedDescription == reason)
            #expect(UpdateControlsView.loadFailureMessage(error) == "Update control is not granted: " + reason)
        }
        // The access query names the observer id with "+" escaped.
        #expect(UpdateStubProtocol.requests.first?.url?.query(percentEncoded: true) == "observer_id=rx%2B1")
        #expect(UpdateStubProtocol.requests.first?.httpMethod == "GET")
    }

    @Test(arguments: rejectionCases)
    func plainTextRejectionsSurfaceTheirReason(status: Int, reason: String) async throws {
        UpdateStubProtocol.respond(status: status, body: reason + "\n")
        do {
            _ = try await client().updateAccess(session: session(), observer: "observer-1", action: "check")
            Issue.record("Expected a rejection")
        } catch let error as FeedError {
            #expect(error == .updateRejected(status: status, message: reason))
            #expect(error.localizedDescription == reason)
            #expect(!error.isAuthorizationLoss)
            #expect(CollectorEndpoint.canRetry(error) == (status >= 500))
            #expect(UpdateControlsView.loadFailureMessage(error) == "Update controls are unavailable: " + reason)
        }
    }

    @Test func aJSONReasonIsHonouredAndOtherContentFallsBackToTheStatus() async throws {
        UpdateStubProtocol.respond(status: 403, body: #"{"error":"no grant for this observer"}"#, contentType: "application/json")
        await #expect(throws: FeedError.updateDenied("no grant for this observer")) {
            _ = try await client().updateAccess(session: session(), observer: "observer-1")
        }
        UpdateStubProtocol.respond(status: 403, body: "<html><body>Forbidden</body></html>", contentType: "text/html")
        do {
            _ = try await client().updateAccess(session: session(), observer: "observer-1")
            Issue.record("Expected a denied update grant")
        } catch let error as FeedError {
            #expect(error == .updateDenied(nil))
            #expect(error.localizedDescription == String(localized: "error.update_denied"))
        }
        UpdateStubProtocol.respond(status: 502, body: "<html><body>Bad Gateway</body></html>", contentType: "text/html")
        await #expect(throws: FeedError.http(502)) {
            _ = try await client().updateAccess(session: session(), observer: "observer-1")
        }
        UpdateStubProtocol.respond(status: 409, body: "   \n")
        await #expect(throws: FeedError.http(409)) {
            _ = try await client().updateAccess(session: session(), observer: "observer-1", action: "cancel")
        }
    }

    @Test func aServerReasonIsBoundedForDisplay() async throws {
        let long = String(repeating: "r", count: 1_000)
        UpdateStubProtocol.respond(status: 409, body: long)
        do {
            _ = try await client().updateAccess(session: session(), observer: "observer-1", action: "check")
            Issue.record("Expected a rejection")
        } catch let error as FeedError {
            #expect(error == .updateRejected(status: 409, message: String(long.prefix(FeedClient.reasonCharacters))))
        }
    }

    @Test func aPublicSessionCannotQueryUpdateControl() async throws {
        let publicSession = try #require(ReadSession(baseURL: URL(string: "https://updates.invalid")!, principalID: nil,
                                                     audience: .publicAudience, authorizationRevision: "public", token: nil))
        await #expect(throws: FeedError.updateDenied(nil)) {
            _ = try await client().updateAccess(session: publicSession, observer: "observer-1")
        }
    }
}
