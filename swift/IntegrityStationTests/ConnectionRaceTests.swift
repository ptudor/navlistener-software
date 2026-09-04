import Foundation
import Testing
@testable import IntegrityStation

actor ConnectionBarrier {
    private var entered = false
    private var used = false
    private var waiter: CheckedContinuation<Void, Never>?
    func hold() async { entered = true; await withCheckedContinuation { waiter = $0 } }
    func holdOnce() async { guard !used else { return }; used = true; await hold() }
    func waitUntilEntered() async { while !entered { await Task.yield() } }
    func release() { waiter?.resume(); waiter = nil }
}

actor RaceCredentials: SecureConnectionStoring {
    var lastServer: String?
    var tokens: [String: String] = [:]
    var deletionFails = false
    var startupBarrier: ConnectionBarrier?
    var saveBarrier: ConnectionBarrier?
    func lastServerURL() async -> String? { if let startupBarrier { await startupBarrier.hold() }; return lastServer }
    func saveLastServerURL(_ value: String) { lastServer = value }
    func token(forServer server: String) -> String? { tokens[server] }
    func saveToken(_ token: String, forServer server: String) async { if let saveBarrier { await saveBarrier.hold() }; tokens[server] = token }
    func deleteToken(forServer server: String) throws {
        if deletionFails { throw CocoaError(.fileWriteNoPermission) }
        tokens.removeValue(forKey: server)
    }
    func failDeletion() { deletionFails = true }
    func holdStartup(_ barrier: ConnectionBarrier) { startupBarrier = barrier }
    func holdSave(_ barrier: ConnectionBarrier) { saveBarrier = barrier }
}

final class AsyncConnectionProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest) async throws -> Data)?
    private var loadingTask: Task<Void, Never>?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        loadingTask = Task {
            do {
                guard let handler = Self.handler else { throw URLError(.badServerResponse) }
                let data = try await handler(request)
                let response = HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: nil)!
                client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
                client?.urlProtocol(self, didLoad: data)
                client?.urlProtocolDidFinishLoading(self)
            } catch { client?.urlProtocol(self, didFailWithError: error) }
        }
    }
    override func stopLoading() { loadingTask?.cancel() }
}

@Suite(.serialized) @MainActor
struct ConnectionRaceTests {
    nonisolated static func response(_ request: URLRequest) -> Data {
        let authenticated = request.value(forHTTPHeaderField: "Authorization") != nil
        if request.url!.path.hasSuffix("/audiences") {
            return Data((authenticated
                ? #"{"ok":true,"data":{"schema":"2.0","principal":"reader","revision":"private-v1","audiences":["public","organization:org"]}}"#
                : #"{"ok":true,"data":{"schema":"2.0","revision":"public-v1","audiences":["public"]}}"#).utf8)
        }
        let audience = request.value(forHTTPHeaderField: "X-GNSS-Audience") ?? "public"
        if request.url!.path.hasSuffix("/gnss/events") { return Data("event: status\ndata: {\"status\":\"connected\"}\n\n".utf8) }
        return Data("{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"audience\":\"\(audience)\",\"observers\":[],\"events\":[]}}".utf8)
    }

    func controller(_ credentials: RaceCredentials = RaceCredentials()) throws -> AppController {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [AsyncConnectionProtocol.self]
        let session = URLSession(configuration: config)
        let defaults = try #require(UserDefaults(suiteName: "ConnectionRaceTests.\(UUID())"))
        let cache = SnapshotCache(directory: FileManager.default.temporaryDirectory.appending(path: "ConnectionRaceTests.\(UUID())"))
        return AppController(store: StationStore(feedClient: FeedClient(session: session), eventStream: EventStream(session: session), cache: cache), settings: AppSettings(defaults: defaults), secureStore: credentials, feedClient: FeedClient(session: session))
    }

    @Test func logoutWinsOverDelayedAuthenticatedDiscovery() async throws {
        let gate = ConnectionBarrier()
        AsyncConnectionProtocol.handler = { request in
            if request.url!.path.hasSuffix("/audiences"), request.value(forHTTPHeaderField: "Authorization") != nil { await gate.hold() }
            return Self.response(request)
        }
        defer { AsyncConnectionProtocol.handler = nil }
        let credentials = RaceCredentials()
        let app = try controller(credentials)
        let pending = Task { try await app.connect(to: "https://first.invalid", readToken: "old-token") }
        await gate.waitUntilEntered()
        await app.logout()
        await gate.release()
        _ = await pending.result
        #expect(app.store.activeSession?.token == nil)
        #expect(app.principalID == AudienceCacheKey.anonymousPrincipal)
        #expect(await credentials.token(forServer: "https://first.invalid") == nil)
        await app.store.disconnect(clearCachedScope: true)
    }

    @Test func newerServerWinsWhenConnectionsCompleteInReverse() async throws {
        let gate = ConnectionBarrier()
        AsyncConnectionProtocol.handler = { request in
            if request.url!.host == "first.invalid", request.value(forHTTPHeaderField: "Authorization") != nil { await gate.hold() }
            return Self.response(request)
        }
        defer { AsyncConnectionProtocol.handler = nil }
        let credentials = RaceCredentials(); let app = try controller(credentials)
        let old = Task { try await app.connect(to: "https://first.invalid", readToken: "old-token") }
        await gate.waitUntilEntered()
        try await app.connect(to: "https://second.invalid", readToken: "new-token")
        await gate.release(); _ = await old.result
        #expect(app.serverURL?.host == "second.invalid")
        #expect(app.store.activeSession?.token == "new-token")
        #expect(await credentials.token(forServer: "https://first.invalid") == nil)
        await app.store.disconnect(clearCachedScope: true)
    }

    @Test func logoutDrainsAnAlreadyPendingCredentialSave() async throws {
        AsyncConnectionProtocol.handler = { Self.response($0) }
        defer { AsyncConnectionProtocol.handler = nil }
        let credentials = RaceCredentials()
        let gate = ConnectionBarrier(); await credentials.holdSave(gate)
        let app = try controller(credentials)
        let pending = Task { try await app.connect(to: "https://first.invalid", readToken: "old-token") }
        await gate.waitUntilEntered()
        let logout = Task { await app.logout() }
        while app.isConnecting { await Task.yield() }
        await gate.release()
        _ = await pending.result; await logout.value
        #expect(await credentials.token(forServer: "https://first.invalid") == nil)
        #expect(app.store.activeSession?.token == nil)
        await app.store.disconnect(clearCachedScope: true)
    }

    @Test func deletionFailureStillRemovesInMemoryAuthority() async throws {
        AsyncConnectionProtocol.handler = { Self.response($0) }
        defer { AsyncConnectionProtocol.handler = nil }
        let credentials = RaceCredentials(); let app = try controller(credentials)
        try await app.connect(to: "https://first.invalid", readToken: "token")
        await credentials.failDeletion()
        await app.logout()
        #expect(app.store.activeSession?.token == nil)
        #expect(!app.hasStoredCredential)
        #expect(app.credentialDeletionError != nil)
        #expect(app.connectionError != nil)
        try await app.connect(to: "https://first.invalid")
        #expect(app.store.activeSession?.token == nil)
        await app.store.disconnect(clearCachedScope: true)
    }

    @Test func startupCannotOverrideManualConnection() async throws {
        AsyncConnectionProtocol.handler = { Self.response($0) }
        defer { AsyncConnectionProtocol.handler = nil }
        let credentials = RaceCredentials(); await credentials.saveLastServerURL("https://startup.invalid")
        let gate = ConnectionBarrier(); await credentials.holdStartup(gate)
        let app = try controller(credentials)
        let startup = Task { await app.start() }
        await gate.waitUntilEntered()
        try await app.connect(to: "https://manual.invalid", readToken: "manual-token")
        await gate.release(); await startup.value
        #expect(app.serverURL?.host == "manual.invalid")
        await app.store.disconnect(clearCachedScope: true)
    }

    @Test func audienceSelectionInvalidatesPendingReconnect() async throws {
        AsyncConnectionProtocol.handler = { Self.response($0) }
        let app = try controller()
        try await app.connect(to: "https://first.invalid", readToken: "token")
        let gate = ConnectionBarrier()
        AsyncConnectionProtocol.handler = { request in
            if request.url!.path.hasSuffix("/audiences"), request.value(forHTTPHeaderField: "Authorization") == "Bearer replacement" { await gate.hold() }
            return Self.response(request)
        }
        defer { AsyncConnectionProtocol.handler = nil }
        let pending = Task { try await app.connect(to: "https://first.invalid", readToken: "replacement") }
        await gate.waitUntilEntered()
        try await app.selectAudience(.publicAudience)
        await gate.release(); _ = await pending.result
        #expect(app.selectedAudience == .publicAudience)
        #expect(app.store.activeSession?.token == nil)
        await app.store.disconnect(clearCachedScope: true)
    }
}
