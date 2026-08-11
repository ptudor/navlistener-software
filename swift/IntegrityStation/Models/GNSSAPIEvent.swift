import Foundation

struct EventsPayload: Codable, Sendable {
    let schema: String?
    let audience: String?
    let total: Int?
    let events: [GNSSAPIEvent]?
}

/// The shared query/SSE event object from docs/OUTPUT.md §3. `sv` is named by
/// the wire but is an opaque subject: station events carry the station id here.
struct GNSSAPIEvent: Codable, Sendable {
    let id: Int64?
    let time: String?
    let subject: String?
    let type: String?
    let oldValue: String?
    let newValue: String?
    let severity: EventSeverity?
    let message: String?
    let params: [String: JSONValue]?
    let raw: [String: JSONValue]?

    enum CodingKeys: String, CodingKey {
        case id, time, type, severity, message, params, raw
        case subject = "sv"
        case oldValue = "old_value"
        case newValue = "new_value"
    }
}

extension GNSSAPIEvent {
    private static let stationTypes: Set<String> = [
        "station_offline",
        "jamming_detected",
        "spoofing_suspected",
        "station_rf_degraded",
        "antenna_fault",
        "capability_signal_lost",
        "capability_impossible",
    ]

    var stationID: String? {
        guard let type, Self.stationTypes.contains(type) else { return nil }
        return params?["station"]?.stringValue ?? subject
    }

    /// The server classifies these state words; the client only tracks whether
    /// the latest transition for a condition is active. No metric threshold is
    /// duplicated here.
    var isActiveStationCondition: Bool? {
        guard let type, let newValue else { return nil }
        return switch type {
        case "station_offline": newValue == "offline"
        case "jamming_detected": newValue != "ok"
        case "spoofing_suspected": newValue != "ok"
        case "station_rf_degraded": newValue == "degraded"
        case "antenna_fault": newValue == "fault"
        case "capability_signal_lost": newValue == "lost"
        case "capability_impossible": newValue == "impossible"
        default: nil
        }
    }

    var conditionKey: String? {
        guard let stationID, let type else { return nil }
        let gnss = params?["gnss"]?.integerValue.map(String.init) ?? ""
        let sig = params?["sig"]?.integerValue.map(String.init) ?? ""
        return "\(stationID)|\(type)|\(gnss)|\(sig)"
    }
}

enum JSONValue: Codable, Equatable, Sendable {
    case string(String)
    case number(Double)
    case bool(Bool)
    case object([String: JSONValue])
    case array([JSONValue])
    case null

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() { self = .null }
        else if let value = try? container.decode(Bool.self) { self = .bool(value) }
        else if let value = try? container.decode(Double.self) { self = .number(value) }
        else if let value = try? container.decode(String.self) { self = .string(value) }
        else if let value = try? container.decode([String: JSONValue].self) { self = .object(value) }
        else if let value = try? container.decode([JSONValue].self) { self = .array(value) }
        else {
            throw DecodingError.dataCorruptedError(in: container, debugDescription: "Unsupported JSON value")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .string(let value): try container.encode(value)
        case .number(let value): try container.encode(value)
        case .bool(let value): try container.encode(value)
        case .object(let value): try container.encode(value)
        case .array(let value): try container.encode(value)
        case .null: try container.encodeNil()
        }
    }

    var stringValue: String? {
        guard case .string(let value) = self else { return nil }
        return value
    }

    var integerValue: Int? {
        guard case .number(let value) = self,
              value.isFinite,
              value.rounded(.towardZero) == value
        else { return nil }
        return Int(exactly: value)
    }
}
