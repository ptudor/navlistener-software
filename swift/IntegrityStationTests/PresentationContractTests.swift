import Foundation
import Testing
@testable import IntegrityStation

@Test func sharedStationPresentationContract() throws {
    let url = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
        .appending(path: "Fixtures/station-presentation-v1.json")
    // BoardSample has explicit wire CodingKeys, so decode it with its own decoder.
    let document = try #require(JSONSerialization.jsonObject(with: Data(contentsOf: url)) as? [String: Any])
    let decoder = JSONDecoder()
    for row in try #require(document["freshness"] as? [[String: Any]]) {
        let sample = try decoder.decode(BoardSample.self, from: JSONSerialization.data(withJSONObject: row["sample"]!))
        let result = sample.freshness(serverTime: WireDate.parse(row["server_time"] as? String),
                                     elapsed: row["elapsed"] as! Double, stale: row["stale"] as? Bool,
                                     cached: row["cached"] as! Bool, threshold: row["threshold"] as! Double)
        #expect(result.rawValue == row["expected"] as? String, "\(row["name"]!)")
    }
    for row in try #require(document["health"] as? [[String: Any]]) {
        let result = HealthState.station(lastSeenSeconds: row["last_seen"] as? Double,
                                        activeEventSeverities: (row["severities"] as! [Int]).compactMap(EventSeverity.init),
                                        conditionsKnown: row["conditions_known"] as! Bool,
                                        receptionAlarm: row["reception_alarm"] as! Bool)
        #expect(result.rawValue == row["expected"] as? String, "\(row["name"]!)")
    }
}

@Test func collectorHardwareTrustSurvivesDecodeAndCache() throws {
    let decoder = JSONDecoder()
    for value in ["none", "open", "test", "trusted", "future-value"] {
        let sample = try decoder.decode(BoardSample.self, from: Data("{\"hardware_trust\":\"\(value)\"}".utf8))
        #expect(sample.hardwareTrust == value)
        let restored = try decoder.decode(BoardSample.self, from: JSONEncoder().encode(sample))
        #expect(restored.hardwareTrust == value)
    }
    #expect(try decoder.decode(BoardSample.self, from: Data("{}".utf8)).hardwareTrust == nil)
}
