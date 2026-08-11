import SwiftUI

/// The Integrity family palette from apps/intsat/docs/COLOR_STANDARDS.md,
/// corrected to navlistener's u-blox gnssid order (OUTPUT.md §0).
enum StationPalette {
    static let darkBackground = Color(red: 0x0D / 255, green: 0x11 / 255, blue: 0x17 / 255)
    static let darkSurface = Color(red: 0x16 / 255, green: 0x1B / 255, blue: 0x22 / 255)
    static let darkRaised = Color(red: 0x1C / 255, green: 0x23 / 255, blue: 0x33 / 255)

    static let ok = Color(red: 0x58 / 255, green: 0xA6 / 255, blue: 0xFF / 255)
    static let warning = Color(red: 0xD2 / 255, green: 0x99 / 255, blue: 0x22 / 255)
    static let critical = Color(red: 0xF8 / 255, green: 0x51 / 255, blue: 0x49 / 255)
    static let stale = Color(red: 0x65 / 255, green: 0x6D / 255, blue: 0x76 / 255)

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
