import SwiftUI

/// The Integrity family palette from apps/intsat/docs/COLOR_STANDARDS.md,
/// corrected to navlistener's u-blox gnssid order (OUTPUT.md §0).
enum StationPalette {
    static let accent = Color("AccentColor")
    static let darkBackground = Color(red: 0x0A / 255, green: 0x0A / 255, blue: 0x1A / 255)
    static let darkSurface = Color(red: 0x16 / 255, green: 0x1B / 255, blue: 0x22 / 255)
    static let darkRaised = Color(red: 0x1C / 255, green: 0x23 / 255, blue: 0x33 / 255)
    static let lightBackground = Color(red: 0xE8 / 255, green: 0xEA / 255, blue: 0xED / 255)
    static let lightSurface = lightBackground
    static let lightRaised = Color(red: 0xDF / 255, green: 0xE2 / 255, blue: 0xE6 / 255)

    static let ok = Color("StatusOK")
    static let warning = Color("StatusWarning")
    static let critical = Color("StatusCritical")
    static let stale = Color("StatusStale")

    static func constellation(gnssid: Int, scheme: ColorScheme) -> Color {
        let pair: (dark: UInt32, light: UInt32) = switch gnssid {
        case 0: (0x2DD4BF, 0x0D9488) // GPS
        case 1: (0xC9D1D9, 0x4A525A) // SBAS, including KASS
        case 2: (0x58A6FF, 0x0969DA) // Galileo
        case 3: (0xBC8CFF, 0x8250DF) // BeiDou
        case 5: (0xE3B341, 0x866100) // QZSS
        case 6: (0xF0883E, 0xBC4C00) // GLONASS
        case 7: (0xF778BA, 0xBF3989) // NavIC
        default: (0xCCCCCC, 0x808080) // IMES (4) and unknown ids
        }
        return Color(rgb: scheme == .dark ? pair.dark : pair.light)
    }

    static func health(_ state: HealthState) -> Color {
        switch state {
        case .ok: ok
        case .warning: warning
        case .critical: critical
        case .offline, .unknown: stale
        }
    }

    static func assurance(_ level: AssuranceLevel?) -> Color {
        switch level {
        case .assured: ok
        case .inconsistent: warning
        case .unassured: critical
        case .unavailable, nil: stale
        }
    }

    static func severity(_ severity: EventSeverity?) -> Color {
        switch severity {
        case .critical: critical
        case .warning: warning
        case .info: ok
        case nil: stale
        }
    }
}

extension HealthState {
    var localizedName: String {
        switch self {
        case .unknown: String(localized: "health.unknown")
        case .offline: String(localized: "health.offline")
        case .critical: String(localized: "health.critical")
        case .warning: String(localized: "health.warning")
        case .ok: String(localized: "health.ok")
        }
    }

    var systemImage: String {
        switch self {
        case .unknown: "circle.dotted"
        case .offline: "antenna.radiowaves.left.and.right.slash"
        case .critical: "exclamationmark.octagon.fill"
        case .warning: "exclamationmark.triangle.fill"
        case .ok: "checkmark.circle.fill"
        }
    }
}

extension AssuranceLevel {
    /// A served state word's display name. A word this build does not know is
    /// shown as served rather than mapped onto a known level.
    static func displayName(_ word: String?) -> String {
        guard let word else { return StationFormat.unknown }
        return AssuranceLevel(rawValue: word)?.localizedName ?? word
    }

    var localizedName: String {
        switch self {
        case .unavailable: String(localized: "assurance.state.unavailable")
        case .unassured: String(localized: "assurance.state.unassured")
        case .inconsistent: String(localized: "assurance.state.inconsistent")
        case .assured: String(localized: "assurance.state.assured")
        }
    }

    var systemImage: String {
        switch self {
        case .unavailable: "circle.dotted"
        case .unassured: "exclamationmark.octagon.fill"
        case .inconsistent: "exclamationmark.triangle.fill"
        case .assured: "checkmark.shield.fill"
        }
    }
}

enum ConstellationName {
    static func localized(gnssid: Int) -> String {
        switch gnssid {
        case 0: String(localized: "constellation.gps")
        case 1: String(localized: "constellation.sbas")
        case 2: String(localized: "constellation.galileo")
        case 3: String(localized: "constellation.beidou")
        case 4: String(localized: "constellation.imes")
        case 5: String(localized: "constellation.qzss")
        case 6: String(localized: "constellation.glonass")
        case 7: String(localized: "constellation.navic")
        default: String(localized: "constellation.unknown")
        }
    }
}

private extension Color {
    init(rgb: UInt32) {
        self.init(
            red: Double((rgb >> 16) & 0xFF) / 255,
            green: Double((rgb >> 8) & 0xFF) / 255,
            blue: Double(rgb & 0xFF) / 255
        )
    }
}
