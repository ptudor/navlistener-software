import Foundation

/// Server-authored severity values. Keep the raw values aligned with
/// docs/OUTPUT.md §2.2; the client does not reinterpret event severity.
enum EventSeverity: Int, Codable, CaseIterable, Sendable {
    case info = 0
    case warning = 1
    case critical = 2
}

/// Display-side station health. Verdicts still come from the served liveness
/// and event state; this type only performs the app's documented rollup.
enum HealthState: String, Codable, CaseIterable, Sendable {
    case unknown
    case offline
    case critical
    case warning
    case ok

    /// ObserverOfflineThreshold from docs/INTEGRITY.md §2. This mirrors the
    /// server contract; it is not an independently chosen client threshold.
    static let observerOfflineThreshold: TimeInterval = 300

    /// Offline takes precedence over alarms, and alarms over unknown liveness:
    /// a station whose contact age the feed does not establish (absent,
    /// non-finite or negative) still renders the conditions the collector has
    /// classified for it, including its own `station_offline`, so a missing
    /// liveness field never hides a critical condition. Only a station with
    /// neither liveness nor an active condition is unknown.
    static func station(
        lastSeenSeconds: TimeInterval?,
        activeEventSeverities: some Sequence<EventSeverity>,
        conditionsKnown: Bool = true,
        receptionAlarm: Bool = false
    ) -> HealthState {
        let age = lastSeenSeconds.flatMap { $0.isFinite && $0 >= 0 ? $0 : nil }
        if let age, age > observerOfflineThreshold { return .offline }

        guard conditionsKnown else { return .unknown }
        var hasWarning = receptionAlarm
        for severity in activeEventSeverities {
            if severity == .critical { return .critical }
            if severity == .warning { hasWarning = true }
        }
        if hasWarning { return .warning }
        return age == nil ? .unknown : .ok
    }

    /// Health states are ordered from most severe to least severe:
    /// unknown → offline → critical → warning → ok.
    static func rollup(_ states: some Sequence<HealthState>) -> HealthState {
        states.min(by: { $0.priority < $1.priority }) ?? .unknown
    }

    func transition(from previous: HealthState?) -> HealthTransition? {
        guard let previous, previous != self else { return nil }
        return HealthTransition(from: previous, to: self)
    }

    fileprivate var priority: Int {
        switch self {
        case .unknown: 0
        case .offline: 1
        case .critical: 2
        case .warning: 3
        case .ok: 4
        }
    }
}

struct HealthTransition: Equatable, Sendable {
    let from: HealthState
    let to: HealthState

    var isRecovery: Bool {
        to.priority > from.priority
    }
}
