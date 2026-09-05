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

    static func body(_ bytes: URLSession.AsyncBytes, maximum: Int) async throws -> Data {
        var data = Data()
        for try await byte in bytes {
            try Task.checkCancellation()
            guard data.count < maximum else { throw FeedError.inputLimit }
            data.append(byte)
        }
        return data
    }
}
