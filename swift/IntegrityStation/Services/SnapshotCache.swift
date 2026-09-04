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

    init(directory: URL? = nil, fileManager: FileManager = .default,
         beforeCursorSave: (@Sendable () async -> Void)? = nil,
         beforeObserverSave: (@Sendable () async -> Void)? = nil,
         beforeCursorLoad: (@Sendable () async -> Void)? = nil) {
        self.beforeCursorSave = beforeCursorSave
        self.beforeObserverSave = beforeObserverSave
        self.beforeCursorLoad = beforeCursorLoad
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

    func saveObservers(_ snapshot: ObserversSnapshot, for key: AudienceCacheKey, access: CacheAccess? = nil) async throws {
        guard snapshot.scope == key else { throw CocoaError(.fileWriteInvalidFileName) }
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
        return document
    }

    private func save(_ document: AudienceCacheDocument) throws {
        try prepareDirectory()
        let url = fileURL(for: document.scope)
        #if os(iOS)
        try encoder.encode(document).write(to: url, options: [.atomic, .completeFileProtectionUnlessOpen])
        #else
        try encoder.encode(document).write(to: url, options: [.atomic])
        #endif
        try? fileManager.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }

    private func prepareDirectory() throws {
        try fileManager.createDirectory(at: directory, withIntermediateDirectories: true)
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
