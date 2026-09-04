import Foundation
import Testing
@testable import IntegrityStation

@Suite(.serialized)
struct AudienceNetworkingTests {
    @Test
    func everyScopedRequestUsesTheSameAudienceAndCredentialHeaders() throws {
        let baseURL = try #require(URL(string: "https://collector.invalid"))
        let audience = try #require(ReadAudience("organization:customer-a"))
        let session = try #require(
            ReadSession(
                baseURL: baseURL,
                principalID: "viewer-a",
                audience: audience,
                authorizationRevision: "grant-v1",
                token: "read-token-a"
            )
        )
        var request = URLRequest(url: baseURL)

        try ReadRequestHeaders.apply(session: session, to: &request)

        #expect(request.value(forHTTPHeaderField: "X-GNSS-Audience") == "organization:customer-a")
        #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer read-token-a")
        #expect(request.cachePolicy == .reloadIgnoringLocalCacheData)

        let publicSession = try #require(
            ReadSession(
                baseURL: baseURL,
                principalID: nil,
                audience: .publicAudience,
                authorizationRevision: "public-v1",
                token: nil
            )
        )
        var publicRequest = URLRequest(url: baseURL)
        try ReadRequestHeaders.apply(session: publicSession, to: &publicRequest)
        #expect(publicRequest.value(forHTTPHeaderField: "X-GNSS-Audience") == "public")
        #expect(publicRequest.value(forHTTPHeaderField: "Authorization") == nil)
    }

    @Test
    func feedClientMapsAuthorizationResponsesWithoutTreatingThemAsEmptyData() async throws {
        URLProtocolStub.handler = { request in
            let body = Data(#"{"ok":false,"error":"audience is not granted","code":403}"#.utf8)
            return (Self.response(for: request, status: 403), body)
        }
        defer { URLProtocolStub.handler = nil }

        let baseURL = try #require(URL(string: "https://collector.invalid"))
        let audience = try #require(ReadAudience("organization:customer-a"))
        let session = try #require(
            ReadSession(
                baseURL: baseURL,
                principalID: "viewer-a",
                audience: audience,
                authorizationRevision: "grant-v1",
                token: "read-token-a"
            )
        )
        let client = FeedClient(session: Self.stubbedSession())

        do {
            _ = try await client.fetchObservers(session: session)
            Issue.record("Expected authorization failure")
        } catch let error as FeedError {
            #expect(error == .forbidden("audience is not granted"))
            #expect(error.isAuthorizationLoss)
        }
    }

    @MainActor
    @Test
    func authenticatedControllerUsesAnonymousRevisionWhenSelectingPublic() async throws {
        URLProtocolStub.handler = { request in
            let path = request.url?.path ?? ""
            let body: Data
            if path.hasSuffix("/gnss/api/v2/audiences") {
                if request.value(forHTTPHeaderField: "Authorization") == "Bearer read-token-a" {
                    body = Data(#"{"ok":true,"data":{"schema":"2.0","principal":"viewer-a","revision":"private-policy-v1","audiences":["public","organization:customer-a"]}}"#.utf8)
                } else {
                    body = Data(#"{"ok":true,"data":{"schema":"2.0","revision":"public-policy-v7","audiences":["public"]}}"#.utf8)
                }
            } else if path.hasSuffix("/gnss/api/v2/observers") {
                let audience = request.value(forHTTPHeaderField: "X-GNSS-Audience") ?? ""
                body = Data("{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"audience\":\"\(audience)\",\"observers\":[]}}".utf8)
            } else if path.hasSuffix("/gnss/api/events") {
                let audience = request.value(forHTTPHeaderField: "X-GNSS-Audience") ?? ""
                body = Data("{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"audience\":\"\(audience)\",\"events\":[]}}".utf8)
            } else {
                body = Data("event: status\ndata: {\"status\":\"connected\"}\n\n".utf8)
            }
            return (Self.response(for: request, status: 200), body)
        }
        defer { URLProtocolStub.handler = nil }

        let suiteName = "IntegrityStationAudienceTests.\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suiteName))
        defer { defaults.removePersistentDomain(forName: suiteName) }
        let cacheDirectory = FileManager.default.temporaryDirectory
            .appending(path: "integrity-station-controller-\(UUID().uuidString)", directoryHint: .isDirectory)
        defer { try? FileManager.default.removeItem(at: cacheDirectory) }
        let networkSession = Self.stubbedSession()
        let stationStore = StationStore(
            feedClient: FeedClient(session: networkSession),
            eventStream: EventStream(session: networkSession),
            cache: SnapshotCache(directory: cacheDirectory)
        )
        let controller = AppController(
            store: stationStore,
            settings: AppSettings(defaults: defaults),
            secureStore: MemoryConnectionStore(),
            feedClient: FeedClient(session: networkSession)
        )

        try await controller.connect(to: "https://collector.invalid", readToken: "read-token-a")
        #expect(controller.selectedAudience.rawValue == "organization:customer-a")
        #expect(controller.principalID == "viewer-a")
        #expect(controller.authorizationRevision == "private-policy-v1")

        try await controller.selectAudience(.publicAudience)
        #expect(controller.principalID == AudienceCacheKey.anonymousPrincipal)
        #expect(controller.authorizationRevision == "public-policy-v7")
        #expect(controller.store.activeSession?.token == nil)
        #expect(controller.store.activeSession?.cacheKey.authorizationRevision == "public-policy-v7")
        await controller.store.disconnect(clearCachedScope: false)
    }

    @MainActor
    @Test
    func revokedAudienceErasesItsPrivateSnapshotAndCursor() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appending(path: "integrity-station-revocation-\(UUID().uuidString)", directoryHint: .isDirectory)
        defer { try? FileManager.default.removeItem(at: directory) }

        let baseURL = try #require(URL(string: "https://collector.invalid"))
        let audience = try #require(ReadAudience("organization:customer-a"))
        let session = try #require(
            ReadSession(
                baseURL: baseURL,
                principalID: "viewer-a",
                audience: audience,
                authorizationRevision: "grant-v1",
                token: "revoked-token"
            )
        )
        let payload = try JSONDecoder().decode(
            ObserversPayload.self,
            from: Data(#"{"schema":"2.0","audience":"organization:customer-a","observers":[{"id":"private-station"}]}"#.utf8)
        )
        let cache = SnapshotCache(directory: directory)
        try await cache.saveObservers(
            ObserversSnapshot(receivedAt: Date(), scope: session.cacheKey, serverTime: nil, payload: payload),
            for: session.cacheKey
        )
        try await cache.saveCursor("private-cursor", for: session.cacheKey)

        URLProtocolStub.handler = { request in
            let body = Data(#"{"ok":false,"error":"invalid read credential","code":401}"#.utf8)
            return (Self.response(for: request, status: 401), body)
        }
        defer { URLProtocolStub.handler = nil }
        let networkSession = Self.stubbedSession()
        let store = StationStore(
            feedClient: FeedClient(session: networkSession),
            eventStream: EventStream(session: networkSession),
            cache: cache
        )

        store.start(session: session, stationIDs: ["private-station"])
        try await Task.sleep(for: .milliseconds(150))

        #expect(store.authorizationLost)
        #expect(store.observers.isEmpty)
        #expect(store.events.isEmpty)
        let erasedSnapshot = try await cache.loadObservers(for: session.cacheKey)
        let erasedCursor = try await cache.loadCursor(for: session.cacheKey)
        #expect(erasedSnapshot == nil)
        #expect(erasedCursor == nil)
    }

    @Test
    func authenticatedHTTPIsRejectedBeforeNetworking() async throws {
        URLProtocolStub.handler = { request in
            #expect(request.value(forHTTPHeaderField: "Authorization") == nil)
            return (Self.response(for: request, status: 200), Data(#"{"ok":true,"data":{"schema":"2.0","revision":"public-v1","audiences":["public"]}}"#.utf8))
        }
        defer { URLProtocolStub.handler = nil }
        let url = try #require(URL(string: "http://collector.local"))
        let network = Self.stubbedSession()
        let client = FeedClient(session: network)
        do {
            _ = try await client.fetchAudiences(baseURL: url, token: "token")
            Issue.record("HTTP discovery accepted a token")
        } catch { #expect(error as? FeedError == .invalidBaseURL) }
        let privateSession = try #require(ReadSession(baseURL: url, principalID: "reader", audience: ReadAudience("organization:customer-a")!, authorizationRevision: "v1", token: "token"))
        do {
            _ = try await client.fetchObservers(session: privateSession)
            Issue.record("HTTP polling accepted a token")
        } catch { #expect(error as? FeedError == .invalidBaseURL) }
        do {
            for try await _ in try EventStream(session: network).updates(session: privateSession, lastEventID: nil) {}
            Issue.record("HTTP SSE accepted a token")
        } catch { #expect(error as? FeedError == .invalidBaseURL) }
        _ = try await client.fetchAudiences(baseURL: url, token: nil)
    }

    @MainActor @Test
    func storedHTTPCredentialCannotBeTransmitted() async throws {
        let credentials = MemoryConnectionStore()
        await credentials.saveToken("stored-token", forServer: "http://collector.local")
        let controller = AppController(secureStore: credentials, feedClient: FeedClient(session: Self.stubbedSession()))
        do {
            try await controller.connect(to: "http://collector.local")
            Issue.record("Stored credential allowed HTTP")
        } catch { #expect(error as? FeedError == .invalidBaseURL) }
    }

    @Test
    func authenticatedRedirectsStayOnOriginalHTTPSOrigin() throws {
        var request = URLRequest(url: URL(string: "https://collector.local/start")!)
        try ReadRequestHeaders.apply(token: "token", to: &request)
        let guardDelegate = CredentialRedirectGuard(request: request)
        #expect(guardDelegate.allows(URL(string: "https://collector.local/next")))
        #expect(guardDelegate.allows(URL(string: "https://collector.local:443/next")))
        for target in ["http://collector.local/next", "https://other.local/next", "https://collector.local:444/next", "https://user@collector.local/next"] {
            #expect(!guardDelegate.allows(URL(string: target)))
        }
    }

    private static func stubbedSession() -> URLSession {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [URLProtocolStub.self]
        return URLSession(configuration: configuration)
    }

    private static func response(for request: URLRequest, status: Int) -> HTTPURLResponse {
        HTTPURLResponse(
            url: request.url!,
            statusCode: status,
            httpVersion: "HTTP/1.1",
            headerFields: ["Content-Type": "application/json"]
        )!
    }
}

private final class URLProtocolStub: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest) throws -> (HTTPURLResponse, Data))?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        guard let handler = Self.handler else {
            client?.urlProtocol(self, didFailWithError: URLError(.unknown))
            return
        }
        do {
            let (response, data) = try handler(request)
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        } catch {
            client?.urlProtocol(self, didFailWithError: error)
        }
    }

    override func stopLoading() {}
}

private actor MemoryConnectionStore: SecureConnectionStoring {
    private var lastServer: String?
    private var tokens: [String: String] = [:]

    func lastServerURL() -> String? { lastServer }
    func saveLastServerURL(_ value: String) { lastServer = value }
    func token(forServer server: String) -> String? { tokens[server] }
    func saveToken(_ token: String, forServer server: String) { tokens[server] = token }
    func deleteToken(forServer server: String) { tokens.removeValue(forKey: server) }
}
