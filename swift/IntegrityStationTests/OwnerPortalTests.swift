import Foundation
import SwiftUI
import Testing
@testable import IntegrityStation

@Test func portalInstallationAndPKCERejectCrossInstallationCallbacks() throws {
    let base = try PortalInstallation("https://stations.example.invalid/stations")
    #expect(base.url.absoluteString == "https://stations.example.invalid/stations/")
    #expect(try PortalInstallation("https://CUSTOMER.example.invalid:443/").url.absoluteString == "https://customer.example.invalid/")
    for bad in ["http://example.invalid/", "https://user:password@example.invalid/", "https://example.invalid/s/?token=a", "https://example.invalid/a/../b", "https://example.invalid/%73/", "https://example.invalid//s/", "https://example.invalid/#secret"] {
        #expect(throws: FeedError.invalidBaseURL) { try PortalInstallation(bad) }
    }
    #expect(PortalAuthorization.challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk") == "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM")
    let authorization = try PortalAuthorization(installation: base)
    #expect(authorization.url.absoluteString.hasPrefix("https://stations.example.invalid/stations/accounts/native/authorize/?"))
    #expect(try authorization.code(from: portalCallback(authorization)) == String(repeating: "c", count: 43))
    #expect(throws: FeedError.invalidResponse) { try authorization.code(from: portalCallback(authorization, issuer: "https://other.example.invalid/")) }
    #expect(throws: FeedError.invalidResponse) { try authorization.code(from: portalCallback(authorization, state: "wrong")) }
    #expect(throws: FeedError.invalidResponse) { try authorization.code(from: URL(string: portalCallback(authorization).absoluteString + "&code=second")!) }
}

private func portalCallback(_ authorization: PortalAuthorization, issuer: String? = nil, state: String? = nil) -> URL {
    var parts = URLComponents(string: PortalAuthorization.callback)!
    parts.queryItems = [URLQueryItem(name: "code", value: String(repeating: "c", count: 43)), URLQueryItem(name: "state", value: state ?? authorization.state),
                       URLQueryItem(name: "iss", value: issuer ?? authorization.installation.url.absoluteString)]
    return parts.url!
}
private let portalSessionID = UUID(uuidString: "310B65F2-278B-4B29-9449-CE80E38A26A2")!
private let portalFleetID = "7c55f2c0-df88-4f5e-8e92-6d1395ba4bf9"
private func portalIdentity() -> [String: Any] {
    ["version": 1, "session_id": portalSessionID.uuidString, "user": ["id": "42", "name": "Alex", "email": "owner@example.invalid"],
     "fleets": [["id": portalFleetID, "kind": "organization", "name": "Example fleet", "role": "owner"]]]
}
private func portalToken() -> [String: Any] {
    ["access_token": String(repeating: "t", count: 43), "token_type": "Bearer", "expires_in": 86400,
     "session_id": portalSessionID.uuidString, "issuer": "https://portal.example.invalid/stations/", "scope": "stations:read"]
}

@Test func portalBoardDiagnosticsPreserveLargeCountersAndCollectorClock() throws {
    let data = Data(#"{"id":"310b65f2-278b-4b29-9449-ce80e38a26a2","status":"reporting","health":"warning","board_freshness":"receipt_only","conditions_known":true,"conditions":[],"temperature":0,"board":{"stale":false,"latest":{"received_at":"2026-09-30T12:00:00Z","sample_time":null}},"details":{"resources":{"spool_dropped_records":"18446744073709551615"}}}"#.utf8)
    let reading = try OwnerPortalClient.decoder().decode(PortalReading.self, from: data)
    #expect(reading.details["resources"]["spool_dropped_records"].display == "18446744073709551615")
    #expect(reading.temperature == 0)
    #expect(reading.freshness(at: WireDate.parse("2026-09-30T12:00:00Z"), elapsed: 1) == .receiptOnly)
    #expect(reading.freshness(at: WireDate.parse("2026-09-30T12:00:00Z"), elapsed: 661) == .stale)
}

@Suite(.serialized) struct OwnerPortalNetworkingTests {
    private func client() -> OwnerPortalClient {
        let config = URLSessionConfiguration.ephemeral; config.protocolClasses = [PortalTestProtocol.self]
        return OwnerPortalClient(session: URLSession(configuration: config))
    }
    @Test @MainActor func browserExchangeScopesCredentialAndReconcilesAfterRestart() async throws {
        let installation = try PortalInstallation("https://portal.example.invalid/stations/")
        let secure = PortalMemoryStore(), client = client()
        nonisolated(unsafe) var requests: [URLRequest] = []
        PortalTestProtocol.handler = { request in
            requests.append(request)
            if request.url!.path.hasSuffix("token") { return (200, portalToken()) }
            if request.url!.path.hasSuffix("revoke") { return (200, ["revoked": true]) }
            #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer " + String(repeating: "t", count: 43))
            return (200, portalIdentity())
        }
        defer { PortalTestProtocol.handler = nil }
        let controller = OwnerPortalController(secure: secure, client: client)
        await controller.connect(to: installation.url.absoluteString) { url in
            let parts = URLComponents(url: url, resolvingAgainstBaseURL: false)!
            #expect(parts.path == "/stations/accounts/native/authorize/")
            #expect(parts.queryItems?.first { $0.name == "code_challenge_method" }?.value == "S256")
            var callback = URLComponents(string: PortalAuthorization.callback)!
            callback.queryItems = [URLQueryItem(name: "code", value: String(repeating: "c", count: 43)),
                                   URLQueryItem(name: "state", value: parts.queryItems!.first { $0.name == "state" }!.value),
                                   URLQueryItem(name: "iss", value: installation.url.absoluteString)]
            return callback.url!
        }
        #expect(controller.identity?.user.id == "42")
        #expect(controller.identity?.fleets.first?.name == "Example fleet")
        #expect(controller.error == nil)
        #expect(requests.allSatisfy { $0.url!.path.hasPrefix("/stations/api/v1/") && !$0.httpShouldHandleCookies })
        let tokenRequest = try #require(requests.first)
        #expect(tokenRequest.httpMethod == "POST" && tokenRequest.value(forHTTPHeaderField: "Authorization") == nil)
        // A fresh controller has no cached fleet data; it rebuilds from the same server account.
        let restarted = OwnerPortalController(secure: secure, client: client)
        #expect(restarted.identity == nil)
        await restarted.restore()
        #expect(restarted.identity?.fleets.first?.id.uuidString.lowercased() == portalFleetID)
        restarted.suspend()
        #expect(restarted.identity == nil && restarted.connection != nil)
        await restarted.refresh()
        #expect(restarted.identity?.user.id == "42")
        await restarted.signOut()
        #expect(restarted.identity == nil && restarted.connection == nil)
        #expect(try await secure.token(forServer: installation.url.absoluteString) == nil)
        #expect(requests.last?.url?.absoluteString == "https://portal.example.invalid/stations/api/v1/native/revoke/")
    }

    #if os(macOS)
    @Test @MainActor func portalRootKeepsApprovedAccountAndClearsHiddenData() async throws {
        let portal = OwnerPortalController(secure: PortalMemoryStore(), client: client())
        PortalTestProtocol.handler = { request in
            request.url!.path.hasSuffix("token") ? (200, portalToken()) : (200, portalIdentity())
        }
        defer { PortalTestProtocol.handler = nil }
        let window = NSWindow(contentRect: NSRect(x: -10000, y: -10000, width: 850, height: 700), styleMask: [.titled], backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false
        let hosting = NSHostingView(rootView: OwnerPortalView(portal: portal).environment(\.scenePhase, .active).environment(\.colorScheme, .dark))
        window.contentView = hosting; window.orderFront(nil)
        defer { window.contentView = nil; window.close() }
        hosting.layoutSubtreeIfNeeded()
        await portal.connect(to: "https://portal.example.invalid/stations/") { url in
            var parts = URLComponents(string: PortalAuthorization.callback)!
            parts.queryItems = [URLQueryItem(name: "code", value: String(repeating: "c", count: 43)),
                                URLQueryItem(name: "iss", value: "https://portal.example.invalid/stations/"),
                                URLComponents(url: url, resolvingAgainstBaseURL: false)!.queryItems!.first { $0.name == "state" }!]
            return parts.url!
        }
        #expect(try await eventually { portal.identity?.user.id == "42" && portal.connection != nil })
        hosting.layoutSubtreeIfNeeded()
        let bitmap = try #require(hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds))
        hosting.cacheDisplay(in: hosting.bounds, to: bitmap)
        try #require(bitmap.representation(using: .png, properties: [:])).write(to: FileManager.default.temporaryDirectory.appending(path: "navlistener-owner-fleets-review.png"))
        hosting.rootView = OwnerPortalView(portal: portal).environment(\.scenePhase, .background).environment(\.colorScheme, .dark)
        // Leaving the active phase suspends the portal, which drops the
        // identity while keeping the connection.
        #expect(try await eventually { portal.identity == nil && portal.connection != nil })
    }

    @Test @MainActor func portalStationRendersApprovedInventoryAndRecoveredHistory() async throws {
        let base = try PortalInstallation("https://portal.example.invalid/stations/")
        let connection = PortalConnection(installation: base, token: String(repeating: "t", count: 43), sessionID: portalSessionID, accountID: "42", expiresAt: .now.addingTimeInterval(3600))
        let secure = PortalMemoryStore()
        try await secure.saveToken(String(decoding: JSONEncoder().encode(connection), as: UTF8.self), forServer: base.url.absoluteString)
        await secure.saveLastServerURL(base.url.absoluteString)
        let identifier = "581d7849-b16d-41b4-965f-6c37e4b9ca59"
        let inventory: [String: Any] = ["id": identifier, "collector_id": portalFleetID, "collector_name": "Research collector",
            "observer_id": "receiver: 001/東京", "label": "Harbor rooftop", "site": "Research building", "enabled": true,
            "model": "GNSS receiver", "board_revision": "Demo", "receiver_model": "Multi-constellation GNSS", "serial": "DEMO-001",
            "antenna": "Active GNSS antenna", "components": "MCP9808 temperature · HDC2080 humidity · BMP384 pressure",
            "facts_source": "Approved demonstration inventory"]
        let clock = "2026-09-30T12:00:00Z"
        let recorder = PortalRequestRecorder()
        PortalTestProtocol.handler = { request in
            let path = request.url!.path; recorder.add(path)
            if path.hasSuffix("me") { return (200, portalIdentity()) }
            if path.contains("/inventory/") { return (200, ["station": inventory]) }
            if path.hasSuffix("history") {
                var points: [[String: Any]] = []
                for index in 0..<30 {
                    let value: Any = (12...15).contains(index) ? NSNull() : 24 + sin(Double(index) / 3)
                    points.append(["time": WireDate.parse("2026-09-30T11:00:00Z")!.addingTimeInterval(Double(index) * 60).ISO8601Format(),
                                   "value": value, "session": index < 22 ? "a" : "b", "hardware_trust": "test"])
                }
                return (200, ["metric": "temperature", "label": "Board temperature", "unit": "°C", "access_revision": "access-1", "points": points,
                    "page": ["since": "2026-09-30T11:00:00Z", "until": clock, "effective_since": "2026-09-30T11:00:00Z", "visible_since": "2026-09-30T10:00:00Z",
                             "revision": "history-1", "history_limited": false, "has_more": false, "pagination_limited": false]])
            }
            let environment = ["mcp9808_c": 24.4, "humidity_percent": 46.8, "pressure_pa": 101245.0]
            return (200, ["collector_time": clock, "station": ["id": identifier, "status": "reporting", "health": "warning", "board_freshness": "receipt_only",
                "conditions_known": true, "conditions": [["type": "antenna_fault", "severity": 1, "message": "Receiver antenna needs attention"]],
                "last_seen_s": 1, "temperature": 24.4, "humidity": 46.8, "pressure": 101245, "hardware_trust": "test",
                "board": ["stale": false, "latest": ["received_at": clock]], "details": ["environment": environment]]])
        }
        defer { PortalTestProtocol.handler = nil }
        let portal = OwnerPortalController(secure: secure, client: client())
        await portal.restore()
        let fleet = try #require(portal.identity?.fleets.first)
        let station = try OwnerPortalClient.decoder().decode(PortalStation.self, from: JSONSerialization.data(withJSONObject: inventory))
        let view = PortalStationView(fleet: fleet, station: station).environment(portal).environment(\.colorScheme, .dark)
        let hosting = NSHostingView(rootView: view)
        let window = NSWindow(contentRect: NSRect(x: -10000, y: -10000, width: 900, height: 1600), styleMask: [.titled], backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false; window.contentView = hosting; window.orderFront(nil)
        defer { window.contentView = nil; window.close() }
        #expect(try await eventually { recorder.contains("history") && recorder.contains(identifier) })
        hosting.layoutSubtreeIfNeeded()
        let bitmap = try #require(hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds))
        hosting.cacheDisplay(in: hosting.bounds, to: bitmap)
        let png = try #require(bitmap.representation(using: .png, properties: [:]))
        try png.write(to: FileManager.default.temporaryDirectory.appending(path: "navlistener-owner-portal-review.png"))
        #expect(bitmap.pixelsWide >= 900)
        portal.suspend()
        #expect(portal.identity == nil)
    }
    #endif

    @Test @MainActor func revokedSessionAndMismatchedAccountDoNotRestorePrivateData() async throws {
        let base = try PortalInstallation("https://portal.example.invalid/stations/")
        let saved = PortalConnection(installation: base, token: String(repeating: "t", count: 43), sessionID: portalSessionID, accountID: "42", expiresAt: .now.addingTimeInterval(3600))
        let store = PortalMemoryStore()
        try await store.saveToken(String(decoding: JSONEncoder().encode(saved), as: UTF8.self), forServer: base.url.absoluteString)
        await store.saveLastServerURL(base.url.absoluteString)
        PortalTestProtocol.handler = { _ in (401, ["error": "invalid_token"]) }
        defer { PortalTestProtocol.handler = nil }
        let controller = OwnerPortalController(secure: store, client: client())
        await controller.restore()
        #expect(controller.connection == nil && controller.identity == nil)
        #expect(try await store.token(forServer: base.url.absoluteString) == nil)
        PortalTestProtocol.handler = { _ in
            var data = portalIdentity(); data["user"] = ["id": "other", "name": "Other", "email": "other@example.invalid"]; return (200, data)
        }
        await #expect(throws: FeedError.invalidResponse) { try await client().identity(saved) }
    }

    @Test @MainActor func delayedKeychainSaveCannotResurrectSignedOutConnection() async throws {
        let secure = PortalMemoryStore(gated: true), client = client()
        PortalTestProtocol.handler = { request in
            request.url!.path.hasSuffix("token") ? (200, portalToken()) : request.url!.path.hasSuffix("revoke") ? (200, ["revoked": true]) : (200, portalIdentity())
        }
        defer { PortalTestProtocol.handler = nil }
        let controller = OwnerPortalController(secure: secure, client: client)
        let connecting = Task {
            await controller.connect(to: "https://portal.example.invalid/stations/") { url in
                let source = URLComponents(url: url, resolvingAgainstBaseURL: false)!
                var reply = URLComponents(string: PortalAuthorization.callback)!
                reply.queryItems = [URLQueryItem(name: "code", value: String(repeating: "c", count: 43)), URLQueryItem(name: "iss", value: "https://portal.example.invalid/stations/"), source.queryItems!.first { $0.name == "state" }!]
                return reply.url!
            }
        }
        try await secure.waitForSave()
        await controller.signOut()
        await secure.releaseSave()
        await connecting.value
        #expect(controller.connection == nil && controller.identity == nil)
        #expect(try await secure.token(forServer: "https://portal.example.invalid/stations/") == nil)
    }
}

private actor PortalMemoryStore: SecureConnectionStoring {
    private var server: String?
    private var tokens: [String: String] = [:]
    private let gated: Bool
    private var gate: CheckedContinuation<Void, Never>?
    private var observer: CheckedContinuation<Void, Never>?
    init(gated: Bool = false) { self.gated = gated }
    func lastServerURL() -> String? { server }
    func saveLastServerURL(_ value: String) { server = value }
    func token(forServer server: String) throws -> String? { tokens[server] }
    func saveToken(_ token: String, forServer server: String) async {
        if gated { await withCheckedContinuation { gate = $0; observer?.resume(); observer = nil } }
        tokens[server] = token
    }
    func deleteToken(forServer server: String) { tokens.removeValue(forKey: server) }
    func waitForSave() async throws {
        for _ in 0..<500 { if gate != nil { return }; try await Task.sleep(for: .milliseconds(10)) }
        throw FeedError.invalidResponse
    }
    func releaseSave() { gate?.resume(); gate = nil }
}
private final class PortalTestProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handler: ((URLRequest) -> (Int, [String: Any]))?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard let handler = Self.handler else {
            client?.urlProtocol(self, didFailWithError: URLError(.cancelled)); return
        }
        let (status, json) = handler(request)
        let data = try! JSONSerialization.data(withJSONObject: json)
        client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil,
                        headerFields: ["Content-Type": "application/json"])!, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: data); client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

private final class PortalRequestRecorder: @unchecked Sendable {
    private let lock = NSLock()
    private var paths: [String] = []
    func add(_ path: String) { lock.lock(); defer { lock.unlock() }; paths.append(path) }
    func contains(_ suffix: String) -> Bool { lock.lock(); defer { lock.unlock() }; return paths.contains { $0.hasSuffix(suffix) } }
}
