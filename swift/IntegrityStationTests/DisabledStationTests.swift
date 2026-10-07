import Foundation
import SwiftUI
import Testing
@testable import IntegrityStation

/// A station the collector serves as `disabled` is configured but deliberately
/// not collected: it is labelled as such and left out of the health rollup, so
/// a parked receiver cannot hold the menu bar and the overview on "offline".
@Suite(.serialized) @MainActor
struct DisabledStationTests {
    private let host = "disabled.invalid"

    @discardableResult
    private func settle(until condition: @MainActor () async throws -> Bool) async throws -> Bool {
        for _ in 0..<500 {
            if try await condition() { return true }
            try await Task.sleep(for: .milliseconds(10))
        }
        return try await condition()
    }

    @Test func disabledStationKeepsItsHealthButLeavesTheRollup() async throws {
        StubCollectorProtocol.register(host: host) { request in
            let path = request.url!.path
            if path.hasSuffix("/audiences") {
                return Data(#"{"ok":true,"data":{"schema":"2.0","revision":"public-v1","audiences":["public"]}}"#.utf8)
            }
            if path.hasSuffix("/conditions") {
                return Data(#"{"ok":true,"data":{"schema":"2.0","audience":"public","complete":true,"epoch":"a","cursor":1,"events":[]}}"#.utf8)
            }
            if path.hasSuffix("/gnss/events") {
                return Data("event: status\ndata: {\"status\":\"connected\"}\n\n".utf8)
            }
            if path.hasSuffix("/observers") {
                return Data(#"{"ok":true,"time":"2026-10-07T12:00:00Z","data":{"schema":"2.0","audience":"public","observers":[{"id":"roof","last_seen_s":1},{"id":"parked","disabled":true,"last_seen_s":100000}]}}"#.utf8)
            }
            return Data(#"{"ok":true,"data":{"schema":"2.0","audience":"public","events":[]}}"#.utf8)
        }
        defer { StubCollectorProtocol.unregister(host: host) }
        let directory = FileManager.default.temporaryDirectory.appending(path: "DisabledStation.\(UUID())")
        defer { try? FileManager.default.removeItem(at: directory) }
        let network = StubCollectorProtocol.session()
        let store = StationStore(feedClient: FeedClient(session: network), eventStream: EventStream(session: network),
                                 cache: SnapshotCache(directory: directory))
        let session = try #require(ReadSession(baseURL: URL(string: "https://\(host)")!, principalID: nil,
                                               audience: .publicAudience, authorizationRevision: "public-v1", token: nil))
        store.start(session: session, stationIDs: ["roof", "parked"])
        #expect(try await settle { store.observers.count == 2 && store.isEventStreamConnected && !store.isRefreshing })

        #expect(store.isDisabled("parked"))
        #expect(!store.isDisabled("roof"))
        #expect(!store.isDisabled("never-served"))
        #expect(store.health(for: "roof") == .ok)
        #expect(store.health(for: "parked") == .offline)
        #expect(store.rollupHealth == .ok)

        store.selectedStationIDs = ["parked"]
        #expect(store.rollupHealth == .unknown)
        store.selectedStationIDs = ["parked", "roof"]
        #expect(store.rollupHealth == .ok)
        await store.disconnect(clearCachedScope: true)
    }

    @Test func disabledBadgeRenders() throws {
        let renderer = ImageRenderer(content: StationDisabledBadge().padding(8))
        let image = try #require(renderer.cgImage)
        #expect(image.width > 20 && image.height > 10)
    }
}
