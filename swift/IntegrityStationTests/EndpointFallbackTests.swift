import Foundation
import Testing
@testable import IntegrityStation

private final class FallbackProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var failure = 0
    /// The transport error the primary reports when `failure` is 0.
    nonisolated(unsafe) static var primaryError = URLError.Code.cannotFindHost
    nonisolated(unsafe) static var hosts: [String] = []
    nonisolated(unsafe) static var updateBodies: [Data] = []
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let host = request.url!.host!
        Self.hosts.append(host)
        let update = request.url!.path == "/gnss/api/v2/updates"
        if update {
            var body = request.httpBody ?? Data()
            if let stream = request.httpBodyStream {
                stream.open(); defer { stream.close() }
                var bytes = [UInt8](repeating: 0, count: 2048)
                while stream.hasBytesAvailable {
                    let n = stream.read(&bytes, maxLength: bytes.count)
                    if n <= 0 { break }
                    body.append(contentsOf: bytes.prefix(n))
                }
            }
            Self.updateBodies.append(body)
        }
        #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer test-token")
        if host == "in.intsat.net" && Self.failure == 0 {
            client?.urlProtocol(self, didFailWithError: URLError(Self.primaryError))
            return
        }
        let status = host == "in.intsat.net" ? Self.failure : 200
        client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: nil)!, cacheStoragePolicy: .notAllowed)
        let data = update ? #"{"request_status":"requested","record":{"command":{"command_id":"9007199254740993"}}}"# : #"{"ok":true,"data":{"audiences":[]}}"#
        client?.urlProtocol(self, didLoad: Data(data.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

@Suite(.serialized)
struct EndpointFallbackTests {
    @Test func updatePostRetriesTheSameDurableRequest() async throws {
        FallbackProtocol.failure = 503; FallbackProtocol.hosts = []; FallbackProtocol.updateBodies = []
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [FallbackProtocol.self]
        let network = URLSession(configuration: config)
        defer { network.invalidateAndCancel() }
        let audience = try #require(ReadAudience("organization:customer-a"))
        let session = try #require(ReadSession(baseURL: URL(string: "https://in.intsat.net")!, principalID: "viewer-a",
                                              audience: audience, authorizationRevision: "v1", token: "test-token"))
        let id = String(repeating: "a", count: 32)
        let result = try await FeedClient(session: network).updateAccess(session: session, observer: "observer-1", action: "check", requestID: id)
        #expect(result.requestStatus == "requested")
        #expect(result.record.command.commandID == "9007199254740993")
        #expect(FallbackProtocol.hosts == ["in.intsat.net", "in.intsat.space"])
        #expect(FallbackProtocol.updateBodies.count == 2)
        #expect(FallbackProtocol.updateBodies[0] == FallbackProtocol.updateBodies[1])
        let fields = try JSONDecoder().decode([String:String].self, from: FallbackProtocol.updateBodies[0])
        #expect(fields["request_id"] == id)
    }
    @Test func aliasesKeepResourceAndPort() throws {
        let url = try #require(URL(string: "https://in.intsat.net:8443/gnss/events?since=a%2Fb"))
        #expect(CollectorEndpoint.secondaryURL(url)?.absoluteString == "https://in.intsat.space:8443/gnss/events?since=a%2Fb")
        for value in ["https://intsat.net", "https://unrelated.intsat.net", "https://in.intsat.net.evil.invalid", "http://in.intsat.net", "https://user@in.intsat.net"] {
            #expect(CollectorEndpoint.secondaryURL(URL(string: value)!) == nil)
        }
        #expect(!CollectorEndpoint.canRetry(URLError(.cancelled)))
        #expect(!CollectorEndpoint.canRetry(FeedError.forbidden(nil)))
        for code in [URLError.Code.cannotFindHost, .timedOut, .networkConnectionLost, .cannotConnectToHost] {
            #expect(CollectorEndpoint.canRetry(URLError(code)))
        }
        // A certificate or TLS failure on the primary is shown, never routed
        // around through the alias.
        for code in [URLError.Code.secureConnectionFailed, .serverCertificateHasBadDate, .serverCertificateUntrusted,
                     .serverCertificateHasUnknownRoot, .serverCertificateNotYetValid, .clientCertificateRejected,
                     .clientCertificateRequired, .appTransportSecurityRequiresSecureConnection] {
            #expect(!CollectorEndpoint.canRetry(URLError(code)), "\(code)")
        }
    }

    @Test func certificateFailureOnThePrimaryIsNotMaskedByTheAlias() async throws {
        FallbackProtocol.failure = 0; FallbackProtocol.primaryError = .serverCertificateUntrusted; FallbackProtocol.hosts = []
        defer { FallbackProtocol.primaryError = .cannotFindHost }
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [FallbackProtocol.self]
        let network = URLSession(configuration: config)
        defer { network.invalidateAndCancel() }
        do {
            _ = try await FeedClient(session: network).fetchAudiences(baseURL: URL(string: "https://in.intsat.net")!, token: "test-token")
            Issue.record("Expected the certificate failure to surface")
        } catch let error as URLError {
            #expect(error.code == .serverCertificateUntrusted)
        }
        #expect(FallbackProtocol.hosts == ["in.intsat.net"])
    }

    @Test(arguments: [0, 503, 401, 403]) func discoveryUsesOnlyApprovedPeer(failure: Int) async throws {
        FallbackProtocol.failure = failure; FallbackProtocol.primaryError = .cannotFindHost; FallbackProtocol.hosts = []
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [FallbackProtocol.self]
        let network = URLSession(configuration: config)
        defer { network.invalidateAndCancel() }
        do {
            _ = try await FeedClient(session: network).fetchAudiences(baseURL: URL(string: "https://in.intsat.net")!, token: "test-token")
            #expect(failure != 401 && failure != 403)
        } catch let error as FeedError {
            #expect((failure == 401 || failure == 403) && error.isAuthorizationLoss)
        }
        #expect(FallbackProtocol.hosts == (failure == 401 || failure == 403 ? ["in.intsat.net"] : ["in.intsat.net", "in.intsat.space"]))
    }
}
