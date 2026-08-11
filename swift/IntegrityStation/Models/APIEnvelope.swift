import Foundation

/// navlistener's v2 response envelope (docs/OUTPUT.md §0). Error responses use
/// the same type with data absent and error/code present.
struct APIEnvelope<Payload: Codable & Sendable>: Codable, Sendable {
    let ok: Bool
    let time: String?
    let data: Payload?
    let error: String?
    let code: Int?
}

enum WireDate {
    static func parse(_ value: String?) -> Date? {
        guard let value else { return nil }
        if let date = try? Date(value, strategy: .iso8601) {
            return date
        }
        return try? Date(
            value,
            strategy: Date.ISO8601FormatStyle(includingFractionalSeconds: true)
        )
    }
}
