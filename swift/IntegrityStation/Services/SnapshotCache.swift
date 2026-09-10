import Foundation

// A session revokes this synchronously before scheduling cache erasure. The
// lock spans the local file write, so even a previously queued actor operation
// cannot recreate private state after its session has been retired.
final class CacheAccess: @unchecked Sendable {
    private let lock = NSLock()
    private var valid = true
    func invalidate() { lock.withLock { valid = false } }
    func perform(_ operation: () throws -> Void) rethrows {
        try lock.withLock { if valid { try operation() } }
    }
}

actor SnapshotCache {
    private let directory: URL
    private let fileManager: FileManager
    private let encoder: JSONEncoder
    private let decoder: JSONDecoder
    private let beforeCursorSave: (@Sendable () async -> Void)?
    private let beforeObserverSave: (@Sendable () async -> Void)?
    private let beforeCursorLoad: (@Sendable () async -> Void)?
    /// Test seam : substitutes the POSIX-permission write so the
    /// fail-closed path can be exercised. A process owns the files it just wrote,
    /// so chmod cannot be made to fail on a normal filesystem; production passes
    /// nil and uses FileManager directly.
    private let setPermissions: (@Sendable (URL, Int) throws -> Void)?

    init(directory: URL? = nil, fileManager: FileManager = .default,
         beforeCursorSave: (@Sendable () async -> Void)? = nil,
         beforeObserverSave: (@Sendable () async -> Void)? = nil,
         beforeCursorLoad: (@Sendable () async -> Void)? = nil,
         setPermissions: (@Sendable (URL, Int) throws -> Void)? = nil) {
        self.beforeCursorSave = beforeCursorSave
        self.beforeObserverSave = beforeObserverSave
        self.beforeCursorLoad = beforeCursorLoad
        self.setPermissions = setPermissions
        self.fileManager = fileManager
        self.directory = directory ?? Self.defaultDirectory(fileManager: fileManager)
        self.encoder = JSONEncoder()
        self.decoder = JSONDecoder()
        encoder.dateEncodingStrategy = .iso8601
        decoder.dateDecodingStrategy = .iso8601
    }

    func loadObservers(for key: AudienceCacheKey) throws -> ObserversSnapshot? {
        try loadDocument(for: key)?.observers
    }

    func restoreObservers(for key: AudienceCacheKey, at now: Date = Date(), access: CacheAccess) throws -> ObserversSnapshot? {
        guard var document = try loadDocument(for: key),
              let snapshot = document.observers else { return nil }
        if now.timeIntervalSince1970.isFinite,
           now >= snapshot.receivedAt,
           now >= (snapshot.lastRestoredAt ?? snapshot.receivedAt) {
            document.observers?.lastRestoredAt = now
            // Best effort: the stamp only hardens a LATER launch against a
            // rolled-back wall clock. A container that cannot be written (full,
            // read-only) must still restore the cached stations with their
            // honest residence age rather than show nothing (astra-6
            // verification of regression fix); apply() still rejects a clock that
            // has moved below the snapshot's own receive time.
            try? access.perform { try save(document) }
        }
        return snapshot
    }

    func saveObservers(_ snapshot: ObserversSnapshot, for key: AudienceCacheKey, access: CacheAccess? = nil) async throws {
        guard snapshot.scope == key else { throw CocoaError(.fileWriteInvalidFileName) }
        try snapshot.payload.validate()
        await beforeObserverSave?()
        let write = {
            var document = try self.loadDocument(for: key) ?? AudienceCacheDocument(scope: key)
            document.observers = snapshot
            try self.save(document)
        }
        if let access { try access.perform(write) } else { try write() }
    }

    func loadCursor(for key: AudienceCacheKey) async throws -> String? {
        await beforeCursorLoad?()
        return try loadDocument(for: key)?.lastEventID
    }

    func saveCursor(_ cursor: String?, for key: AudienceCacheKey, access: CacheAccess? = nil) async throws {
        await beforeCursorSave?()
        let write = {
            var document = try self.loadDocument(for: key) ?? AudienceCacheDocument(scope: key)
            document.lastEventID = cursor
            try self.save(document)
        }
        if let access { try access.perform(write) } else { try write() }
    }

    func clear(_ key: AudienceCacheKey) throws {
        let url = fileURL(for: key)
        guard fileManager.fileExists(atPath: url.path) else { return }
        try fileManager.removeItem(at: url)
    }

    func clearAll() throws {
        guard fileManager.fileExists(atPath: directory.path) else { return }
        for url in try fileManager.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil)
        where url.lastPathComponent.hasPrefix(Self.filePrefix) {
            try fileManager.removeItem(at: url)
        }
    }

    func clearPrivate(forServer server: String, principal: String? = nil) throws {
        guard fileManager.fileExists(atPath: directory.path) else { return }
        for url in try fileManager.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil)
        where url.lastPathComponent.hasPrefix(Self.filePrefix) {
            guard let document = try? decoder.decode(
                AudienceCacheDocument.self,
                from: Data(contentsOf: url)
            ) else {
                // A corrupt scoped document is never useful offline and cannot
                // be proven public, so fail closed during credential cleanup.
                try fileManager.removeItem(at: url)
                continue
            }
            guard document.scope.server == server,
                  document.scope.audience.isPrivate,
                  principal == nil || document.scope.principal == principal
            else { continue }
            try fileManager.removeItem(at: url)
        }
    }

    private func loadDocument(for key: AudienceCacheKey) throws -> AudienceCacheDocument? {
        let url = fileURL(for: key)
        guard fileManager.fileExists(atPath: url.path) else { return nil }
        let document = try decoder.decode(AudienceCacheDocument.self, from: Data(contentsOf: url))
        guard document.scope == key else { throw CocoaError(.fileReadCorruptFile) }
        try document.observers?.payload.validate()
        return document
    }

    private func save(_ document: AudienceCacheDocument) throws {
        try prepareDirectory()
        let url = fileURL(for: document.scope)
        #if os(iOS)
        try encoder.encode(document).write(to: url, options: [.atomic, .completeFileProtectionUnlessOpen])
        #else
        try encoder.encode(document).write(to: url, options: [.atomic])
        // on macOS the hardening used to be `try?` — discarded — so a
        // failure left the file at whatever the atomic write created while `save`
        // reported success. Measured on this platform: an atomic write of a NEW file
        // uses 0644 under the default 022 umask (a replacement inherits the existing
        // file's mode, which is why the first write is the one that matters), and
        // createDirectory makes 0755. A private snapshot carries organization and
        // collection observer metadata plus cursors, so neither is acceptable.
        //
        // The directory is hardened to 0700 first, which is what actually denies
        // other users — a 0644 file is unreachable through a directory they cannot
        // traverse — and the file's own 0600 is defense in depth on top. For a
        // private scope that 0600 is part of a successful save: if it cannot be
        // applied and verified, the newly written file is removed and the error is
        // raised rather than leaving a permissive private cache behind. Public
        // snapshots keep the previous best-effort behavior; nothing in them is
        // confidential.
        if document.scope.audience.isPrivate {
            do {
                try applyPermissions(0o600, to: url)
                guard try posixPermissions(of: url) == 0o600 else {
                    throw CocoaError(.fileWriteNoPermission)
                }
            } catch {
                try? fileManager.removeItem(at: url)
                throw error
            }
        } else {
            try? applyPermissions(0o600, to: url)
        }
        #endif
    }

    #if !os(iOS)
    private func applyPermissions(_ mode: Int, to url: URL) throws {
        if let setPermissions {
            try setPermissions(url, mode)
            return
        }
        try fileManager.setAttributes([.posixPermissions: mode], ofItemAtPath: url.path)
    }

    private func posixPermissions(of url: URL) throws -> UInt16 {
        let attributes = try fileManager.attributesOfItem(atPath: url.path)
        guard let mode = attributes[.posixPermissions] as? NSNumber else {
            throw CocoaError(.fileReadUnknown)
        }
        // Mask to the permission bits: file-type and set-id bits are not ours to assert.
        return mode.uint16Value & 0o7777
    }
    #endif

    private func prepareDirectory() throws {
        // create the directory narrow rather than widening it after
        // the fact. `attributes:` applies only at creation, so an existing directory
        // (including one left at 0755 by an earlier build) is tightened explicitly.
        try fileManager.createDirectory(at: directory, withIntermediateDirectories: true,
                                        attributes: [.posixPermissions: 0o700])
        #if !os(iOS)
        // This is the control that actually denies other users, so unlike the
        // per-file mode it is enforced for every scope.
        if try posixPermissions(of: directory) != 0o700 {
            try applyPermissions(0o700, to: directory)
            guard try posixPermissions(of: directory) == 0o700 else {
                throw CocoaError(.fileWriteNoPermission)
            }
        }
        #endif
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var mutableDirectory = directory
        try? mutableDirectory.setResourceValues(values)
    }

    private func fileURL(for key: AudienceCacheKey) -> URL {
        directory.appending(path: "\(Self.filePrefix)\(key.storageID).json")
    }

    private static func defaultDirectory(fileManager: FileManager) -> URL {
        let root = fileManager.urls(for: .applicationSupportDirectory, in: .userDomainMask).first
            ?? fileManager.temporaryDirectory
        return root.appending(path: "net.intsat.station", directoryHint: .isDirectory)
    }

    private static let filePrefix = "audience-v3-"
}

private struct AudienceCacheDocument: Codable, Sendable {
    let scope: AudienceCacheKey
    var observers: ObserversSnapshot?
    var lastEventID: String?

    init(scope: AudienceCacheKey, observers: ObserversSnapshot? = nil, lastEventID: String? = nil) {
        self.scope = scope
        self.observers = observers
        self.lastEventID = lastEventID
    }
}
