import Foundation

enum StationFormat {
    static let unknown = "—"

    static func uptime(seconds value: TimeInterval?) -> String {
        guard let value, value.isFinite, value >= 0 else { return unknown }
        guard let seconds = Int(exactly: value.rounded(.down)) else { return unknown }
        let days = seconds / 86_400
        let hours = seconds % 86_400 / 3_600
        let minutes = seconds % 3_600 / 60

        if days > 0 { return "\(days)d \(hours)h" }
        if hours > 0 { return "\(hours)h \(minutes)m" }
        if minutes > 0 { return "\(minutes)m \(seconds % 60)s" }
        return "\(seconds)s"
    }

    static func age(seconds value: TimeInterval?) -> String {
        guard let value, value.isFinite, value >= 0 else { return unknown }
        if value < 5 { return "now" }
        if value < 60 { return "\(Int(value))s ago" }
        if value < 3_600 { return "\(Int(value / 60))m ago" }
        if value < 86_400 { return "\(Int(value / 3_600))h ago" }
        guard let days = Int(exactly: (value / 86_400).rounded(.down)) else { return unknown }
        return "\(days)d ago"
    }

    static func clockDrift(nanoseconds value: Double?) -> String {
        guard let value, value.isFinite else { return unknown }
        let magnitude = abs(value)
        if magnitude < 1_000 { return "\(fixed(value)) ns" }
        if magnitude < 1_000_000 { return "\(fixed(value / 1_000)) µs" }
        return "\(fixed(value / 1_000_000)) ms"
    }

    static func carrierToNoise(_ value: Int?) -> String {
        value.map { "\($0) dB-Hz" } ?? unknown
    }

    /// The fused assurance score in [−1, +1], signed so its direction reads at a
    /// glance.
    static func assuranceScore(_ value: Double?) -> String {
        guard let value, value.isFinite else { return unknown }
        return String(format: "%+.2f", locale: Locale(identifier: "en_US_POSIX"), value)
    }

    /// A check's evidence as `name value` pairs in name order. Each name carries
    /// its unit as a suffix, as served, so no unit is added or converted here.
    static func assuranceValues(_ values: [String: Double]?) -> String? {
        guard let values, !values.isEmpty else { return nil }
        return values.sorted { $0.key < $1.key }
            .map { "\($0.key) \(evidenceValue($0.value))" }
            .joined(separator: "  ")
    }

    // Six significant digits without an exponent or grouping: nanosecond offsets and
    // ns/s² rates both stay readable and copyable.
    private static func evidenceValue(_ value: Double) -> String {
        guard value.isFinite else { return unknown }
        return value.formatted(.number.precision(.significantDigits(1...6)).grouping(.never)
            .locale(Locale(identifier: "en_US_POSIX")))
    }

    /// A configuration hash shortened for display: its algorithm label and the
    /// first twelve digest digits, enough to tell configurations apart by eye.
    static func configurationHash(_ value: String?) -> String {
        guard let value, !value.isEmpty else { return unknown }
        let parts = value.split(separator: ":", maxSplits: 1)
        guard parts.count == 2 else { return String(value.prefix(12)) }
        return "\(parts[0]):\(parts[1].prefix(12))"
    }

    private static func fixed(_ value: Double) -> String {
        String(format: "%.1f", locale: Locale(identifier: "en_US_POSIX"), value)
    }
}
