import Foundation
import Testing
@testable import IntegrityStation

/// regression fix. On macOS the cache hardening was `try?` — discarded — so a
/// failed `setAttributes` left a private snapshot at whatever the atomic write
/// created while `save` reported success. Measured on this platform: an atomic
/// write of a NEW file lands at 0644 under the default 022 umask, and
/// `createDirectory` makes 0755. A private snapshot carries organization and
/// collection observer metadata plus cursors, so neither is acceptable.
@Suite(.serialized)
struct CachePermissionTests {
    private func mode(_ url: URL) throws -> UInt16 {
        let attributes = try FileManager.default.attributesOfItem(atPath: url.path)
        return try #require(attributes[.posixPermissions] as? NSNumber).uint16Value & 0o7777
    }

    private func key(isPrivate: Bool) throws -> AudienceCacheKey {
        let audience = isPrivate
            ? try #require(ReadAudience("organization:customer-a"))
            : ReadAudience.publicAudience
        return AudienceCacheKey(server: "https://cache.invalid",
                                principal: isPrivate ? "viewer-a" : AudienceCacheKey.anonymousPrincipal,
                                audience: audience, authorizationRevision: "rev-1")
    }

    private func snapshot(for key: AudienceCacheKey) throws -> ObserversSnapshot {
        let payload = try JSONDecoder().decode(ObserversPayload.self, from: Data(
            #"{"schema":"2.0","audience":"public","observers":[{"id":"station"}]}"#.utf8))
        return ObserversSnapshot(receivedAt: Date(), scope: key, serverTime: nil, payload: payload)
    }

    /// Under the permissive default umask — the case that actually ships — a
    /// private save must leave a 0700 directory and a 0600 file.
    @Test func privateSaveIsNarrowUnderAPermissiveUmask() async throws {
        let previous = umask(0o022) // force the default; other tests may have changed it
        defer { umask(previous) }

        let dir = FileManager.default.temporaryDirectory
            .appending(path: "cacheperm-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: dir) }
        let cache = SnapshotCache(directory: dir)

        let scope = try key(isPrivate: true)
        try await cache.saveObservers(try snapshot(for: scope), for: scope, access: CacheAccess())

        #expect(try mode(dir) == 0o700,
                "the cache directory must deny traversal to group and other; createDirectory alone makes 0755")
        let file = try #require(try FileManager.default
            .contentsOfDirectory(at: dir, includingPropertiesForKeys: nil)
            .first { $0.lastPathComponent.hasPrefix("audience-v3-") })
        #expect(try mode(file) == 0o600,
                "a private snapshot must not be group/other readable; an atomic write makes 0644")

        // Replacement must stay narrow, and must still be atomic (readable, valid).
        try await cache.saveObservers(try snapshot(for: scope), for: scope, access: CacheAccess())
        #expect(try mode(dir) == 0o700, "replacement must not widen the directory")
        #expect(try mode(file) == 0o600, "replacement must not widen the file")
        #expect(try await cache.loadObservers(for: scope) != nil, "the replaced document must still load")
    }

    /// An existing directory left at 0755 by an earlier build must be tightened,
    /// not accepted: `createDirectory`'s `attributes:` only apply at creation.
    @Test func existingPermissiveDirectoryIsTightened() async throws {
        let previous = umask(0o022)
        defer { umask(previous) }

        let dir = FileManager.default.temporaryDirectory
            .appending(path: "cacheperm-old-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: dir) }
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true,
                                                attributes: [.posixPermissions: 0o755])
        #expect(try mode(dir) == 0o755, "fixture must start permissive")

        let cache = SnapshotCache(directory: dir)
        let scope = try key(isPrivate: true)
        try await cache.saveObservers(try snapshot(for: scope), for: scope, access: CacheAccess())
        #expect(try mode(dir) == 0o700, "an already-permissive cache directory must be tightened")
    }

    /// Public snapshots keep working — nothing in them is confidential, and the
    /// review requires public-cache behavior to be preserved.
    @Test func publicSaveStillWorks() async throws {
        let dir = FileManager.default.temporaryDirectory
            .appending(path: "cacheperm-pub-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: dir) }
        let cache = SnapshotCache(directory: dir)
        let scope = try key(isPrivate: false)
        try await cache.saveObservers(try snapshot(for: scope), for: scope, access: CacheAccess())
        #expect(try await cache.loadObservers(for: scope) != nil)
        #expect(try mode(dir) == 0o700, "the directory is hardened for every scope")
    }

    /// A private save that cannot be hardened must fail rather than leave a
    /// permissive file behind — and must not leave that file behind either. A
    /// process owns the file it just wrote, so chmod cannot be made to fail on a
    /// real filesystem; the injected seam stands in for the ACL/attribute failures
    /// the review asks about.
    @Test func privateSaveFailsClosedWhenHardeningIsImpossible() async throws {
        let previous = umask(0o022)
        defer { umask(previous) }

        let dir = FileManager.default.temporaryDirectory
            .appending(path: "cacheperm-fail-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: dir) }
        // Fail only the FILE hardening: the directory must still be tightened, so
        // the failure being tested is the one the review names.
        let cache = SnapshotCache(directory: dir, setPermissions: { url, mode in
            if mode == 0o600 { throw CocoaError(.fileWriteNoPermission) }
            try FileManager.default.setAttributes([.posixPermissions: mode], ofItemAtPath: url.path)
        })

        let scope = try key(isPrivate: true)
        let payload = try snapshot(for: scope)
        await #expect(throws: (any Error).self) {
            try await cache.saveObservers(payload, for: scope, access: CacheAccess())
        }
        let leftovers = try FileManager.default
            .contentsOfDirectory(at: dir, includingPropertiesForKeys: nil)
            .filter { $0.lastPathComponent.hasPrefix("audience-v3-") }
        #expect(leftovers.isEmpty,
                "a private snapshot that could not be hardened must not be left on disk")
        #expect(try await cache.loadObservers(for: scope) == nil,
                "the failed save must not be readable back")
    }

    /// The same failure on a PUBLIC snapshot is tolerated: nothing in it is
    /// confidential, and the review requires public-cache behavior to be preserved.
    @Test func publicSaveToleratesHardeningFailure() async throws {
        let dir = FileManager.default.temporaryDirectory
            .appending(path: "cacheperm-pubfail-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: dir) }
        let cache = SnapshotCache(directory: dir, setPermissions: { url, mode in
            if mode == 0o600 { throw CocoaError(.fileWriteNoPermission) }
            try FileManager.default.setAttributes([.posixPermissions: mode], ofItemAtPath: url.path)
        })
        let scope = try key(isPrivate: false)
        try await cache.saveObservers(try snapshot(for: scope), for: scope, access: CacheAccess())
        #expect(try await cache.loadObservers(for: scope) != nil)
    }
}
