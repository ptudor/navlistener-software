import Foundation
import SwiftUI
import Testing
@testable import IntegrityStation

enum BoardFixture {
    static let json = #"""
    {"schema":"2.0","audience":"organization:example","observers":[{
      "id":"observer-s3","board":{
        "stale":false,"timing_stale":false,
        "latest":{"received_at":"2026-09-14T12:00:00Z","sample_time":null,
          "session":"boot-a","sequence":18446744073709551615,"details":{
            "version":1,"reason":4,"uptime_ms":120000,"event_count":2,
            "environment":{"ready_mask":7,"mcp9808_c":0,"hdc2080_c":null,
              "bmp388_bmp384_c":23.5,"humidity_percent":null,"pressure_pa":100123},
            "rtc":{"flags":31,"unix_seconds":1789387200,"sampled_uptime_ms":119000},
            "atecc":{"checked_uptime_ms":123,"revision":"00006002","config_lock":"locked",
              "data_lock":"unlocked","rng_screening":"repeating_output"},
            "eeprom":{"action":"use_manifest","eui64":"0011223344556677",
              "capabilities_valid":true,"revision":2,"component_count":6},
            "resources":{"spool_psram":true,"spool_used_bytes":1024,"spool_capacity_bytes":4194304,
              "spool_records":100,"spool_dropped_records":18446744073709551615,
              "internal_free_bytes":32000,"psram_free_bytes":1000000},
            "firmware":"0.1.0+1.abcdef12"}},
        "timing":{"received_at":"2026-09-14T12:00:00Z","sample_time":"2026-09-14T11:59:59Z",
          "session":"boot-a","sequence":42,"details":{"version":1,"uptime_ms":121000,
            "timing":{"clock":"esp_apb","resolution_hz":80000000,"elapsed_ms":120000,
              "rtc_square_wave_state":"enabled_1hz","capture_queue_dropped":0,
              "rtc_minus_gnss_phase_ns":-125,"next_timepulse_flags":null,
              "gnss":{"flags":63,"hardware_pulses":9007199254740993,"captured_rising_edges":120,
                "period_ns":1000000025,"width_ns":100000000,"period_error_ppm":0.025,
                "missing_pulse_estimate":0,"discontinuities":0,"counter_discontinuities":0},
              "rtc":{"flags":3,"hardware_pulses":0,"period_ns":null,"width_ns":null,
                "period_error_ppm":null}}}},
        "last_interference":{"snapshot":{"session":"boot-a","details":{"event_count":2}}}
      }}]}
    """#

    static func payload() throws -> ObserversPayload {
        try JSONDecoder().decode(ObserversPayload.self, from: Data(json.utf8))
    }
}

@Test func boardFeedPreservesUnitsNullsAndFullWidthCounters() throws {
    let payload = try BoardFixture.payload()
    try payload.validate()
    let board = try #require(payload.observers?.first?.board)
    #expect(board.latest?.sequence == UInt64.max)
    #expect(board.latest?.details?.environment?.mcp9808C == 0)
    #expect(board.latest?.details?.environment?.hdc2080C == nil)
    #expect(board.latest?.details?.environment?.humidityPercent == nil)
    #expect(board.latest?.details?.environment?.pressurePa == 100123)
    #expect(board.latest?.details?.resources?.spoolDroppedRecords == UInt64.max)
    #expect(board.timing?.details?.timing?.rtcMinusGNSSPhaseNS == -125)
    #expect(board.timing?.details?.timing?.gnss?.validHardwarePulses == 9007199254740993)
    #expect(board.timing?.details?.timing?.rtc?.validHardwarePulses == 0)
    #expect(board.timing?.details?.timing?.rtc?.validPeriodNS == nil)
    #expect(board.lastInterference?.before == nil)
    let roundTrip = try JSONDecoder().decode(ObserversPayload.self, from: JSONEncoder().encode(payload))
    #expect(roundTrip.observers?.first?.board?.latest?.sequence == UInt64.max)
}

@Test func boardFreshnessAgesBothClocksWithoutTrustingUnstampedReplay() throws {
    let board = try #require(BoardFixture.payload().observers?.first?.board)
    let now = try #require(WireDate.parse("2026-09-14T12:00:00Z"))
    let sample = try #require(board.timing)
    #expect(sample.freshness(serverTime: now, elapsed: 0, stale: false, cached: false, threshold: 5) == .current)
    #expect(sample.freshness(serverTime: now, elapsed: 4, stale: false, cached: false, threshold: 5) == .current)
    #expect(sample.freshness(serverTime: now, elapsed: 4.1, stale: false, cached: false, threshold: 5) == .stale)
    #expect(sample.freshness(serverTime: now, elapsed: 0, stale: true, cached: false, threshold: 5) == .stale)
    #expect(sample.freshness(serverTime: now, elapsed: 0, stale: false, cached: true, threshold: 5) == .stale)
    #expect(sample.freshness(serverTime: nil, elapsed: 0, stale: false, cached: false, threshold: 5) == .unknown)
    #expect(sample.freshness(serverTime: now, elapsed: .nan, stale: false, cached: false, threshold: 5) == .unknown)
    #expect(sample.freshness(serverTime: now.addingTimeInterval(-10), elapsed: 0, stale: false, cached: false, threshold: 5) == .unknown)
    #expect(board.latest?.freshness(serverTime: now, elapsed: 0, stale: false, cached: false, threshold: 660) == .receiptOnly)
    #expect(board.latest?.freshness(serverTime: now, elapsed: 661, stale: false, cached: false, threshold: 660) == .stale)
}

@Test func staleTimingFlagsDoNotPromoteHistoricalNumbersToMeasurements() throws {
    let channel = try JSONDecoder().decode(BoardTimingChannel.self, from: Data(#"{"flags":1,"hardware_pulses":42,"period_ns":1000000000,"width_ns":100000000,"period_error_ppm":2}"#.utf8))
    #expect(channel.validHardwarePulses == nil)
    #expect(channel.validPeriodNS == nil)
    #expect(channel.validWidthNS == nil)
    #expect(channel.validPeriodErrorPPM == nil)
}

@Test func publicBoardPayloadIsRejectedBeforePresentationOrCaching() throws {
    let payload = try JSONDecoder().decode(ObserversPayload.self, from: Data(BoardFixture.json.replacingOccurrences(of: "organization:example", with: "public").utf8))
    #expect(throws: FeedError.invalidResponse) { try payload.validate() }
    let oldCollector = try JSONDecoder().decode(ObserversPayload.self, from: Data(#"{"audience":"public","observers":[{"id":"old"}]}"#.utf8))
    try oldCollector.validate()
    #expect(oldCollector.observers?.first?.board == nil)
}

@MainActor @Test func boardPanelsRenderAndCacheCannotAppearLive() throws {
    let payload = try BoardFixture.payload()
    let scope = AudienceCacheKey(server: "https://collector.invalid", principal: "viewer",
                                audience: ReadAudience("organization:example")!, authorizationRevision: "v1")
    let store = StationStore()
    let snapshot = ObserversSnapshot(receivedAt: Date(), scope: scope,
                                     serverTime: "2026-09-14T12:00:00Z", payload: payload)
    try store.apply(snapshot, cached: true)
    let board = try #require(store.observers.first?.board)
    #expect(store.boardFreshness(board.timing, stale: false, timing: true) == .stale)
    #expect(store.currentLastSeenAge(for: "observer-s3") == nil)
    #expect(store.health(for: "observer-s3") == .unknown)
    let suite = "BoardRender.\(UUID())"
    let defaults = try #require(UserDefaults(suiteName: suite))
    defer { defaults.removePersistentDomain(forName: suite) }
    let controller = AppController(store: store, settings: AppSettings(defaults: defaults))
    let renderer = ImageRenderer(content: BoardTelemetryView(board: board).environment(controller).frame(width: 900))
    #expect(renderer.cgImage != nil)
}

@MainActor @Test func fractionalBoardReceiptIsCompatibleWithWholeSecondEnvelopeTime() throws {
    let payload = try JSONDecoder().decode(ObserversPayload.self, from: Data(BoardFixture.json
        .replacingOccurrences(of: "2026-09-14T12:00:00Z", with: "2026-09-14T12:00:00.750Z").utf8))
    let scope = AudienceCacheKey(server: "https://collector.invalid", principal: "reader",
                                audience: ReadAudience("organization:example")!, authorizationRevision: "v1")
    let store = StationStore()
    try store.apply(ObserversSnapshot(receivedAt: Date(), scope: scope,
                                     serverTime: "2026-09-14T12:00:00Z", payload: payload), cached: false)
    let board = try #require(store.observers.first?.board)
    #expect(store.boardFreshness(board.timing, stale: false, timing: true) == .current)
    #expect(store.boardFreshness(board.latest, stale: false) == .receiptOnly)
}

@Test func reportedReleaseTrackSeparatesOpenDevicesFromUnlockedDevelopment() throws {
    func update(_ fields: String) throws -> BoardUpdate {
        try JSONDecoder().decode(BoardUpdate.self, from: Data("{\(fields)}".utf8))
    }
    let trusted = try update(#""security_flags":31,"trust_profile":"trusted""#)
    #expect(trusted.trackDescription == "Trusted" && !trusted.isOpenTrack && !trusted.isDevelopmentDevice)
    // An unlocked board on the open track is where it belongs, not a warning.
    let open = try update(#""security_flags":16,"trust_profile":"open""#)
    #expect(open.trackDescription == "Open" && open.isOpenTrack && !open.isDevelopmentDevice)
    let unlocked = try update(#""security_flags":16,"trust_profile":"trusted""#)
    #expect(!unlocked.isOpenTrack && unlocked.isDevelopmentDevice)
    let bench = try update(#""security_flags":16,"trust_profile":"test""#)
    #expect(bench.trackDescription == "Test" && bench.isDevelopmentDevice)
    // Firmware and collectors that predate the report keep the original behavior.
    for legacy in [#""security_flags":16"#, #""security_flags":16,"trust_profile":"unreported""#] {
        let value = try update(legacy)
        #expect(value.trackDescription == nil && !value.isOpenTrack && value.isDevelopmentDevice)
    }
}
