import Foundation

enum ObserverSetupError: Error, LocalizedError {
    case invalidLabel, invalidConfig, incompatibleDevice, invalidResponse, saveFailed, powerCycle
    case connection, wifi, timeout

    var errorDescription: String? {
        switch self {
        case .invalidLabel: String(localized: "setup.error.label")
        case .invalidConfig: String(localized: "setup.error.config")
        case .incompatibleDevice: String(localized: "setup.error.incompatible")
        case .invalidResponse: String(localized: "setup.error.response")
        case .saveFailed: String(localized: "setup.error.save")
        case .powerCycle: String(localized: "setup.error.power_cycle")
        case .connection: String(localized: "setup.error.connection")
        case .wifi: String(localized: "setup.error.wifi")
        case .timeout: String(localized: "setup.error.timeout")
        }
    }
}

/// The physical label is a recovery credential, not a collector read token.
struct ObserverSetupLabel: Decodable, Sendable {
    let ver: String
    let name: String
    let username: String
    let pop: String
    let transport: String

    static func parse(_ json: String) throws -> Self {
        guard json.utf8.count <= 512,
              let label = try? JSONDecoder().decode(Self.self, from: Data(json.utf8))
        else { throw ObserverSetupError.invalidLabel }
        try label.validate()
        return label
    }

    func validate() throws {
        let suffix = name.dropFirst("navfeeder-".count)
        guard ver == "v1", transport == "ble", username == name,
              name.hasPrefix("navfeeder-"), suffix.utf8.count == 6,
              suffix.utf8.allSatisfy({ (48...57).contains($0) || (65...70).contains($0) }),
              pop.utf8.count == 12, pop.utf8.allSatisfy({ (33...126).contains($0) })
        else { throw ObserverSetupError.invalidLabel }
    }
}

struct ObserverSetupConfig: Sendable {
    let host: String
    let port: UInt16
    let stationID: String
    let enrollmentToken: String

    /// Exact NVF1 framing in esp32/docs/PROVISIONING.md. Never normalize tokens
    /// or opaque station IDs, and never accept URLs in the host field.
    func encoded() throws -> Data {
        let hostBytes = Array(host.utf8)
        let stationBytes = Array(stationID.utf8)
        let tokenBytes = Array(enrollmentToken.utf8)
        guard port > 0, (1...63).contains(hostBytes.count),
              (1...32).contains(stationBytes.count), (1...128).contains(tokenBytes.count),
              [hostBytes, stationBytes, tokenBytes].allSatisfy({ $0.allSatisfy { (32...126).contains($0) } }),
              !host.contains("://"), !host.contains("/"), !host.contains("@"),
              !host.contains(" "), !host.contains("?"), !host.contains("#")
        else { throw ObserverSetupError.invalidConfig }
        return Data([0x4e, 0x56, 0x46, 0x31, 0, UInt8(port >> 8), UInt8(port & 0xff),
                     UInt8(hostBytes.count), UInt8(stationBytes.count), UInt8(tokenBytes.count)]
                    + hostBytes + stationBytes + tokenBytes)
    }
}

enum ObserverSetupReply: UInt8, Sendable {
    case waitingForWiFi = 0, saved = 1, invalid = 2, saveFailed = 3, powerCycle = 4

    static func parse(_ data: Data) throws -> Self {
        let bytes = Array(data)
        guard bytes.count == 5, bytes.prefix(4).elementsEqual([0x4e, 0x56, 0x52, 0x31]),
              let reply = Self(rawValue: bytes[4]) else { throw ObserverSetupError.invalidResponse }
        return reply
    }
}

enum ObserverSetupContract {
    /// Check before disclosing the label password, and again before sending
    /// enrollment/Wi-Fi credentials; ESPProvision can negotiate older security.
    static func supports(_ version: [String: Any]?) -> Bool {
        guard let prov = version?["prov"] as? [String: Any], prov["sec_ver"] as? Int == 2,
              let app = version?["navfeeder"] as? [String: Any],
              let capabilities = app["cap"] as? [String], capabilities.contains("nav-config-v1")
        else { return false }
        return true
    }
}
