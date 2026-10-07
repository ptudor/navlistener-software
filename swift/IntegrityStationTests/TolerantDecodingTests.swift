import Foundation
import Testing
@testable import IntegrityStation

/// A newer collector: schema 2.1 everywhere, a fourth severity, an audience
/// kind this build does not know, a public sub-audience, and a public row
/// carrying a redacted board subset. Every one of these is additive.
private enum NewerCollector {
    static func response(_ request: URLRequest) -> Data {
        let authenticated = request.value(forHTTPHeaderField: "Authorization") != nil
        let path = request.url!.path
        if path.hasSuffix("/audiences") {
            return Data((authenticated
                ? #"{"ok":true,"data":{"schema":"2.1","principal":"reader","revision":"private-v1","audiences":["public","organization:org","federation:x"]}}"#
                : #"{"ok":true,"data":{"schema":"2.1","revision":"public-v1","audiences":["public","public:open-data"]}}"#).utf8)
        }
        let audience = request.value(forHTTPHeaderField: "X-GNSS-Audience") ?? "public"
        let condition = #"{"id":1,"time":"2026-10-07T00:00:00Z","sv":"roof","type":"jamming_detected","new_value":"jammed","severity":3}"#
        if path.hasSuffix("/gnss/events") {
            return Data("id: 1\nevent: gnss\ndata: \(condition)\n\nevent: status\ndata: {\"status\":\"connected\"}\n\n".utf8)
        }
        if path.hasSuffix("/conditions") {
            return Data("{\"ok\":true,\"data\":{\"schema\":\"2.1\",\"audience\":\"\(audience)\",\"complete\":true,\"epoch\":\"a\",\"cursor\":1,\"events\":[\(condition)]}}".utf8)
        }
        return Data("{\"ok\":true,\"data\":{\"schema\":\"2.1\",\"audience\":\"\(audience)\",\"observers\":[{\"id\":\"roof\",\"last_seen_s\":1,\"board\":{},\"integrity\":{\"state\":\"assured\"}}],\"events\":[\(condition)]}}".utf8)
    }
}

/// Additive server changes (a new severity, a new audience kind, a minor
/// schema bump, a redacted public subset) must never fail a whole payload.
/// The strict checks that remain are the ones that carry meaning: the
/// audience echo, duplicate or empty ids, and the selected audience itself.
@Suite(.serialized)
struct TolerantDecodingTests {
    private func event(severity: String) throws -> GNSSAPIEvent {
        try JSONDecoder().decode(GNSSAPIEvent.self, from: Data(
            "{\"id\":7,\"sv\":\"roof\",\"type\":\"jamming_detected\",\"new_value\":\"jammed\",\"severity\":\(severity)}".utf8))
    }

    @Test func unknownSeverityDecodesAndRanksAtLeastWarning() throws {
        #expect(try event(severity: "3").severity == .critical)
        #expect(try event(severity: "3").severityValue == 3)
        #expect(try event(severity: "7").severity == .critical)
        #expect(try event(severity: "-1").severity == .warning)
        #expect(try event(severity: "-1").severityValue == -1)
        #expect(try event(severity: "2").severity == .critical)
        #expect(try event(severity: "1").severity == .warning)
        #expect(try event(severity: "0").severity == .info)
        #expect(try event(severity: "null").severity == nil)
        // A severity that is not an integer leaves the row intact with no rank.
        let odd = try event(severity: "\"high\"")
        #expect(odd.id == 7 && odd.severity == nil && odd.severityValue == nil)
        // The raw value survives a round trip.
        let restored = try JSONDecoder().decode(GNSSAPIEvent.self, from: JSONEncoder().encode(try event(severity: "3")))
        #expect(restored.severityValue == 3 && restored.severity == .critical)
        // Fields the model declares with a type still fail on the wrong type.
        #expect(throws: DecodingError.self) {
            try JSONDecoder().decode(GNSSAPIEvent.self, from: Data(#"{"id":"not-a-number","sv":"roof","type":"jamming_detected","severity":1}"#.utf8))
        }
    }

    @Test func conditionsWithUnknownSeverityColourTheStation() throws {
        let payload = try JSONDecoder().decode(ConditionsPayload.self, from: Data(
            #"{"schema":"2.0","audience":"public","complete":true,"epoch":"a","cursor":5,"events":[{"id":5,"time":"2026-10-07T00:00:00Z","sv":"roof","type":"jamming_detected","new_value":"jammed","severity":3}]}"#.utf8))
        var state = ConditionState()
        #expect(try state.install(payload))
        let severities = state.active.compactMap(\.severity)
        #expect(severities == [.critical])
        #expect(HealthState.station(lastSeenSeconds: 1, activeEventSeverities: severities) != .ok)
        #expect(HealthState.station(lastSeenSeconds: 1, activeEventSeverities: severities) == .critical)
    }

    @Test func streamDeliversAnEventWithAnUnknownSeverity() async throws {
        let host = "tolerant-stream.invalid"
        let body = Data((
            "id: 1\nevent: gnss\ndata: {\"id\":1,\"sv\":\"roof\",\"type\":\"jamming_detected\",\"new_value\":\"jammed\",\"severity\":3}\n\n"
            + "id: 2\nevent: gnss\ndata: {\"id\":2,\"sv\":\"roof\",\"type\":\"jamming_detected\",\"new_value\":\"ok\",\"severity\":0}\n\n").utf8)
        StubCollectorProtocol.register(host: host) { _ in body }
        defer { StubCollectorProtocol.unregister(host: host) }
        let network = StubCollectorProtocol.session()
        defer { network.invalidateAndCancel() }
        let readSession = try #require(ReadSession(baseURL: URL(string: "https://\(host)")!,
                                                   principalID: nil, audience: .publicAudience,
                                                   authorizationRevision: "a", token: nil))
        var cursors: [String?] = [], ranks: [EventSeverity?] = [], raw: [Int?] = []
        for try await update in try EventStream(session: network).updates(session: readSession, lastEventID: nil) {
            if case .event(let event, let cursor) = update {
                cursors.append(cursor); ranks.append(event.severity); raw.append(event.severityValue)
            }
            if cursors.count == 2 { break }
        }
        #expect(cursors == ["1", "2"])
        #expect(ranks == [.critical, .info])
        #expect(raw == [3, 0])
    }

    @Test func discoveryDropsAudiencesItCannotParseButStillRequiresPublic() throws {
        let decoded = try JSONDecoder().decode(AudienceDiscoveryPayload.self, from: Data(
            #"{"schema":"2.0","principal":"viewer-a","revision":"grant-v1","audiences":["public","federation:x","organization:a","organization:","collection:bad id"]}"#.utf8))
        let discovery = try #require(decoded.validated())
        #expect(discovery.audiences == [ReadAudience("organization:a")!, .publicAudience])
        #expect(discovery.principalID == "viewer-a")
        let withoutPublic = try JSONDecoder().decode(AudienceDiscoveryPayload.self, from: Data(
            #"{"schema":"2.0","principal":"viewer-a","revision":"grant-v1","audiences":["federation:x","organization:a"]}"#.utf8))
        #expect(withoutPublic.validated() == nil)
        // Everywhere else an audience decodes strictly: a cache key with an
        // unknown kind is corrupt, not a grant.
        #expect(throws: DecodingError.self) {
            try JSONDecoder().decode(ReadAudience.self, from: Data(#""federation:x""#.utf8))
        }
    }

    @Test func minorSchemaVersionsAreAcceptedAndOtherMajorsAreNot() throws {
        for schema in ["2.0", "2.1", "2.10", "2.99"] { #expect(WireSchema.isSupported(schema), "\(schema)") }
        for schema in ["1.0", "3.0", "2", "2.", ".0", "2.x", "v2.0", " 2.0", "2.0.1", "", "２.0"] {
            #expect(!WireSchema.isSupported(schema), "\(schema)")
        }
        #expect(!WireSchema.isSupported(nil))
        let minor = try JSONDecoder().decode(AudienceDiscoveryPayload.self, from: Data(
            #"{"schema":"2.1","revision":"public-v1","audiences":["public"]}"#.utf8))
        #expect(minor.validated() != nil)
        let major = try JSONDecoder().decode(AudienceDiscoveryPayload.self, from: Data(
            #"{"schema":"3.0","revision":"public-v1","audiences":["public"]}"#.utf8))
        #expect(major.validated() == nil)
    }

    @MainActor @Test func publicRowsWithRedactedPrivateDetailStillRender() throws {
        let payload = try JSONDecoder().decode(ObserversPayload.self, from: Data(
            #"{"schema":"2.1","audience":"public","observers":[{"id":"roof","last_seen_s":4,"board":{},"integrity":{"state":"assured"}},{"id":"ridge","last_seen_s":9}]}"#.utf8))
        try payload.validate()
        let key = AudienceCacheKey(server: "https://collector.invalid", principal: AudienceCacheKey.anonymousPrincipal,
                                   audience: .publicAudience, authorizationRevision: "public")
        let store = StationStore()
        try store.apply(ObserversSnapshot(receivedAt: Date(), scope: key, serverTime: nil, payload: payload), cached: false)
        #expect(store.observers.map(\.id) == ["roof", "ridge"])
        #expect(store.observers.allSatisfy { $0.board == nil && $0.integrity == nil })
        #expect(try #require(store.currentLastSeenAge(for: "roof")) >= 4)
        // Private rows keep their detail.
        let privatePayload = try JSONDecoder().decode(ObserversPayload.self, from: Data(
            #"{"schema":"2.1","audience":"organization:a","observers":[{"id":"roof","integrity":{"state":"assured"}}]}"#.utf8))
        #expect(privatePayload.redacted().observers?.first?.integrity?.state == "assured")
        // Duplicate and empty ids are still rejected.
        for rows in [#"[{"id":"dup"},{"id":"dup"}]"#, #"[{"id":""}]"#] {
            let bad = try JSONDecoder().decode(ObserversPayload.self, from: Data(
                "{\"schema\":\"2.0\",\"audience\":\"public\",\"observers\":\(rows)}".utf8))
            #expect(throws: FeedError.invalidResponse) { try bad.validate() }
        }
    }

    @MainActor @Test(arguments: [true, false])
    func newerCollectorStillRendersTheStationList(privateAudience: Bool) async throws {
        let host = "newer.invalid"
        StubCollectorProtocol.register(host: host) { NewerCollector.response($0) }
        defer { StubCollectorProtocol.unregister(host: host) }
        let session = StubCollectorProtocol.session()
        let suite = "TolerantDecodingTests.\(UUID())"
        let defaults = try #require(UserDefaults(suiteName: suite))
        let directory = FileManager.default.temporaryDirectory.appending(path: suite)
        defer { try? FileManager.default.removeItem(at: directory); defaults.removePersistentDomain(forName: suite) }
        let store = StationStore(feedClient: FeedClient(session: session), eventStream: EventStream(session: session),
                                 cache: SnapshotCache(directory: directory))
        let app = AppController(store: store, settings: AppSettings(defaults: defaults),
                                secureStore: RaceCredentials(), feedClient: FeedClient(session: session))

        try await app.connect(to: "https://\(host)", readToken: privateAudience ? "token" : nil)
        let active = try #require(store.activeSession)
        #expect(active.audience.isPrivate == privateAudience)
        // The unknown kind and the public sub-audience are dropped; the known
        // grants remain selectable, sorted by raw value.
        #expect(app.availableAudiences == (privateAudience
            ? [ReadAudience("organization:org")!, .publicAudience]
            : [.publicAudience]))
        for _ in 0..<500 where store.observers.isEmpty || store.health(for: "roof") == .unknown
            || !store.isEventStreamConnected || store.isRefreshing {
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(store.isEventStreamConnected)
        #expect(store.observers.map(\.id) == ["roof"])
        #expect(store.observers.first?.board == nil || privateAudience)
        #expect(store.observers.first?.integrity == nil || privateAudience)
        #expect(store.health(for: "roof") == .critical)
        #expect(store.activeEvents(for: "roof").first?.severityValue == 3)
        #expect(store.errorMessage == nil)
        #expect(store.eventStreamMessage == nil)
        await store.disconnect(clearCachedScope: true)
    }
}
