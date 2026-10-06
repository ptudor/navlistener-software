import Foundation
import Testing
@testable import IntegrityStation

/// An observers row as the collector serves it to a private audience, with a check
/// and a state word from a newer collector this build does not know.
private func assurancePayload(audience: String) -> Data {
    Data("""
    {"schema":"2.0","audience":"\(audience)","observers":[{"id":"roof","last_seen_s":2,
     "integrity":{"state":"unassured","score":-0.5,"unassured_domains":["position","receiver_clock"],
      "spoofing_indicated":true,"evaluated_at":1791200000,"engine_version":1,
      "config_hash":"sha256:0123456789abcdef0123","mode":"fixed","surveyed_position":true,
      "checks":[
       {"check":"static_position","version":2,"domain":"position","state":"unassured","candidate":"unassured",
        "since":1791199900,"evaluated_at":1791200000,"metrics":{"horizontal_error_m":412.5,"vertical_error_m":3.25},
        "thresholds":{"horizontal_unassured_m":100},"reasons":["horizontal_error"]},
       {"check":"clock_bias_drift","version":1,"domain":"receiver_clock","state":"inconsistent","candidate":"assured",
        "since":1791199800,"recovering_since":1791199950,"evaluated_at":1791200000},
       {"check":"future_check","version":1,"domain":"future_domain","state":"future_state","candidate":"future_state",
        "since":0,"evaluated_at":1791200000,"future_field":true}
      ]}}]}
    """.utf8)
}

private func assuranceEvent(_ id: Int64, _ value: String) throws -> GNSSAPIEvent {
    try JSONDecoder().decode(GNSSAPIEvent.self, from: Data("""
    {"id":\(id),"time":"2026-10-05T00:00:00Z","sv":"roof","type":"station_assurance","new_value":"\(value)",
     "severity":\(value == "unassured" ? 2 : value == "inconsistent" ? 1 : 0),
     "params":{"station":"roof","state":"\(value)","checks":[]}}
    """.utf8))
}

@Test func assessmentDecodesAndSurvivesTheSnapshotCache() throws {
    let payload = try JSONDecoder().decode(ObserversPayload.self, from: assurancePayload(audience: "organization:org"))
    try payload.validate()
    let assessment = try #require(payload.observers?.first?.integrity)
    #expect(assessment.state == "unassured")
    #expect(assessment.score == -0.5)
    #expect(assessment.unassuredDomains == ["position", "receiver_clock"])
    #expect(assessment.spoofingIndicated == true)
    #expect(assessment.engineVersion == 1)
    #expect(assessment.surveyedPosition == true)
    let checks = try #require(assessment.checks)
    #expect(checks.map(\.check) == ["static_position", "clock_bias_drift", "future_check"])
    #expect(checks[0].metrics == ["horizontal_error_m": 412.5, "vertical_error_m": 3.25])
    #expect(checks[0].reasons == ["horizontal_error"])
    #expect(checks[1].recoveringSince == 1791199950)
    #expect(checks[1].candidate == "assured")
    #expect(checks[0].recoveringSince == nil)

    let restored = try JSONDecoder().decode(ObserversPayload.self, from: JSONEncoder().encode(payload))
    let cached = try #require(restored.observers?.first?.integrity)
    #expect(cached.checks?.map(\.state) == ["unassured", "inconsistent", "future_state"])
    #expect(cached.configHash == assessment.configHash)
}

@Test func publicFeedMayNotCarryAnAssessment() throws {
    let payload = try JSONDecoder().decode(ObserversPayload.self, from: assurancePayload(audience: "public"))
    #expect(throws: FeedError.invalidResponse) { try payload.validate() }
}

@Test func assuranceStateWordsKeepUnknownVocabularyVisible() {
    #expect(AssuranceLevel.displayName("assured") == "Assured")
    #expect(AssuranceLevel.displayName("unassured") == "Unassured")
    #expect(AssuranceLevel.displayName("future_state") == "future_state")
    #expect(AssuranceLevel.displayName(nil) == StationFormat.unknown)
}

@Test func stationAssuranceIsAServerClassifiedStationCondition() throws {
    for (value, active) in [("inconsistent", true), ("unassured", true), ("assured", false)] {
        let event = try assuranceEvent(1, value)
        #expect(event.stationID == "roof")
        #expect(event.isActiveStationCondition == active, "\(value)")
    }
    var state = ConditionState()
    #expect(try state.install(ConditionsPayload(schema: "2.0", audience: "organization:org", complete: true,
                                                epoch: "a", cursor: 1, events: [try assuranceEvent(1, "unassured")])))
    #expect(state.active.map(\.id) == [1])
    try state.apply(try assuranceEvent(2, "inconsistent"))
    #expect(state.active.map(\.id) == [2])
    try state.apply(try assuranceEvent(3, "assured"))
    #expect(state.active.isEmpty)
}

@Test func assuranceEvidenceFormatting() {
    #expect(StationFormat.assuranceScore(-0.5) == "-0.50")
    #expect(StationFormat.assuranceScore(0.25) == "+0.25")
    #expect(StationFormat.assuranceScore(nil) == StationFormat.unknown)
    #expect(StationFormat.assuranceScore(.nan) == StationFormat.unknown)
    #expect(StationFormat.assuranceValues(nil) == nil)
    #expect(StationFormat.assuranceValues([:]) == nil)
    #expect(StationFormat.assuranceValues(["vertical_error_m": 3.25, "bias_ns": 123456.789, "rate_ns_per_s2": 0.05,
                                           "offset_ns": 2e9])
            == "bias_ns 123457  offset_ns 2000000000  rate_ns_per_s2 0.05  vertical_error_m 3.25")
    #expect(StationFormat.configurationHash("sha256:0123456789abcdef0123") == "sha256:0123456789ab")
    #expect(StationFormat.configurationHash("0123456789abcdef") == "0123456789ab")
    #expect(StationFormat.configurationHash(nil) == StationFormat.unknown)
    #expect(StationFormat.configurationHash("") == StationFormat.unknown)
}
