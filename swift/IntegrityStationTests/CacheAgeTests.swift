import Foundation
import Testing
@testable import IntegrityStation

@MainActor @Test
func cachedObservationAgeIncludesResidence() throws {
    let received = Date(timeIntervalSince1970: 1_786_388_400)
    let key = AudienceCacheKey(server: "https://collector.invalid", principal: "anonymous", audience: .publicAudience, authorizationRevision: "public")
    for fields in [#""last_seen_s":2"#, #""last_seen":1786388398"#, #""rf":{"last_seen":1786388398}"#] {
        let payload = try JSONDecoder().decode(ObserversPayload.self, from: Data("{\"schema\":\"2.0\",\"audience\":\"public\",\"observers\":[{\"id\":\"station\",\(fields)}]}".utf8))
        let snapshot = ObserversSnapshot(receivedAt: received, scope: key, serverTime: "2026-08-10T19:00:00Z", payload: payload)
        for elapsed in [60.0, 86400.0, 345600.0] {
            let store = StationStore()
            store.apply(snapshot, cached: true, now: received.addingTimeInterval(elapsed))
            let age = try #require(store.currentLastSeenAge(for: "station"))
            #expect(age >= elapsed + 2 && age < elapsed + 3)
            if elapsed >= 86400 { #expect(store.health(for: "station") == .offline) }
        }
        for invalid in [received.addingTimeInterval(-1), Date(timeIntervalSince1970: .nan)] {
            let store = StationStore()
            store.apply(snapshot, cached: true, now: invalid)
            #expect(store.currentLastSeenAge(for: "station") == nil)
        }
        let store = StationStore()
        store.apply(snapshot, cached: false, now: received.addingTimeInterval(86400))
        #expect(try #require(store.currentLastSeenAge(for: "station")) < 3)
    }
}

@MainActor @Test
func cacheRestoreWatermarkPreventsYoungerRelaunch() async throws {
    let dir = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: dir) }
    let cache = SnapshotCache(directory: dir)
    let received = Date(timeIntervalSince1970: 1_786_388_400)
    let key = AudienceCacheKey(server: "https://collector.invalid", principal: "anonymous", audience: .publicAudience, authorizationRevision: "public")
    let payload = try JSONDecoder().decode(ObserversPayload.self, from: Data(#"{"schema":"2.0","audience":"public","observers":[{"id":"station","last_seen_s":2}]}"#.utf8))
    try await cache.saveObservers(ObserversSnapshot(receivedAt: received, scope: key, serverTime: nil, payload: payload), for: key)
    let first = try #require(try await cache.restoreObservers(for: key, at: received.addingTimeInterval(86400), access: CacheAccess()))
    let store = StationStore()
    store.apply(first, cached: true, now: received.addingTimeInterval(86400))
    #expect(try #require(store.currentLastSeenAge(for: "station")) >= 86402)
    let second = try #require(try await cache.restoreObservers(for: key, at: received.addingTimeInterval(60), access: CacheAccess()))
    store.apply(second, cached: true, now: received.addingTimeInterval(60))
    #expect(store.currentLastSeenAge(for: "station") == nil)
    let invalid = ObserversSnapshot(receivedAt: Date(timeIntervalSince1970: 0), scope: key, serverTime: "invalid", payload: payload)
    store.apply(invalid, cached: true)
    #expect(store.currentLastSeenAge(for: "station") == nil)
}
