import CryptoKit
import Foundation

enum ReadAudienceKind: String, Codable, Sendable {
    case `public`
    case `operator`
    case organization
    case collection
}

/// A canonical audience returned by the collector's discovery endpoint. This
/// type validates syntax only; discovery is what grants the value to a client.
struct ReadAudience: Codable, Hashable, Identifiable, Sendable {
    static let publicAudience = ReadAudience(unchecked: "public", kind: .public)

    let rawValue: String
    let kind: ReadAudienceKind

    var id: String { rawValue }
    var isPrivate: Bool { kind != .public }

    init?(_ rawValue: String) {
        if rawValue == "public" {
            self = .publicAudience
            return
        }

        guard let separator = rawValue.firstIndex(of: ":") else { return nil }
        let kindValue = String(rawValue[..<separator])
        let identifier = String(rawValue[rawValue.index(after: separator)...])
        guard let kind = ReadAudienceKind(rawValue: kindValue),
              kind != .public,
              Self.isValidScopeID(identifier)
        else { return nil }

        self.init(unchecked: rawValue, kind: kind)
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        let rawValue = try container.decode(String.self)
        guard let value = ReadAudience(rawValue) else {
            throw DecodingError.dataCorruptedError(
                in: container,
                debugDescription: "Invalid collector audience"
            )
        }
        self = value
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        try container.encode(rawValue)
    }

    static func isValidScopeID(_ value: String) -> Bool {
        let characters = Array(value.utf8)
        guard (1...253).contains(characters.count),
              Self.isASCIIAlphaNumeric(characters[0])
        else { return false }
        return characters.dropFirst().allSatisfy {
            Self.isASCIIAlphaNumeric($0) || $0 == 46 || $0 == 95 || $0 == 58 || $0 == 45
        }
    }

    private init(unchecked rawValue: String, kind: ReadAudienceKind) {
        self.rawValue = rawValue
        self.kind = kind
    }

    private static func isASCIIAlphaNumeric(_ byte: UInt8) -> Bool {
        (48...57).contains(byte) || (65...90).contains(byte) || (97...122).contains(byte)
    }
}

struct AudienceDiscoveryPayload: Codable, Sendable {
    let schema: String?
    let principal: String?
    let revision: String?
    let audiences: [ReadAudience]?

    /// Rejects malformed or internally contradictory discovery documents. A
    /// free-form audience entered by a user never reaches a data request.
    func validated() -> AudienceDiscovery? {
        guard schema == "2.0",
              let audiences, !audiences.isEmpty,
              audiences.contains(.publicAudience),
              Set(audiences).count == audiences.count,
              let revision, ReadAudience.isValidScopeID(revision)
        else { return nil }

        let hasPrivate = audiences.contains(where: \.isPrivate)
        if hasPrivate {
            guard let principal, ReadAudience.isValidScopeID(principal) else { return nil }
        } else if let principal, !ReadAudience.isValidScopeID(principal) {
            return nil
        }

        return AudienceDiscovery(
            principalID: principal,
            authorizationRevision: revision,
            audiences: audiences.sorted { $0.rawValue < $1.rawValue }
        )
    }
}

struct AudienceDiscovery: Hashable, Sendable {
    let principalID: String?
    let authorizationRevision: String
    let audiences: [ReadAudience]
}

/// The complete local partition key required by GROUPS-AND-FEDERATION.md §6.3.
/// The digest is used only as an opaque filename/preference key; the document
/// still carries and verifies the full tuple before any payload is restored.
struct AudienceCacheKey: Codable, Hashable, Sendable {
    static let anonymousPrincipal = "anonymous"

    let server: String
    let principal: String
    let audience: ReadAudience
    let authorizationRevision: String

    var storageID: String {
        let input = "\(server)\u{0}\(principal)\u{0}\(audience.rawValue)\u{0}\(authorizationRevision)"
        return SHA256.hash(data: Data(input.utf8)).map { String(format: "%02x", $0) }.joined()
    }

    /// Presentation preferences survive a policy revision but remain isolated
    /// across servers, principals, and audiences. Payloads/cursors use storageID.
    var preferenceID: String {
        let input = "\(server)\u{0}\(principal)\u{0}\(audience.rawValue)"
        return SHA256.hash(data: Data(input.utf8)).map { String(format: "%02x", $0) }.joined()
    }
}

/// One authorized read context. Private contexts retain a read-side bearer
/// token only in memory; durable storage is provided by the Keychain service.
struct ReadSession: Hashable, Sendable {
    let baseURL: URL
    let principalID: String
    let audience: ReadAudience
    let authorizationRevision: String
    let token: String?

    var cacheKey: AudienceCacheKey {
        AudienceCacheKey(
            server: baseURL.absoluteString,
            principal: principalID,
            audience: audience,
            authorizationRevision: authorizationRevision
        )
    }

    init?(
        baseURL: URL,
        principalID: String?,
        audience: ReadAudience,
        authorizationRevision: String?,
        token: String?
    ) {
        guard let authorizationRevision,
              ReadAudience.isValidScopeID(authorizationRevision)
        else { return nil }
        if audience.isPrivate {
            guard let principalID,
                  ReadAudience.isValidScopeID(principalID),
                  let token,
                  Self.isValidToken(token)
            else { return nil }
            self.principalID = principalID
            self.authorizationRevision = authorizationRevision
            self.token = token
        } else {
            self.principalID = AudienceCacheKey.anonymousPrincipal
            self.authorizationRevision = authorizationRevision
            self.token = nil
        }
        self.baseURL = baseURL
        self.audience = audience
    }

    static func normalizedToken(_ value: String?) -> String? {
        guard let value else { return nil }
        let token = value.trimmingCharacters(in: .whitespacesAndNewlines)
        return isValidToken(token) ? token : nil
    }

    static func isValidToken(_ token: String) -> Bool {
        !token.isEmpty && token.utf8.count <= 4_096 && !token.contains(where: \.isWhitespace)
    }
}
