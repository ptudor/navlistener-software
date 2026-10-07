import Foundation
import SwiftUI
import Testing
@testable import IntegrityStation

/// A public collector with one active warning condition for `roof`.
private final class PhaseCollector: @unchecked Sendable {
    private let lock = NSLock()
    private var requests = 0
    var conditionRequests: Int { lock.withLock { requests } }

    func response(_ request: URLRequest) -> Data {
        let path = request.url!.path
        if path.hasSuffix("/audiences") {
            return Data(#"{"ok":true,"data":{"schema":"2.0","revision":"public-v1","audiences":["public"]}}"#.utf8)
        }
        let condition = #"{"id":1,"time":"2026-10-07T00:00:00Z","sv":"roof","type":"jamming_detected","new_value":"jammed","severity":1}"#
        if path.hasSuffix("/gnss/events") {
            return Data("event: status\ndata: {\"status\":\"connected\"}\n\n".utf8)
        }
        if path.hasSuffix("/conditions") {
            lock.withLock { requests += 1 }
            return Data("{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"audience\":\"public\",\"complete\":true,\"epoch\":\"a\",\"cursor\":1,\"events\":[\(condition)]}}".utf8)
        }
        return Data("{\"ok\":true,\"data\":{\"schema\":\"2.0\",\"audience\":\"public\",\"observers\":[{\"id\":\"roof\",\"last_seen_s\":1}],\"events\":[\(condition)]}}".utf8)
    }
}

@Suite(.serialized) @MainActor
struct ScenePhaseTests {
    @Test func onlyTheBackgroundLosesForeground() {
        #expect(ScenePhase.active.keepsForeground)
        #expect(ScenePhase.inactive.keepsForeground)
        #expect(!ScenePhase.background.keepsForeground)
    }

    /// A phase change must not turn known condition health into unknown; the
    /// refresh on return to the foreground reconciles instead, once.
    @Test func phaseChangesKeepConditionHealthKnown() async throws {
        let host = "phase.invalid"
        let collector = PhaseCollector()
        StubCollectorProtocol.register(host: host) { collector.response($0) }
        defer { StubCollectorProtocol.unregister(host: host) }
        let session = StubCollectorProtocol.session()
        let directory = FileManager.default.temporaryDirectory.appending(path: "ScenePhaseTests.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory) }
        let store = StationStore(feedClient: FeedClient(session: session), eventStream: EventStream(session: session),
                                 cache: SnapshotCache(directory: directory))
        let readSession = try #require(ReadSession(baseURL: URL(string: "https://\(host)")!, principalID: nil,
                                                   audience: .publicAudience, authorizationRevision: "public-v1", token: nil))
        store.setForeground(true)
        store.start(session: readSession, stationIDs: ["roof"])
        for _ in 0..<500 where store.health(for: "roof") != .warning || !store.isEventStreamConnected || store.isRefreshing {
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(store.health(for: "roof") == .warning)
        #expect(store.isEventStreamConnected)
        try await Task.sleep(for: .milliseconds(100))
        let reconciled = collector.conditionRequests

        store.setForeground(false)
        #expect(store.health(for: "roof") == .warning)
        store.setForeground(false)
        #expect(store.health(for: "roof") == .warning)

        store.setForeground(true)
        #expect(store.health(for: "roof") == .warning)
        for _ in 0..<500 where collector.conditionRequests == reconciled { try await Task.sleep(for: .milliseconds(10)) }
        #expect(collector.conditionRequests == reconciled + 1)
        for _ in 0..<500 where store.isRefreshing { try await Task.sleep(for: .milliseconds(10)) }
        #expect(store.health(for: "roof") == .warning)
        #expect(store.isEventStreamConnected)
        // Repeating the same phase is not a change and does not refresh again.
        store.setForeground(true)
        try await Task.sleep(for: .milliseconds(100))
        #expect(collector.conditionRequests == reconciled + 1)
        await store.disconnect(clearCachedScope: true)
    }
}
