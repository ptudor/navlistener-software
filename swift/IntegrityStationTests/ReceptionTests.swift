import Foundation
import Testing
@testable import IntegrityStation

@Test func receptionCountsAndIndependentVerdicts() throws {
    let json = #"""
    {"reception_stale":false,"reception":{
      "received_at":"2026-09-26T12:00:00Z","details":{"reception":{
        "expectation_id":"18446744073709551615","sample_unix":1790424000,
        "alarm_mask":1,"valid_mask":1,
        "expected":[12,0,0,0,0,0,0,0],"observed":[2,0,0,0,0,0,0,0]}},
      "collector_reception":{"alarm_mask":0,"valid_mask":1,"disagreement_mask":1,"per_signal":false,
        "expected":[12,0,0,0,0,0,0,0],"observed":[2,0,0,0,0,0,0,0]}}}
    """#
    let board = try JSONDecoder().decode(StationBoard.self, from: Data(json.utf8))
    let edge = try #require(board.reception?.details?.reception)
    #expect(edge.expectationID == "18446744073709551615")
    #expect(edge.counts(0) == "2 / 12")
    #expect(edge.counts(8) == nil)
    #expect(edge.stateKey(0, current: true) == "reception.alarm")
    #expect(edge.stateKey(0, current: false) == "reception.held")
    #expect(edge.stateKey(2, current: true) == "board.freshness.unknown")
    #expect(board.hasReceptionAlarm)
    #expect(board.reception?.collectorReception?.stateKey(0, current: true) == "reception.no_alarm")
    #expect(board.reception?.collectorReception?.stateKey(0, current: false) == "board.freshness.unknown")
    let roundTrip = try JSONDecoder().decode(StationBoard.self, from: JSONEncoder().encode(board))
    #expect(roundTrip.hasReceptionAlarm)
    let malformed = try JSONDecoder().decode(ReceptionAssessment.self, from: Data(#"{"expected":[12],"observed":[2]}"#.utf8))
    #expect(malformed.counts(0) == nil)
}
