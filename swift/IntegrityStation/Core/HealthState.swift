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

    static func station(
        lastSeenSeconds: TimeInterval?,
        activeEventSeverities: some Sequence<EventSeverity>
    ) -> HealthState {
        guard let lastSeenSeconds else { return .unknown }
        guard lastSeenSeconds <= observerOfflineThreshold else { return .offline }

        var hasWarning = false
        for severity in activeEventSeverities {
            if severity == .critical { return .critical }
            if severity == .warning { hasWarning = true }
        }
        return hasWarning ? .warning : .ok
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
