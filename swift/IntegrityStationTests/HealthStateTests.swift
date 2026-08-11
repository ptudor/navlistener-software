import Testing
@testable import IntegrityStation

@Test
func healthUsesServedLivenessAndSeverityOrder() {
    #expect(HealthState.station(lastSeenSeconds: nil, activeEventSeverities: []) == .unknown)
    #expect(HealthState.station(lastSeenSeconds: 301, activeEventSeverities: [.critical]) == .offline)
    #expect(HealthState.station(lastSeenSeconds: 300, activeEventSeverities: [.critical]) == .critical)
    #expect(HealthState.station(lastSeenSeconds: 1, activeEventSeverities: [.info, .warning]) == .warning)
    #expect(HealthState.station(lastSeenSeconds: 1, activeEventSeverities: [.info]) == .ok)
}

@Test
func healthRollupIsWorstOf() {
    #expect(HealthState.rollup([.ok, .warning, .critical]) == .critical)
    #expect(HealthState.rollup([.ok, .offline, .warning]) == .offline)
    #expect(HealthState.rollup([HealthState]()) == .unknown)
}

@Test
func healthTransitionsOnlyOnChange() {
    #expect(HealthState.warning.transition(from: .warning) == nil)
    #expect(HealthState.ok.transition(from: .offline) == HealthTransition(from: .offline, to: .ok))
    #expect(HealthState.ok.transition(from: .offline)?.isRecovery == true)
    #expect(HealthState.critical.transition(from: .warning)?.isRecovery == false)
}
