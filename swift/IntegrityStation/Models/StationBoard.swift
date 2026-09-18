import Foundation

/// Private observer context (docs/OBSERVER-TELEMETRY.md). Components and values
/// stay optional so old collectors, unsupported hardware and failed reads remain
/// distinguishable from measured zero. Each sample replaces its components.
struct StationBoard: Codable, Sendable {
    let latest: BoardSample?
    let stale: Bool?
    let timing: BoardSample?
    let timingStale: Bool?
    var update: BoardSample? = nil
    var updateStale: Bool? = nil
    let lastInterference: BoardInterference?

    enum CodingKeys: String, CodingKey {
        case latest, stale, timing, update
        case updateStale = "update_stale"
        case timingStale = "timing_stale"
        case lastInterference = "last_interference"
    }
}

struct BoardSample: Codable, Sendable {
    let receivedAt: String?
    let sampleTime: String?
    let session: String?
    let sequence: UInt64?
    let details: BoardDetails?

    enum CodingKeys: String, CodingKey {
        case session, sequence, details
        case receivedAt = "received_at"
        case sampleTime = "sample_time"
    }

    func freshness(serverTime: Date?, elapsed: TimeInterval, stale: Bool?,
                   cached: Bool, threshold: TimeInterval) -> BoardFreshness {
        if cached || stale == true { return .stale }
        guard stale == false, let serverTime,
              let received = WireDate.parse(receivedAt), elapsed.isFinite, elapsed >= 0
        else { return .unknown }
        let receiptAge = serverTime.timeIntervalSince(received)
        guard receiptAge.isFinite, receiptAge >= 0 else { return .unknown }
        if receiptAge + elapsed > threshold { return .stale }
        guard let sample = WireDate.parse(sampleTime) else { return .receiptOnly }
        let sampleAge = serverTime.timeIntervalSince(sample)
        guard sampleAge.isFinite, sampleAge >= 0 else { return .unknown }
        return sampleAge + elapsed > threshold ? .stale : .current
    }
}

enum BoardFreshness: Sendable {
    case current, receiptOnly, stale, unknown
}

struct BoardInterference: Codable, Sendable {
    let before: BoardSample?
    let snapshot: BoardSample?
}

struct BoardDetails: Codable, Sendable {
    let version: UInt8?
    let reason: UInt8?
    let uptimeMS: UInt64?
    let eventCount: UInt32?
    let eventUptimeMS: UInt64?
    let eventFlags: UInt8?
    let eventStates: UInt8?
    let environment: BoardEnvironment?
    let rtc: BoardRTC?
    let atecc: BoardATECC?
    let eeprom: BoardEEPROM?
    let resources: BoardResources?
    let receiver: BoardReceiver?
    let firmware: String?
    let timing: BoardTiming?
    var update: BoardUpdate? = nil

    enum CodingKeys: String, CodingKey {
        case version, reason, environment, rtc, atecc, eeprom, resources, receiver, firmware, timing, update
        case uptimeMS = "uptime_ms"
        case eventCount = "event_count"
        case eventUptimeMS = "event_uptime_ms"
        case eventFlags = "event_flags"
        case eventStates = "event_states"
    }
}

struct BoardEnvironment: Codable, Sendable {
    let readyMask: UInt8?
    let mcp9808C: Double?
    let hdc2080C: Double?
    let bmp388BMP384C: Double?
    let humidityPercent: Double?
    let pressurePa: UInt32?

    enum CodingKeys: String, CodingKey {
        case readyMask = "ready_mask"
        case mcp9808C = "mcp9808_c"
        case hdc2080C = "hdc2080_c"
        case bmp388BMP384C = "bmp388_bmp384_c"
        case humidityPercent = "humidity_percent"
        case pressurePa = "pressure_pa"
    }
}

struct BoardRTC: Codable, Sendable {
    let flags: UInt8?
    let unixSeconds: UInt64?
    let sampledUptimeMS: UInt64?

    enum CodingKeys: String, CodingKey {
        case flags
        case unixSeconds = "unix_seconds"
        case sampledUptimeMS = "sampled_uptime_ms"
    }

    func hasFlag(_ flag: UInt8) -> Bool? { flags.map { $0 & flag == flag } }
}

struct BoardATECC: Codable, Sendable {
    let checkedUptimeMS: UInt64?
    let revision: String?
    let configLock: String?
    let dataLock: String?
    let rngScreening: String?

    enum CodingKeys: String, CodingKey {
        case revision
        case checkedUptimeMS = "checked_uptime_ms"
        case configLock = "config_lock"
        case dataLock = "data_lock"
        case rngScreening = "rng_screening"
    }
}

struct BoardEEPROM: Codable, Sendable {
    let action: String?
    let eui64: String?
    let capabilitiesValid: Bool?
    let revision: UInt8?
    let componentCount: UInt8?

    enum CodingKeys: String, CodingKey {
        case action, eui64, revision
        case capabilitiesValid = "capabilities_valid"
        case componentCount = "component_count"
    }
}

struct BoardResources: Codable, Sendable {
    let spoolPSRAM: Bool?
    let spoolUsedBytes: UInt32?
    let spoolCapacityBytes: UInt32?
    let spoolRecords: UInt32?
    let spoolDroppedRecords: UInt64?
    let internalFreeBytes: UInt32?
    let psramFreeBytes: UInt32?

    enum CodingKeys: String, CodingKey {
        case spoolPSRAM = "spool_psram"
        case spoolUsedBytes = "spool_used_bytes"
        case spoolCapacityBytes = "spool_capacity_bytes"
        case spoolRecords = "spool_records"
        case spoolDroppedRecords = "spool_dropped_records"
        case internalFreeBytes = "internal_free_bytes"
        case psramFreeBytes = "psram_free_bytes"
    }

    var spoolFraction: Double? {
        guard let used = spoolUsedBytes, let capacity = spoolCapacityBytes,
              capacity > 0, used <= capacity else { return nil }
        return Double(used) / Double(capacity)
    }
}

struct BoardReceiver: Codable, Sendable {
    let supportedMask: UInt8?
    let expectedMask: UInt8?
    let tracked: [UInt8]?
    let validMask: UInt8?
    let jammingState: UInt8?
    let spoofingState: UInt8?
    let rfUptimeMS: UInt64?
    let statusUptimeMS: UInt64?

    enum CodingKeys: String, CodingKey {
        case tracked
        case supportedMask = "supported_mask"
        case expectedMask = "expected_mask"
        case validMask = "valid_mask"
        case jammingState = "jamming_state"
        case spoofingState = "spoofing_state"
        case rfUptimeMS = "rf_uptime_ms"
        case statusUptimeMS = "status_uptime_ms"
    }
}

struct BoardTiming: Codable, Sendable {
    let clock: String?
    let resolutionHz: UInt32?
    let startedUptimeMS: UInt64?
    let elapsedMS: UInt64?
    let captureQueueDropped: UInt32?
    let rtcSquareWaveState: String?
    let rtcControl: UInt8?
    let rtcTrimRaw: UInt8?
    let rtcMinusGNSSPhaseTicks: Int32?
    let rtcMinusGNSSPhaseNS: Double?
    let nextTimepulseFlags: UInt8?
    let nextTimepulseReference: UInt8?
    let timepulseReceivedUptimeMS: UInt64?
    let gnss: BoardTimingChannel?
    let rtc: BoardTimingChannel?

    enum CodingKeys: String, CodingKey {
        case clock, gnss, rtc
        case resolutionHz = "resolution_hz"
        case startedUptimeMS = "started_uptime_ms"
        case elapsedMS = "elapsed_ms"
        case captureQueueDropped = "capture_queue_dropped"
        case rtcSquareWaveState = "rtc_square_wave_state"
        case rtcControl = "rtc_control"
        case rtcTrimRaw = "rtc_trim_raw"
        case rtcMinusGNSSPhaseTicks = "rtc_minus_gnss_phase_ticks"
        case rtcMinusGNSSPhaseNS = "rtc_minus_gnss_phase_ns"
        case nextTimepulseFlags = "next_timepulse_flags"
        case nextTimepulseReference = "next_timepulse_reference"
        case timepulseReceivedUptimeMS = "timepulse_received_uptime_ms"
    }
}

struct BoardTimingChannel: Codable, Sendable {
    let flags: UInt32?
    let periodTicks: UInt32?
    let widthTicks: UInt32?
    let minPeriodTicks: UInt32?
    let maxPeriodTicks: UInt32?
    let discontinuities: UInt32?
    let missingPulseEstimate: UInt64?
    let capturedRisingEdges: UInt64?
    let hardwarePulses: UInt64?
    let spanTicks: UInt64?
    let spanIntervals: UInt64?
    let lastRiseUptimeMS: UInt64?
    let counterDiscontinuities: UInt32?
    let periodNS: Double?
    let widthNS: Double?
    let spanPhaseNS: Double?
    let periodErrorPPM: Double?

    enum CodingKeys: String, CodingKey {
        case flags, discontinuities
        case periodTicks = "period_ticks"
        case widthTicks = "width_ticks"
        case minPeriodTicks = "min_period_ticks"
        case maxPeriodTicks = "max_period_ticks"
        case missingPulseEstimate = "missing_pulse_estimate"
        case capturedRisingEdges = "captured_rising_edges"
        case hardwarePulses = "hardware_pulses"
        case spanTicks = "span_ticks"
        case spanIntervals = "span_intervals"
        case lastRiseUptimeMS = "last_rise_uptime_ms"
        case counterDiscontinuities = "counter_discontinuities"
        case periodNS = "period_ns"
        case widthNS = "width_ns"
        case spanPhaseNS = "span_phase_ns"
        case periodErrorPPM = "period_error_ppm"
    }

    func hasFlags(_ mask: UInt32) -> Bool { flags.map { $0 & mask == mask } ?? false }
    var validHardwarePulses: UInt64? { hasFlags(3) ? hardwarePulses : nil }
    var validPeriodNS: Double? { hasFlags(13) ? periodNS : nil }
    var validWidthNS: Double? { hasFlags(21) ? widthNS : nil }
    var validPeriodErrorPPM: Double? { hasFlags(45) ? periodErrorPPM : nil }
}

struct BoardUpdate: Codable, Sendable {
    let mode: String?
    let channel: String?
    let state: String?
    let securityFlags: UInt8?
    let trustProfile: String?
    let partitionLayoutID: UInt16?
    let runningRelease: String?
    let availableRelease: String?
    let stagedRelease: String?
    let failedRelease: String?
    let bytesReceived: UInt32?
    let artifactLength: UInt32?
    let lastCheck: String?
    let nextCheck: String?
    let lastCommand: String?
    let error: String?
    enum CodingKeys: String, CodingKey {
        case mode, channel, state, error
        case securityFlags = "security_flags"
        case trustProfile = "trust_profile"
        case partitionLayoutID = "partition_layout_id"
        case runningRelease = "running_release"
        case availableRelease = "available_release"
        case stagedRelease = "staged_release"
        case failedRelease = "failed_release"
        case bytesReceived = "bytes_received"
        case artifactLength = "artifact_length"
        case lastCheck = "last_check"
        case nextCheck = "next_check"
        case lastCommand = "last_command"
    }
    var progress: Double? {
        guard let received = bytesReceived, let total = artifactLength, total > 0, received <= total else { return nil }
        return Double(received) / Double(total)
    }
    /// The release track this build reports following. It is the device's own
    /// label for sorting a fleet, not evidence of which firmware is running.
    var trackDescription: String? {
        switch trustProfile {
        case "trusted": String(localized: "update.track.trusted")
        case "open": String(localized: "update.track.open")
        case "test": String(localized: "update.track.test")
        default: nil
        }
    }
    /// Open and test builds belong on unlocked hardware. Only a trusted or
    /// unreported build without the complete locked profile is a development device.
    var isOpenTrack: Bool { trustProfile == "open" }
    var isDevelopmentDevice: Bool { !isOpenTrack && securityFlags != 31 }
    var stateDescription: String {
        switch state {
        case "staged": String(localized: "update.downloaded")
        case "reboot-pending", "quiescing": String(localized: "update.rebooting")
        case "confirmed": String(localized: "update.confirmed")
        case "rolled-back": String(localized: "update.rolled_back")
        case "waiting-safe": String(localized: "update.waiting_safe")
        default: state?.replacingOccurrences(of: "-", with: " ").capitalized ?? StationFormat.unknown
        }
    }
}
