import Foundation
import Testing
@testable import IntegrityStation

@Test
func observerDecodingKeepsAbsentFieldsUnknown() throws {
    let data = Data(#"""
    {
      "ok": true,
      "time": "2026-08-10T12:00:00Z",
      "data": {
        "schema": "2.0",
        "observers": [{
          "id": "00-04-a3-ff-fe-12-34-56",
          "remark": "roof",
          "last_seen": 1786388398,
          "rf": {
            "rf_trust": 0.75,
            "bands": [{"block": 0, "noise_level": 23, "jam_state": 1}]
          },
          "svs": {
            "J03@0": {"name": "J03", "gnssid": 5, "sigid": 0, "cn0_db_hz": 44}
          }
        }]
      }
    }
    """#.utf8)

    let envelope = try JSONDecoder().decode(APIEnvelope<ObserversPayload>.self, from: data)
    let observer = try #require(envelope.data?.observers?.first)
    #expect(observer.id == "00-04-a3-ff-fe-12-34-56")
    #expect(observer.owner == nil)
    #expect(observer.latitudeDeg == nil)
    #expect(observer.disabled == nil)
    #expect(observer.rf?.rfTrust == 0.75)
    #expect(observer.rf?.bands?.first?.noiseLevel == 23)
    #expect(observer.svs?["J03@0"]?.gnssid == 5)
    #expect(observer.svs?["J03@0"]?.used == nil)
}

@Test
func stationEventUsesOpaqueSubjectAndServerClassifiedState() throws {
    let data = Data(#"""
    {
      "id": 42,
      "time": "2026-08-10T12:00:00.125Z",
      "sv": "rx-observer16.example.invalid",
      "type": "capability_signal_lost",
      "old_value": "present",
      "new_value": "lost",
      "severity": 1,
      "params": {"station": "rx-observer16.example.invalid", "gnss": 7, "sig": 0}
    }
    """#.utf8)

    let event = try JSONDecoder().decode(GNSSAPIEvent.self, from: data)
    #expect(event.stationID == "rx-observer16.example.invalid")
    #expect(event.isActiveStationCondition == true)
    #expect(event.conditionKey == "rx-observer16.example.invalid|capability_signal_lost|7|0")
    #expect(WireDate.parse(event.time) != nil)
}

@Test
func collectorEndpointAcceptsOnlyHTTPCollectors() throws {
    let base = try #require(URL(string: "https://collector.invalid/site"))
    #expect(try CollectorEndpoint.url(baseURL: base, path: "/gnss/api/v2/observers").absoluteString
            == "https://collector.invalid/site/gnss/api/v2/observers")

    let fileURL = try #require(URL(string: "file:///tmp/collector"))
    #expect(throws: FeedError.invalidBaseURL) {
        try CollectorEndpoint.url(baseURL: fileURL, path: "gnss/api/v2/observers")
    }
}

@Test
func observersSnapshotCacheRoundTrips() async throws {
    let directory = FileManager.default.temporaryDirectory
        .appending(path: "integrity-station-tests-\(UUID().uuidString)", directoryHint: .isDirectory)
    defer { try? FileManager.default.removeItem(at: directory) }

    let json = Data(#"""
    {"schema":"2.0","observers":[{"id":"station-1","remark":"ridge"}]}
    """#.utf8)
    let payload = try JSONDecoder().decode(ObserversPayload.self, from: json)
    let snapshot = ObserversSnapshot(
        receivedAt: Date(timeIntervalSince1970: 1_786_388_400),
        serverBaseURL: "https://collector.invalid",
        serverTime: "2026-08-10T12:00:00Z",
        payload: payload
    )
    let cache = SnapshotCache(directory: directory)

    try await cache.saveObservers(snapshot)
    let loaded = try #require(try await cache.loadObservers())
    #expect(loaded.payload.schema == "2.0")
    #expect(loaded.serverBaseURL == "https://collector.invalid")
    #expect(loaded.payload.observers?.first?.id == "station-1")
    #expect(loaded.payload.observers?.first?.hwVersion == nil)
}
