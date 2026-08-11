import Foundation
import Observation
import SwiftUI
import CryptoKit

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
    @ObservationIgnored private var stationIDsByScope: [String: [String]]
    @ObservationIgnored private var labelsByScope: [String: [String: String]]
    @ObservationIgnored private var audienceByServer: [String: String]
    @ObservationIgnored private var activeScopeID: String?
    @ObservationIgnored private var isActivatingScope = false
    @ObservationIgnored private var legacyStationIDs: [String]
    @ObservationIgnored private var legacyLabels: [String: String]

    /// The durable copy lives in KeychainConnectionStore. This in-memory value
    /// exists so SwiftUI can edit and display the active connection.
    var serverURLString: String
    var stationIDs: [String] { didSet { persistActiveStations() } }
    var labelsByStationID: [String: String] { didSet { persistActiveLabels() } }
    var notifyOffline: Bool { didSet { defaults.set(notifyOffline, forKey: Keys.notifyOffline) } }
    var notifyRF: Bool { didSet { defaults.set(notifyRF, forKey: Keys.notifyRF) } }
    var notifyCritical: Bool { didSet { defaults.set(notifyCritical, forKey: Keys.notifyCritical) } }
    var appearance: AppAppearance {
        didSet { defaults.set(appearance.rawValue, forKey: Keys.appearance) }
    }

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        stationIDsByScope = Self.decode(
            [String: [String]].self,
            from: defaults.data(forKey: Keys.stationIDsByScope)
        ) ?? [:]
        labelsByScope = Self.decode(
            [String: [String: String]].self,
            from: defaults.data(forKey: Keys.labelsByScope)
        ) ?? [:]
        audienceByServer = Self.decode(
            [String: String].self,
            from: defaults.data(forKey: Keys.audienceByServer)
        ) ?? [:]
        legacyStationIDs = Self.decode([String].self, from: defaults.data(forKey: Keys.stationIDs)) ?? []
        legacyLabels = Self.decode([String: String].self, from: defaults.data(forKey: Keys.labels)) ?? [:]
        serverURLString = defaults.string(forKey: Keys.serverURL) ?? ""
        stationIDs = []
        labelsByStationID = [:]
        notifyOffline = defaults.object(forKey: Keys.notifyOffline) as? Bool ?? true
        notifyRF = defaults.object(forKey: Keys.notifyRF) as? Bool ?? true
        notifyCritical = defaults.object(forKey: Keys.notifyCritical) as? Bool ?? true
        appearance = AppAppearance(rawValue: defaults.string(forKey: Keys.appearance) ?? "") ?? .dark

    }

    func label(for stationID: String) -> String? {
        labelsByStationID[stationID].flatMap { $0.isEmpty ? nil : $0 }
    }

    func activateScope(_ key: AudienceCacheKey) {
        let scopeID = key.preferenceID
        guard activeScopeID != scopeID else { return }

        isActivatingScope = true
        defer { isActivatingScope = false }
        if let activeScopeID {
            stationIDsByScope[activeScopeID] = stationIDs
            labelsByScope[activeScopeID] = labelsByStationID
        }

        activeScopeID = scopeID
        if stationIDsByScope[scopeID] == nil,
           !legacyStationIDs.isEmpty || !legacyLabels.isEmpty {
            stationIDsByScope[scopeID] = legacyStationIDs
            labelsByScope[scopeID] = legacyLabels
            legacyStationIDs = []
            legacyLabels = [:]
            defaults.removeObject(forKey: Keys.stationIDs)
            defaults.removeObject(forKey: Keys.labels)
        }
        stationIDs = stationIDsByScope[scopeID] ?? []
        labelsByStationID = labelsByScope[scopeID] ?? [:]
        save(stationIDsByScope, key: Keys.stationIDsByScope)
        save(labelsByScope, key: Keys.labelsByScope)
    }

    func clearScope(_ key: AudienceCacheKey) {
        let scopeID = key.preferenceID
        stationIDsByScope.removeValue(forKey: scopeID)
        labelsByScope.removeValue(forKey: scopeID)
        if activeScopeID == scopeID {
            isActivatingScope = true
            stationIDs = []
            labelsByStationID = [:]
            isActivatingScope = false
        }
        save(stationIDsByScope, key: Keys.stationIDsByScope)
        save(labelsByScope, key: Keys.labelsByScope)
    }

    func preferredAudience(forServer server: String) -> ReadAudience? {
        audienceByServer[Self.serverID(server)].flatMap(ReadAudience.init)
    }

    func setPreferredAudience(_ audience: ReadAudience, forServer server: String) {
        audienceByServer[Self.serverID(server)] = audience.rawValue
        save(audienceByServer, key: Keys.audienceByServer)
    }

    func markServerURLMigrated() {
        defaults.removeObject(forKey: Keys.serverURL)
    }

    private func persistActiveStations() {
        guard !isActivatingScope, let activeScopeID else { return }
        stationIDsByScope[activeScopeID] = stationIDs
        save(stationIDsByScope, key: Keys.stationIDsByScope)
    }

    private func persistActiveLabels() {
        guard !isActivatingScope, let activeScopeID else { return }
        labelsByScope[activeScopeID] = labelsByStationID
        save(labelsByScope, key: Keys.labelsByScope)
    }

    private func save<T: Encodable>(_ value: T, key: String) {
        defaults.set(try? JSONEncoder().encode(value), forKey: key)
    }

    private static func decode<T: Decodable>(_ type: T.Type, from data: Data?) -> T? {
        guard let data else { return nil }
        return try? JSONDecoder().decode(type, from: data)
    }

    private static func serverID(_ server: String) -> String {
        SHA256.hash(data: Data(server.utf8)).map { String(format: "%02x", $0) }.joined()
    }

    private enum Keys {
        static let serverURL = "serverURL"
        static let stationIDs = "stationIDs"
        static let labels = "stationLabels"
        static let stationIDsByScope = "stationIDsByReadScopeV1"
        static let labelsByScope = "stationLabelsByReadScopeV1"
        static let audienceByServer = "preferredAudienceByServerV1"
        static let notifyOffline = "notifyOffline"
        static let notifyRF = "notifyRF"
        static let notifyCritical = "notifyCritical"
        static let appearance = "appearance"
    }
}
