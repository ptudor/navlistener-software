import Foundation
import Observation
import SwiftUI

enum AppAppearance: String, CaseIterable, Sendable {
    case system
    case dark
    case light

    var colorScheme: ColorScheme? {
        switch self {
        case .system: nil
        case .dark: .dark
        case .light: .light
        }
    }
}

@MainActor
@Observable
final class AppSettings {
    @ObservationIgnored private let defaults: UserDefaults

    var serverURLString: String { didSet { defaults.set(serverURLString, forKey: Keys.serverURL) } }
    var stationIDs: [String] { didSet { save(stationIDs, key: Keys.stationIDs) } }
    var labelsByStationID: [String: String] { didSet { save(labelsByStationID, key: Keys.labels) } }
    var notifyOffline: Bool { didSet { defaults.set(notifyOffline, forKey: Keys.notifyOffline) } }
    var notifyRF: Bool { didSet { defaults.set(notifyRF, forKey: Keys.notifyRF) } }
    var notifyCritical: Bool { didSet { defaults.set(notifyCritical, forKey: Keys.notifyCritical) } }
    var appearance: AppAppearance {
        didSet { defaults.set(appearance.rawValue, forKey: Keys.appearance) }
    }

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        serverURLString = defaults.string(forKey: Keys.serverURL) ?? ""
        stationIDs = Self.decode([String].self, from: defaults.data(forKey: Keys.stationIDs)) ?? []
        labelsByStationID = Self.decode([String: String].self, from: defaults.data(forKey: Keys.labels)) ?? [:]
        notifyOffline = defaults.object(forKey: Keys.notifyOffline) as? Bool ?? true
        notifyRF = defaults.object(forKey: Keys.notifyRF) as? Bool ?? true
        notifyCritical = defaults.object(forKey: Keys.notifyCritical) as? Bool ?? true
        appearance = AppAppearance(rawValue: defaults.string(forKey: Keys.appearance) ?? "") ?? .dark
    }

    func label(for stationID: String) -> String? {
        labelsByStationID[stationID].flatMap { $0.isEmpty ? nil : $0 }
    }

    private func save<T: Encodable>(_ value: T, key: String) {
        defaults.set(try? JSONEncoder().encode(value), forKey: key)
    }

    private static func decode<T: Decodable>(_ type: T.Type, from data: Data?) -> T? {
        guard let data else { return nil }
        return try? JSONDecoder().decode(type, from: data)
    }

    private enum Keys {
        static let serverURL = "serverURL"
        static let stationIDs = "stationIDs"
        static let labels = "stationLabels"
        static let notifyOffline = "notifyOffline"
        static let notifyRF = "notifyRF"
        static let notifyCritical = "notifyCritical"
        static let appearance = "appearance"
    }
}
