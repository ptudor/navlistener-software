import Foundation

enum ObserverSetupError: Error, LocalizedError {
    case invalidLabel, invalidConfig, invalidTunnel, incompatibleDevice, invalidResponse, saveFailed, powerCycle
    case tunnelUnsupported, tunnelTooLate, connection, wifi, timeout

    var errorDescription: String? {
        switch self {
        case .invalidLabel: String(localized: "setup.error.label")
        case .invalidConfig: String(localized: "setup.error.config")
        case .invalidTunnel: String(localized: "setup.error.tunnel")
        case .incompatibleDevice: String(localized: "setup.error.incompatible")
        case .invalidResponse: String(localized: "setup.error.response")
        case .saveFailed: String(localized: "setup.error.save")
        case .powerCycle: String(localized: "setup.error.power_cycle")
        case .tunnelUnsupported: String(localized: "setup.error.tunnel_unsupported")
        case .tunnelTooLate: String(localized: "setup.error.tunnel_late")
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
    case waitingForWiFi = 0, saved = 1, invalid = 2, saveFailed = 3, powerCycle = 4, alreadySaved = 5

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

    /// True only when the device advertises the WireGuard capability. A build without it
    /// must never be sent a profile: the tunnel endpoint would not exist and the private
    /// key would be lost, so the app hides the field instead.
    static func supportsTunnel(_ version: [String: Any]?) -> Bool {
        guard supports(version), let app = version?["navfeeder"] as? [String: Any],
              let capabilities = app["cap"] as? [String], capabilities.contains("nav-tunnel-v1")
        else { return false }
        return true
    }
}

/// The optional WireGuard profile, parsed in the app from a pasted wg-quick(8) `.conf` so
/// the private key never leaves the phone except inside the encrypted Security 2 session.
/// The parse rules and the `nav-tunnel` (NVT1) byte layout mirror the firmware exactly
/// (`esp32/components/netcfg/src/netcfg_tunnel.c`, `esp32/docs/PROVISIONING.md`): one
/// `[Interface]` and one `[Peer]`, IPv4 only, and `AllowedIPs` naming exactly one /32, the
/// collector's address inside the tunnel.
struct ObserverTunnelConfig: Sendable, Equatable {
    var privateKey: Data       // 32 bytes
    var peerPublicKey: Data    // 32 bytes
    var presharedKey: Data     // 32 bytes, all-zero means none
    var endpointHost: String
    var endpointPort: UInt16
    var address: (UInt8, UInt8, UInt8, UInt8)
    var prefix: UInt8          // 1...32
    var collector: (UInt8, UInt8, UInt8, UInt8)
    var keepalive: UInt16      // 0 = off

    static func == (a: Self, b: Self) -> Bool {
        a.privateKey == b.privateKey && a.peerPublicKey == b.peerPublicKey &&
        a.presharedKey == b.presharedKey && a.endpointHost == b.endpointHost &&
        a.endpointPort == b.endpointPort && a.address == b.address && a.prefix == b.prefix &&
        a.collector == b.collector && a.keepalive == b.keepalive
    }

    /// Canonical WireGuard base64: exactly 44 characters, '=' padded, trailing bits zero,
    /// standard alphabet. wg(8) refuses anything else and so does the firmware, so the app
    /// does too, rather than sending a key the device will reject.
    static func decodeKey(_ text: String) throws -> Data {
        let bytes = Array(text.utf8)
        guard bytes.count == 44, bytes[43] == UInt8(ascii: "="),
              !text.contains("-"), !text.contains("_"),
              let data = Data(base64Encoded: text), data.count == 32 else {
            throw ObserverSetupError.invalidTunnel
        }
        return data
    }

    private static func parseIPv4(_ text: Substring) -> (UInt8, UInt8, UInt8, UInt8)? {
        let parts = text.split(separator: ".", omittingEmptySubsequences: false)
        guard parts.count == 4 else { return nil }
        var octets: [UInt8] = []
        for part in parts {
            guard (1...3).contains(part.count), part.allSatisfy(\.isNumber),
                  part.count == 1 || part.first != "0", let value = UInt8(part) else { return nil }
            octets.append(value)
        }
        return (octets[0], octets[1], octets[2], octets[3])
    }

    private static func parseCIDR(_ text: Substring, defaultPrefix: UInt8) -> ((UInt8, UInt8, UInt8, UInt8), UInt8)? {
        let parts = text.split(separator: "/", omittingEmptySubsequences: false)
        guard let ip = parseIPv4(parts[0]) else { return nil }
        if parts.count == 1 { return (ip, defaultPrefix) }
        guard parts.count == 2, let p = UInt8(parts[1]), p <= 32 else { return nil }
        return (ip, p)
    }

    static func parse(_ text: String) throws -> Self {
        guard text.utf8.count < 1024 else { throw ObserverSetupError.invalidTunnel }
        var section = ""
        var priv: Data?, pub: Data?, psk = Data(count: 32)
        var host: String?, port: UInt16?
        var addr: (UInt8, UInt8, UInt8, UInt8)?, prefix: UInt8 = 32
        var collector: (UInt8, UInt8, UInt8, UInt8)?
        var keepalive: UInt16 = 0
        var seen = Set<String>()
        let ignoredInterface: Set<String> = ["listenport", "dns", "mtu", "table", "fwmark",
            "preup", "postup", "predown", "postdown", "saveconfig"]

        for rawLine in text.split(separator: "\n", omittingEmptySubsequences: false) {
            var line = rawLine
            if let hash = line.firstIndex(of: "#") { line = line[line.startIndex..<hash] }
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.isEmpty { continue }
            if trimmed.hasPrefix("[") {
                guard trimmed.hasSuffix("]") else { throw ObserverSetupError.invalidTunnel }
                let name = trimmed.dropFirst().dropLast().lowercased()
                guard name == "interface" || name == "peer", !seen.contains("[\(name)]")
                else { throw ObserverSetupError.invalidTunnel }
                seen.insert("[\(name)]")
                section = name
                continue
            }
            guard let eq = trimmed.firstIndex(of: "=") else { throw ObserverSetupError.invalidTunnel }
            let key = trimmed[trimmed.startIndex..<eq].trimmingCharacters(in: .whitespaces).lowercased()
            let value = trimmed[trimmed.index(after: eq)...].trimmingCharacters(in: .whitespaces)
            guard !key.isEmpty, !value.isEmpty, !seen.contains("\(section).\(key)")
            else { throw ObserverSetupError.invalidTunnel }
            seen.insert("\(section).\(key)")

            switch (section, key) {
            case ("interface", "privatekey"): priv = try decodeKey(value)
            case ("interface", "address"):
                guard !value.contains(","), let (ip, p) = parseCIDR(value[...], defaultPrefix: 32),
                      (1...32).contains(p) else { throw ObserverSetupError.invalidTunnel }
                addr = ip; prefix = p
            case ("interface", let k) where ignoredInterface.contains(k): break
            case ("peer", "publickey"): pub = try decodeKey(value)
            case ("peer", "presharedkey"): psk = try decodeKey(value)
            case ("peer", "endpoint"):
                guard let colon = value.lastIndex(of: ":") else { throw ObserverSetupError.invalidTunnel }
                let h = String(value[value.startIndex..<colon])
                guard !h.contains(":"), isHost(h), let p = UInt16(value[value.index(after: colon)...]), p > 0
                else { throw ObserverSetupError.invalidTunnel }
                host = h; port = p
            case ("peer", "allowedips"):
                guard !value.contains(","), let (ip, p) = parseCIDR(value[...], defaultPrefix: 32), p == 32
                else { throw ObserverSetupError.invalidTunnel }
                collector = ip
            case ("peer", "persistentkeepalive"):
                if value.lowercased() == "off" { keepalive = 0 }
                else { guard let k = UInt16(value) else { throw ObserverSetupError.invalidTunnel }; keepalive = k }
            default: throw ObserverSetupError.invalidTunnel
            }
        }
        guard let priv, let pub, let host, let port, let addr, let collector
        else { throw ObserverSetupError.invalidTunnel }
        let config = Self(privateKey: priv, peerPublicKey: pub, presharedKey: psk,
                          endpointHost: host, endpointPort: port, address: addr, prefix: prefix,
                          collector: collector, keepalive: keepalive)
        _ = try config.encoded() // enforce the shared validation before returning
        return config
    }

    private static func isHost(_ s: String) -> Bool {
        let bytes = Array(s.utf8)
        guard (1...63).contains(bytes.count), s.first != ".", s.last != "." else { return false }
        return bytes.allSatisfy { (48...57).contains($0) || (65...90).contains($0) ||
                                  (97...122).contains($0) || $0 == UInt8(ascii: "-") || $0 == UInt8(ascii: ".") }
    }

    /// Exact NVT1 framing. Validates the same rule the firmware applies before storing.
    func encoded() throws -> Data {
        let hostBytes = Array(endpointHost.utf8)
        let zero = Data(count: 32)
        guard privateKey.count == 32, privateKey != zero,
              peerPublicKey.count == 32, peerPublicKey != zero,
              presharedKey.count == 32, (1...63).contains(hostBytes.count),
              Self.isHost(endpointHost), endpointPort > 0, (1...32).contains(prefix),
              address != (0, 0, 0, 0), collector != (0, 0, 0, 0), address != collector
        else { throw ObserverSetupError.invalidTunnel }
        var data = Data([0x4e, 0x56, 0x54, 0x31, 0])
        data.append(privateKey)
        data.append(peerPublicKey)
        data.append(presharedKey)
        data.append(contentsOf: [address.0, address.1, address.2, address.3, prefix,
                                 collector.0, collector.1, collector.2, collector.3,
                                 UInt8(endpointPort >> 8), UInt8(endpointPort & 0xff),
                                 UInt8(keepalive >> 8), UInt8(keepalive & 0xff),
                                 UInt8(hostBytes.count)])
        data.append(contentsOf: hostBytes)
        return data
    }
}

