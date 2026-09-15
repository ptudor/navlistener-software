import Foundation

enum BoardFormat {
    static func measurement(_ value: Double?, unit: String, digits: Int = 2) -> String {
        guard let value, value.isFinite else { return StationFormat.unknown }
        return "\(value.formatted(.number.precision(.fractionLength(0...digits)))) \(unit)"
    }

    static func count<T: BinaryInteger>(_ value: T?) -> String {
        guard let value else { return StationFormat.unknown }
        return String(value)
    }

    static func bytes(_ value: UInt32?) -> String {
        value.map { ByteCountFormatter.string(fromByteCount: Int64($0), countStyle: .memory) }
            ?? StationFormat.unknown
    }

    static func flag(_ value: Bool?) -> String {
        guard let value else { return StationFormat.unknown }
        return String(localized: value ? "board.yes" : "board.no")
    }

    static func timestamp(_ value: String?) -> String {
        guard let date = WireDate.parse(value) else { return StationFormat.unknown }
        return date.formatted(date: .abbreviated, time: .standard)
    }

    static func status(_ value: String?) -> String {
        // Unknown future wire values remain visible without guessing their meaning.
        guard let value else { return StationFormat.unknown }
        switch value {
        case "unknown": return String(localized: "board.status.unknown")
        case "enabled_1hz": return String(localized: "board.status.enabled_1hz")
        case "oscillator_stopped": return String(localized: "board.status.oscillator_stopped")
        case "alarm_or_coarse_trim_conflict": return String(localized: "board.status.trim_conflict")
        case "io_error": return String(localized: "board.status.io_error")
        case "unlocked": return String(localized: "board.status.unlocked")
        case "locked": return String(localized: "board.status.locked")
        case "invalid": return String(localized: "board.status.invalid")
        case "untested": return String(localized: "board.status.untested")
        case "screening_pass": return String(localized: "board.status.screening_pass")
        case "repeating_output": return String(localized: "board.status.repeating_untrusted")
        case "initialization_required": return String(localized: "board.status.initialization_required")
        case "recovery_required": return String(localized: "board.status.recovery_required")
        case "replacement_confirmation_required": return String(localized: "board.status.replacement_required")
        case "invalid_manifest": return String(localized: "board.status.invalid_manifest")
        case "use_manifest": return String(localized: "board.status.use_manifest")
        case "absent": return String(localized: "board.status.absent")
        default: return value
        }
    }
}
