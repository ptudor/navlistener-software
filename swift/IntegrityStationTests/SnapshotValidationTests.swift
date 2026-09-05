import Foundation
import Testing
@testable import IntegrityStation

@MainActor @Test
func malformedSnapshotCannotReplaceDisplayOrCache() async throws {
    let directory = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    let cache = SnapshotCache(directory: directory)
    let key = AudienceCacheKey(server: "https://collector.invalid", principal: "anonymous", audience: .publicAudience, authorizationRevision: "public")
    func snapshot(_ rows: String) throws -> ObserversSnapshot {
        let payload = try JSONDecoder().decode(ObserversPayload.self, from: Data("{\"schema\":\"2.0\",\"audience\":\"public\",\"observers\":\(rows)}".utf8))
        return ObserversSnapshot(receivedAt: Date(), scope: key, serverTime: nil, payload: payload)
    }
    let good = try snapshot(#"[{"id":"first","last_seen_s":10},{"id":"second","last_seen_s":20}]"#)
    let store = StationStore()
    try store.apply(good, cached: false)
    try await cache.saveObservers(good, for: key)
    for rows in [#"[{"id":"duplicate","last_seen_s":1},{"id":"duplicate","last_seen_s":2}]"#, #"[{"id":"duplicate"},{"id":"duplicate"}]"#, #"[{"id":""}]"#] {
        let bad = try snapshot(rows)
        for cached in [false,true] {
            #expect(throws: FeedError.invalidResponse) { try store.apply(bad, cached: cached) }
            #expect(store.observers.map(\.id) == ["first","second"])
            #expect(try #require(store.currentLastSeenAge(for: "first")) >= 10)
            #expect(store.currentLastSeenAge(for: "duplicate") == nil)
        }
        do {try await cache.saveObservers(bad, for: key);Issue.record("invalid snapshot saved")}
        catch let error as FeedError {#expect(error == .invalidResponse)}
        #expect(try await cache.loadObservers(for: key)?.payload.observers?.map(\.id) == ["first","second"])
        // Malformed on-disk input bypasses save admission and must be a miss
        // on the real restore path, before assigning any snapshot presentation.
        struct Document: Encodable {let scope: AudienceCacheKey;let observers: ObserversSnapshot}
        let encoder = JSONEncoder();encoder.dateEncodingStrategy = .iso8601
        let file = directory.appending(path: "audience-v3-\(key.storageID).json")
        try encoder.encode(Document(scope:key,observers:bad)).write(to:file)
        do {_ = try await cache.restoreObservers(for:key,access:CacheAccess());Issue.record("invalid cache restored")}
        catch let error as FeedError {#expect(error == .invalidResponse)}
        try FileManager.default.removeItem(at:file)
        try await cache.saveObservers(good,for:key)
    }
}
