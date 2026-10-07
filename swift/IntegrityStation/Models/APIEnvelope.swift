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

/// `schema` is served as "<major>.<minor>" (docs/OUTPUT.md §0). A client built
/// for major version 2 accepts every 2.x minor, since a minor bump is additive;
/// a different major, or anything that is not a version string, is an invalid
/// response. The audience echo on each payload is checked separately and
/// exactly, because that one is a cross-audience check, not a version check.
enum WireSchema {
    static let supportedMajor = 2

    static func isSupported(_ schema: String?) -> Bool {
        guard let schema else { return false }
        let parts = schema.split(separator: ".", omittingEmptySubsequences: false)
        guard parts.count == 2,
              parts.allSatisfy({ !$0.isEmpty && $0.utf8.allSatisfy { (48...57).contains($0) } }),
              let major = Int(parts[0])
        else { return false }
        return major == supportedMajor
    }
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
