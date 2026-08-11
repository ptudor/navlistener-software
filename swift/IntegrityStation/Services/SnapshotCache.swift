import Foundation

actor SnapshotCache {
    private let directory: URL
    private let fileManager: FileManager
    private let encoder: JSONEncoder
    private let decoder: JSONDecoder

    init(directory: URL? = nil, fileManager: FileManager = .default) {
        self.fileManager = fileManager
        self.directory = directory ?? Self.defaultDirectory(fileManager: fileManager)
        self.encoder = JSONEncoder()
        self.decoder = JSONDecoder()
        encoder.dateEncodingStrategy = .iso8601
        decoder.dateDecodingStrategy = .iso8601
    }

    func loadObservers() throws -> ObserversSnapshot? {
        let url = directory.appending(path: "observers-v2.json")
        guard fileManager.fileExists(atPath: url.path) else { return nil }
        return try decoder.decode(ObserversSnapshot.self, from: Data(contentsOf: url))
    }

    func saveObservers(_ snapshot: ObserversSnapshot) throws {
        try fileManager.createDirectory(at: directory, withIntermediateDirectories: true)
        try encoder.encode(snapshot).write(
            to: directory.appending(path: "observers-v2.json"),
            options: [.atomic]
        )
    }

    func clear() throws {
        let url = directory.appending(path: "observers-v2.json")
        guard fileManager.fileExists(atPath: url.path) else { return }
        try fileManager.removeItem(at: url)
    }

    private static func defaultDirectory(fileManager: FileManager) -> URL {
        let root = fileManager.urls(for: .applicationSupportDirectory, in: .userDomainMask).first
            ?? fileManager.temporaryDirectory
        return root.appending(path: "net.intsat.station", directoryHint: .isDirectory)
    }
}
