import Foundation
import Testing
@testable import IntegrityStation

private func conditionEvent(_ id: Int64, _ value: String, station: String = "roof") throws -> GNSSAPIEvent {
    try JSONDecoder().decode(GNSSAPIEvent.self, from: Data("{\"id\":\(id),\"time\":\"2026-08-01T00:00:00Z\",\"sv\":\"\(station)\",\"type\":\"jamming_detected\",\"new_value\":\"\(value)\",\"severity\":1}".utf8))
}

@Test func conditionOrderingRetainsResolutionTombstones() throws {
    for liveFirst in [false, true] {
        var state = ConditionState()
        let old = try conditionEvent(1, "jammed")
        let active = try conditionEvent(2, "jammed")
        let resolved = try conditionEvent(3, "ok")
        if !liveFirst { state.apply(old) }
        state.apply(active); state.apply(resolved)
        if liveFirst { state.apply(old) }
        state.apply(active); state.apply(resolved) // duplicate SSE and stale poll
        #expect(state.active.isEmpty)
        let snapshot = ConditionsPayload(schema: "2.0", audience: "public", complete: true, epoch: "a", cursor: 1, events: [old])
        #expect(try state.install(snapshot))
        #expect(state.active.isEmpty) // resolution arrived during snapshot request
        state.apply(try conditionEvent(4, "jammed"))
        #expect(state.active.first?.id == 4)
        #expect(try state.install(snapshot))
        #expect(state.active.first?.id == 4)
    }
}

@Test func conditionSnapshotRestoresOldActiveAndRecoversGap() throws {
    var state = ConditionState()
    let old = try conditionEvent(1, "jammed") // well beyond recent-history horizon
    let tail = try (2...202).map { try conditionEvent(Int64($0), "ok", station: "other-\($0)") }
    #expect(!state.isKnown)
    #expect(try state.install(ConditionsPayload(schema: "2.0", audience: "public", complete: true, epoch: "a", cursor: 202, events: [old]+tail)))
    #expect(state.active.map(\.id) == [1])
    state.invalidate() // explicit replay_gap / transport loss
    #expect(!state.isKnown)
    let resolved = try conditionEvent(300, "ok")
    #expect(try state.install(ConditionsPayload(schema: "2.0", audience: "public", complete: true, epoch: "a", cursor: 300, events: [resolved]+tail)))
    #expect(state.active.isEmpty)
    state.apply(old)
    #expect(state.active.isEmpty)
    // A policy change discards pre-withdrawal transitions even if its currently
    // visible history is empty. It needs a second current-epoch snapshot.
    let next = ConditionsPayload(schema: "2.0", audience: "public", complete: true, epoch: "b", cursor: 0, events: [])
    #expect(try !state.install(next))
    #expect(try state.install(next))
    #expect(state.active.isEmpty)
}
