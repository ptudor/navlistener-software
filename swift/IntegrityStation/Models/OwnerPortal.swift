import CryptoKit
import Foundation
import Security

enum PortalErrorMessage {
    static func describe(_ error: Error) -> String {
        switch error {
        case FeedError.invalidBaseURL: String(localized: "portal.invalid_url")
        case FeedError.invalidResponse: String(localized: "portal.invalid_response")
        case FeedError.http(409): String(localized: "history.changed")
        case FeedError.http(let code): String(format: String(localized: "portal.http"), code)
        default: error.localizedDescription
        }
    }
}

struct PortalInstallation: Codable, Hashable, Sendable {
    let url: URL
    init(_ text: String) throws {
        guard var parts = URLComponents(string: text.trimmingCharacters(in: .whitespacesAndNewlines)),
              parts.scheme?.lowercased() == "https", let host = parts.host, !host.isEmpty,
              parts.user == nil, parts.password == nil, parts.query == nil, parts.fragment == nil,
              parts.port != 0, parts.percentEncodedPath == parts.path,
              parts.path.isEmpty || parts.path == "/" || parts.path.range(of: #"^/(?:[A-Za-z0-9][A-Za-z0-9_-]*/)*[A-Za-z0-9][A-Za-z0-9_-]*/?$"#, options: .regularExpression) != nil
        else { throw FeedError.invalidBaseURL }
        parts.scheme = "https"; parts.host = host.lowercased()
        if parts.port == 443 { parts.port = nil }
        parts.path = parts.path.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        parts.path = parts.path.isEmpty ? "/" : "/" + parts.path + "/"
        guard let url = parts.url else { throw FeedError.invalidBaseURL }
        self.url = url
    }
    func endpoint(_ path: String) -> URL { url.appending(path: path) }
}

struct PortalAuthorization: Sendable {
    static let clientID = "net.intsat.station"
    static let callback = "net.intsat.station:/portal-authorized"
    let installation: PortalInstallation
    let verifier: String
    let state: String
    init(installation: PortalInstallation) throws {
        self.installation = installation
        verifier = try Self.random(); state = try Self.random()
    }
    static func challenge(_ verifier: String) -> String { Data(SHA256.hash(data: Data(verifier.utf8))).base64URL }
    private static func random() throws -> String {
        var bytes = [UInt8](repeating: 0, count: 32)
        guard SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes) == errSecSuccess else { throw FeedError.invalidCredential }
        return Data(bytes).base64URL
    }
    var url: URL {
        var components = URLComponents(url: installation.endpoint("accounts/native/authorize/"), resolvingAgainstBaseURL: false)!
        components.queryItems = [URLQueryItem(name: "client_id", value: Self.clientID), URLQueryItem(name: "redirect_uri", value: Self.callback),
            URLQueryItem(name: "response_type", value: "code"), URLQueryItem(name: "code_challenge_method", value: "S256"),
            URLQueryItem(name: "code_challenge", value: Self.challenge(verifier)), URLQueryItem(name: "state", value: state)]
        return components.url!
    }
    func code(from callback: URL) throws -> String {
        guard let parts = URLComponents(url: callback, resolvingAgainstBaseURL: false),
              parts.scheme == Self.clientID, parts.host == nil, parts.path == "/portal-authorized", parts.fragment == nil
        else { throw FeedError.invalidResponse }
        let items = parts.queryItems ?? []
        guard Set(items.map(\.name)).count == items.count,
              items.first(where: { $0.name == "state" })?.value == state,
              items.first(where: { $0.name == "iss" })?.value == installation.url.absoluteString
        else { throw FeedError.invalidResponse }
        if items.first(where: { $0.name == "error" })?.value == "access_denied" { throw CancellationError() }
        guard Set(items.map(\.name)) == ["code", "state", "iss"],
              let code = items.first(where: { $0.name == "code" })?.value, Self.validToken(code)
        else { throw FeedError.invalidResponse }
        return code
    }
    static func validToken(_ text: String) -> Bool { text.range(of: #"^[A-Za-z0-9_-]{43}$"#, options: .regularExpression) != nil }
}
private extension Data { var base64URL: String { base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "") } }

struct PortalConnection: Codable, Equatable, Sendable {
    let installation: PortalInstallation
    let token: String
    let sessionID: UUID
    let accountID: String
    let expiresAt: Date
}
struct PortalToken: Decodable, Sendable {
    let accessToken: String, tokenType: String, issuer: String, scope: String
    let expiresIn: Int
    let sessionId: UUID
}
struct PortalIdentity: Decodable, Sendable {
    struct User: Decodable, Sendable { let id: String, name: String, email: String }
    let version: Int
    let user: User
    let sessionId: UUID
    let fleets: [PortalFleet]
}
struct PortalFleet: Codable, Hashable, Identifiable, Sendable {
    let id: UUID
    let kind: String, name: String, role: String
    var path: String { "api/v1/fleet/\(kind)/\(id.uuidString.lowercased())/" }
}
struct PortalStation: Decodable, Identifiable, Hashable, Sendable {
    let id: UUID, collectorId: UUID
    let collectorName: String, observerId: String, label: String, site: String
    let enabled: Bool
    let model: String, boardRevision: String, receiverModel: String, serial: String, antenna: String, components: String, factsSource: String
    let factsRecordedAt: String?, lastContact: String?
    var path: String { "stations/\(id.uuidString.lowercased())/" }
    var facts: [(String, String)] {
        [(String(localized: "portal.observer"), observerId), (String(localized: "portal.collector"), collectorName),
         (String(localized: "portal.site"), site), (String(localized: "portal.model"), model),
         (String(localized: "portal.board_revision"), boardRevision), (String(localized: "portal.receiver"), receiverModel),
         (String(localized: "portal.serial"), serial), (String(localized: "portal.antenna"), antenna),
         (String(localized: "portal.components"), components), (String(localized: "portal.source"), factsSource)]
    }
}
struct PortalInventory: Decodable, Sendable { let stations: [PortalStation]; let revision: String; let nextOffset: Int? }
struct PortalStationRecord: Decodable, Sendable { let station: PortalStation }
struct PortalReadings: Decodable, Sendable { let stations: [PortalReading]; let collectorTime: String? }
struct PortalStationResponse: Decodable, Sendable { let station: PortalReading; let collectorTime: String? }
struct PortalReading: Decodable, Identifiable, Sendable {
    struct Condition: Decodable, Sendable { let type: String, message: String; let severity: Int }
    let id: UUID
    let status: String, health: HealthState, boardFreshness: String
    let conditionsKnown: Bool
    let conditions: [Condition]
    let temperature: Double?, humidity: Double?, pressure: Double?
    let hardwareTrust: String?, receivedAt: String?, lastContact: String?
    let lastSeenS: Double?
    let board: PortalValue, details: PortalValue
    var statusText: String { switch status {
        case "reporting": String(localized: "portal.online")
        case "offline": String(localized: "health.offline")
        case "disabled": String(localized: "portal.disabled")
        case "waiting": String(localized: "portal.no_contact")
        default: String(localized: "health.unknown")
    } }
    func freshness(at reference: Date?, elapsed: Double, component: String = "latest") -> BoardFreshness {
        let value = board[component]
        guard value.object != nil else { return .unknown }
        let sample = BoardSample(receivedAt: value["received_at"].text, sampleTime: value["sample_time"].text,
                                 session: nil, sequence: nil, details: nil)
        let staleKey = component == "latest" ? "stale" : "\(component)_stale"
        return sample.freshness(serverTime: reference, elapsed: elapsed, stale: board[staleKey].boolean, cached: false,
                                threshold: component == "timing" ? 5 : component == "reception" ? 15 : 660)
    }
}

indirect enum PortalValue: Decodable, Sendable {
    case object([String: Self]), array([Self]), string(String), number(Double), bool(Bool), null
    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let b = try? c.decode(Bool.self) { self = .bool(b) }
        else if let s = try? c.decode(String.self) { self = .string(s) }
        else if let n = try? c.decode(Double.self), n.isFinite { self = .number(n) }
        else if let a = try? c.decode([Self].self) { self = .array(a) }
        else { self = .object(try c.decode([String: Self].self)) }
    }
    var object: [String: Self]? { if case .object(let v) = self { v } else { nil } }
    subscript(_ key: String) -> Self { object?[key] ?? .null }
    var text: String? { if case .string(let v) = self { v } else { nil } }
    var number: Double? { if case .number(let v) = self { v } else { nil } }
    var boolean: Bool? { if case .bool(let v) = self { v } else { nil } }
    var display: String {
        switch self {
        case .string(let v): v
        case .number(let v): v.formatted(.number.precision(.fractionLength(0...3)))
        case .bool(let v): String(localized: v ? "portal.yes" : "portal.no")
        case .array(let v): v.map(\.display).joined(separator: ", ")
        case .object: ""
        case .null: StationFormat.unknown
        }
    }
    var facts: [(String, String)] {
        guard let object else { return [] }
        return object.keys.sorted().flatMap { key -> [(String, String)] in
            let value = object[key]!
            if value.object != nil { return value.facts.map { (key + " · " + $0.0, $0.1) } }
            return [(key.replacingOccurrences(of: "_", with: " "), value.display)]
        }
    }
}

struct PortalHistory: Decodable, Sendable {
    struct Point: Decodable, Sendable { let time: String; let value: Double?; let session: String?, sampleTime: String?, hardwareTrust: String? }
    struct Page: Decodable, Sendable {
        let since: String, until: String, effectiveSince: String, visibleSince: String, revision: String
        let historyLimited: Bool, hasMore: Bool, paginationLimited: Bool
        let nextOffset: Int?
    }
    let points: [Point]
    let metric: String, label: String, unit: String, accessRevision: String
    let page: Page
    func chartPoints(metric: SensorMetric) -> [SensorHistoryPoint] {
        var result: [SensorHistoryPoint] = [], segment = 0
        var previous: Point?
        for (index, point) in points.enumerated() {
            guard let time = WireDate.parse(point.time), let value = point.value, value.isFinite else {
                segment += 1; previous = nil; continue
            }
            if let previous, previous.session != point.session || time.timeIntervalSince(WireDate.parse(previous.time) ?? .distantPast) > metric.gapSeconds { segment += 1 }
            result.append(SensorHistoryPoint(id: index, time: time, value: value, segment: segment)); previous = point
        }
        return result
    }
}
