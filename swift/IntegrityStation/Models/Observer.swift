import Foundation

struct ObserversPayload: Codable, Sendable {
    let schema: String?
    let audience: String?
    let observers: [Observer]?
}

/// One observers-feed row (docs/OUTPUT.md §1.3). Receiver metadata, exact
/// location, RF detail, and even liveness may be absent under public policy;
/// every such field remains optional rather than acquiring a fake zero/default.
struct Observer: Codable, Identifiable, Sendable {
    let id: String
    let owner: String?
    let remark: String?
    let latitudeDeg: Double?
    let longitudeDeg: Double?
    let heightM: Double?
    let hwVersion: String?
    let swVersion: String?
    let gitHash: String?
    let vendor: String?
    let mods: String?
    let serialNo: String?
    let lastSeen: TimeInterval?
    let lastSeenS: TimeInterval?
    let uptimeS: TimeInterval?
    let clockDriftNs: Double?
    let accuracyM: Double?
    let disabled: Bool?
    let svs: [String: StationSignal]?
    let rf: StationRF?
    let capabilities: [StationCapability]?
    let declaredCapabilities: [CapabilitySignal]?
    let unexpectedCapabilities: [CapabilitySignal]?
    let missingCapabilities: [CapabilitySignal]?

    enum CodingKeys: String, CodingKey {
        case id, owner, remark, vendor, mods, disabled, svs, rf, capabilities
        case latitudeDeg = "latitude_deg"
        case longitudeDeg = "longitude_deg"
        case heightM = "height_m"
        case hwVersion = "hw_version"
        case swVersion = "sw_version"
        case gitHash = "git_hash"
        case serialNo = "serial_no"
        case lastSeen = "last_seen"
        case lastSeenS = "last_seen_s"
        case uptimeS = "uptime_s"
        case clockDriftNs = "clock_drift_ns"
        case accuracyM = "accuracy_m"
        case declaredCapabilities = "declared_capabilities"
        case unexpectedCapabilities = "unexpected_capabilities"
        case missingCapabilities = "missing_capabilities"
    }
}

/// Per-SV reception nested in an observer. `name` and the dictionary key are
/// display-only; constellation behavior keys exclusively on numeric gnssid and
/// normalized sigid (docs/OUTPUT.md §0/§1.3).
struct StationSignal: Codable, Sendable {
    let name: String?
    let fullName: String?
    let gnssid: Int?
    let svid: Int?
    let sigid: Int?
    let aziDeg: Double?
    let elevDeg: Double?
    let cn0DbHz: Int?
    let qi: Int?
    let prresM: Double?
    let used: Bool?
    let ageS: TimeInterval?
    let lastSeenS: TimeInterval?
    let deltaHz: Double?
    let deltaHzCorr: Double?
    let ionoDelayM: Double?
    let ionoModelM: Double?
    let ionoResidM: Double?
    let ionoPairSigid: Int?
    let ionoCal: Int?

    enum CodingKeys: String, CodingKey {
        case name, gnssid, svid, sigid, qi, used
        case fullName = "full_name"
        case aziDeg = "azi_deg"
        case elevDeg = "elev_deg"
        case cn0DbHz = "cn0_db_hz"
        case prresM = "prres_m"
        case ageS = "age_s"
        case lastSeenS = "last_seen_s"
        case deltaHz = "delta_hz"
        case deltaHzCorr = "delta_hz_corr"
        case ionoDelayM = "iono_delay_m"
        case ionoModelM = "iono_model_m"
        case ionoResidM = "iono_resid_m"
        case ionoPairSigid = "iono_pair_sigid"
        case ionoCal = "iono_cal"
    }
}

struct StationRF: Codable, Sendable {
    let id: String?
    let lastSeen: TimeInterval?
    let bands: [StationRFBand]?
    let cn0MeanDbHz: Double?
    let cn0ElevResidVar: Double?
    let numSats: Int?
    let rfTrust: Double?

    enum CodingKeys: String, CodingKey {
        case id, bands
        case lastSeen = "last_seen"
        case cn0MeanDbHz = "cn0_mean_db_hz"
        case cn0ElevResidVar = "cn0_elev_resid_var"
        case numSats = "num_sats"
        case rfTrust = "rf_trust"
    }
}

/// Raw receiver indicators; `noise_level` and `jam_state` deliberately carry
/// no invented unit (docs/DEFENSE-PNT.md §6).
struct StationRFBand: Codable, Sendable {
    let block: Int?
    let agc: Int?
    let agcDeparture: Double?
    let cwSuppress: Int?
    let noiseLevel: Int?
    let jamState: Int?
    let antStatus: Int?

    enum CodingKeys: String, CodingKey {
        case block, agc
        case agcDeparture = "agc_departure"
        case cwSuppress = "cw_suppress"
        case noiseLevel = "noise_level"
        case jamState = "jam_state"
        case antStatus = "ant_status"
    }
}

struct StationCapability: Codable, Sendable {
    let gnss: Int?
    let sig: Int?
    let firstSeen: TimeInterval?
    let lastSeen: TimeInterval?
    let count: UInt64?

    enum CodingKeys: String, CodingKey {
        case gnss, sig, count
        case firstSeen = "first_seen"
        case lastSeen = "last_seen"
    }
}

struct CapabilitySignal: Codable, Hashable, Sendable {
    let gnss: Int?
    let sig: Int?
}

struct ObserversSnapshot: Codable, Sendable {
    let receivedAt: Date
    let scope: AudienceCacheKey
    let serverTime: String?
    let payload: ObserversPayload
}
