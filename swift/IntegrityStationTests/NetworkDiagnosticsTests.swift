import Foundation
import SwiftUI
import Testing
@testable import IntegrityStation

private let networkJSON = #"""
{"received_at":"2026-10-17T12:00:00Z","sample_time":"2026-10-17T11:00:00Z",
 "session":"boot-a","sequence":42,"details":{"uptime_ms":120000,"network":{
 "wifi":{"configured":true,"associated":false,"rssi_dbm":null,"minimum_rssi_dbm":-87,"retry_count":null},
 "ethernet":{"present":true,"link_up":true,"speed_mbps":100,"duplex":"full","rx_frame_errors":null},
 "collector":{"connected":true,"socket_interface":"ethernet","tls_written_bytes":18446744073709551615},
 "counters":{"wifi_disconnects":3},"spool_dropped_records":9007199254740993,
 "events":[{"sequence":7,"uptime_ms":60000,"kind":"wifi_disconnected","code":200,"detail":-87}]
 }}}
"""#

@Test func networkPreservesMissingCountersAndEventClock() throws {
    let sample = try JSONDecoder().decode(BoardSample.self, from: Data(networkJSON.utf8))
    let network = try #require(sample.details?.network)
    #expect(network.wifi?.rssiDBM == nil)
    #expect(network.wifi?.minimumRSSIDBM == -87)
    #expect(network.ethernet?.speedMbps == 100)
    #expect(network.ethernet?.rxFrameErrors == nil)
    #expect(network.collector?.tlsWrittenBytes == UInt64.max)
    #expect(network.spoolDroppedRecords == 9_007_199_254_740_993)
    let event = try #require(network.events?.first)
    #expect(event.estimatedDate(in: sample) == WireDate.parse("2026-10-17T10:59:00Z"))
    #expect(sample.freshness(serverTime: WireDate.parse(sample.receivedAt), elapsed: 0, stale: false, cached: false, threshold: 660) == .stale)
    let roundtrip = try JSONDecoder().decode(BoardSample.self, from: JSONEncoder().encode(sample))
    #expect(roundtrip.details?.network?.collector?.tlsWrittenBytes == UInt64.max)
}

@Test func networkHistoryLeavesGapsAndSeparatesBootCounters() throws {
    let detail = try JSONDecoder().decode(BoardSample.self, from: Data(networkJSON.utf8)).details
    let a = SensorHistorySample(receivedAt: "2026-10-17T12:00:00Z", sampleTime: nil, session: "a", sequence: "1", hardwareTrust: nil, details: detail)
    let b = SensorHistorySample(receivedAt: "2026-10-17T12:01:00Z", sampleTime: nil, session: "b", sequence: "1", hardwareTrust: nil, details: detail)
    #expect(SensorMetric.networkRSSI.kind == "network")
    #expect(SensorHistoryPoint.make(samples: [a,b], metric: .networkRSSI).isEmpty)
    let points = SensorHistoryPoint.make(samples: [a,b], metric: .networkDisconnects)
    #expect(points.count == 2 && points[0].segment != points[1].segment)
}

@Test func networkAndExistingBoardSamplesDecodeOnAWorkerThread() async throws {
    let encoded = try await Task.detached {
        var payload = try #require(JSONSerialization.jsonObject(with: Data(BoardFixture.json.utf8)) as? [String: Any])
        var observers = try #require(payload["observers"] as? [[String: Any]])
        var board = try #require(observers.first?["board"] as? [String: Any])
        board["network"] = try JSONSerialization.jsonObject(with: Data(networkJSON.utf8))
        board["network_stale"] = false
        observers[0]["board"] = board
        payload["observers"] = observers
        let decoded = try JSONDecoder().decode(ObserversPayload.self, from: JSONSerialization.data(withJSONObject: payload))
        try decoded.validate()
        #expect(decoded.observers?.first?.board?.network?.details?.network?.collector?.tlsWrittenBytes == UInt64.max)
        #expect(decoded.observers?.first?.board?.timing?.details?.timing?.gnss?.validHardwarePulses == 9_007_199_254_740_993)
        return try JSONEncoder().encode(decoded)
    }.value
    let roundtrip = try JSONDecoder().decode(ObserversPayload.self, from: encoded)
    #expect(roundtrip.observers?.first?.board?.network?.session == "boot-a")
    #expect(roundtrip.observers?.first?.board?.networkStale == false)
}

@MainActor @Test func networkCardRendersAtPhoneWidth() throws {
    let sample = try JSONDecoder().decode(BoardSample.self, from: Data(networkJSON.utf8))
    let network = try #require(sample.details?.network)
    let renderer = ImageRenderer(content: InstrumentCard("network.title", systemImage: "network") {
        NetworkDiagnosticsView(network: network, sample: sample, observerID: "observer-test", showHistory: false)
    }.environment(\.colorScheme, .dark).frame(width: 390).padding(16))
    let image = try #require(renderer.cgImage)
    #expect(image.width >= 390 && image.height > 200)
    #if os(macOS)
    let png = try #require(NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:]))
    try png.write(to: FileManager.default.temporaryDirectory.appending(path: "navlisten-network-review.png"))
    #endif
}
