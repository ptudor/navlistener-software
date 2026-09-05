import Foundation

// Audience-scoped commit order, including resolution tombstones. A snapshot
// cursor is a floor for every condition, including ones absent from that set.
struct ConditionState {
    private struct Transition {
        let id: Int64
        let event: GNSSAPIEvent?
    }
    private var latest: [String: Transition] = [:]
    private var floor: Int64 = 0
    private var epoch: String?
    private(set) var isKnown = false
    var active: [GNSSAPIEvent] { latest.values.compactMap(\.event) }

    mutating func invalidate() { isKnown = false }

    @discardableResult mutating func apply(_ event: GNSSAPIEvent, resolved: Bool = false) throws -> Bool {
        guard let key = event.conditionKey, let id = event.id, id > floor,
              id > (latest[key]?.id ?? 0),
              resolved || event.isActiveStationCondition != nil else { return false }
        guard latest[key] != nil || latest.count < NetworkLimits.conditions else {
            isKnown = false
            throw FeedError.inputLimit
        }
        latest[key] = Transition(id: id, event: event.isActiveStationCondition == true && !resolved ? event : nil)
        return true
    }

    // Returns false after an epoch change: discard every prior-policy record,
    // then take a second snapshot while admitting only fresh live transitions.
    mutating func install(_ snapshot: ConditionsPayload) throws -> Bool {
        guard snapshot.complete, snapshot.cursor >= 0, snapshot.events.count <= NetworkLimits.conditions else {
            throw FeedError.invalidResponse
        }
        var replacement: [String: Transition] = [:]
        for event in snapshot.events {
            guard let key = event.conditionKey, let id = event.id, id > 0, id <= snapshot.cursor,
                  let active = event.isActiveStationCondition, replacement[key] == nil else {
                throw FeedError.invalidResponse
            }
            replacement[key] = Transition(id: id, event: active ? event : nil)
        }
        let changed = epoch != nil && epoch != snapshot.epoch
        if !changed {
            // A delayed poll cannot replace a newer live activation or resolution.
            guard snapshot.cursor >= floor else { return isKnown }
            for (key, transition) in latest where transition.id > snapshot.cursor {
                replacement[key] = transition
            }
        }
        guard replacement.count <= NetworkLimits.conditions else {
            isKnown = false
            throw FeedError.inputLimit
        }
        latest = replacement
        floor = snapshot.cursor
        epoch = snapshot.epoch
        isKnown = !changed
        return isKnown
    }
}
