import Foundation

// Bounds are on received bytes, not Content-Length (which can be absent or lie).
// 32 MiB accommodates large observer fleets; condition snapshots separately cap
// at 10,000 entries. SSE queues retain at most 64 frames of at most 256 KiB each.
enum NetworkLimits {
    static let responseBytes = 32 * 1024 * 1024
    static let errorBytes = 64 * 1024
    static let lineBytes = 64 * 1024
    static let frameBytes = 256 * 1024
    static let pendingEvents = 64
    static let conditions = 10_000

    /// Bytes gathered before each append to the body. URLSession hands the
    /// body out one byte at a time; appending to `Data` per byte made a
    /// multi-megabyte fleet document CPU-bound on the client.
    static let chunkBytes = 64 * 1024

    static func body(_ bytes: URLSession.AsyncBytes, maximum: Int) async throws -> Data {
        var data = Data()
        var chunk: [UInt8] = []
        chunk.reserveCapacity(chunkBytes)
        var received = 0
        for try await byte in bytes {
            received += 1
            guard received <= maximum else { throw FeedError.inputLimit }
            chunk.append(byte)
            if chunk.count == chunkBytes {
                try Task.checkCancellation()
                data.append(contentsOf: chunk)
                chunk.removeAll(keepingCapacity: true)
            }
        }
        try Task.checkCancellation()
        data.append(contentsOf: chunk)
        return data
    }
}
