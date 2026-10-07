import Foundation
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

/// A station the feed serves without liveness (no `last_seen_s`, no RF
/// telemetry) still shows the conditions the collector classified for it;
/// missing liveness only means unknown when nothing else is known either.
@Test
func unknownLivenessDoesNotHideActiveConditions() {
    #expect(HealthState.station(lastSeenSeconds: nil, activeEventSeverities: [.critical]) == .critical)
    #expect(HealthState.station(lastSeenSeconds: nil, activeEventSeverities: [.info, .warning]) == .warning)
    #expect(HealthState.station(lastSeenSeconds: nil, activeEventSeverities: [.info]) == .unknown)
    #expect(HealthState.station(lastSeenSeconds: nil, activeEventSeverities: [], receptionAlarm: true) == .warning)
    #expect(HealthState.station(lastSeenSeconds: -1, activeEventSeverities: [.critical]) == .critical)
    #expect(HealthState.station(lastSeenSeconds: .nan, activeEventSeverities: [.critical]) == .critical)
    #expect(HealthState.station(lastSeenSeconds: .infinity, activeEventSeverities: []) == .unknown)
    // Reconciliation still gates everything but offline.
    #expect(HealthState.station(lastSeenSeconds: nil, activeEventSeverities: [.critical], conditionsKnown: false) == .unknown)
    #expect(HealthState.station(lastSeenSeconds: 301, activeEventSeverities: [], conditionsKnown: false) == .offline)
}

/// The served observer row carries `last_seen_s` from the collector's receiver
/// liveness (docs/STATION-PRESENTATION.md), independently of RF telemetry, so
/// an SBF/RTCM/NTRIP station without `rf` still has a contact age.
@MainActor @Test
func servedLivenessWithoutRFTelemetryEstablishesContactAge() throws {
    let envelope = try JSONDecoder().decode(APIEnvelope<ObserversPayload>.self, from: Data(#"""
    {"ok":true,"time":"2026-10-07T12:00:00Z","data":{"schema":"2.0","audience":"public","observers":[
      {"id":"rx-sbf-1","vendor":"septentrio","remark":"SBF capture","disabled":false,"last_seen_s":12},
      {"id":"rx-ubx-1","vendor":"u-blox","remark":"RF telemetry","disabled":false,"last_seen_s":3,
       "rf":{"last_seen":1791460797,"rf_trust":0.9}},
      {"id":"rx-silent","vendor":"septentrio","remark":"never heard","disabled":false}
    ]}}
    """#.utf8))
    let payload = try #require(envelope.data)
    let key = AudienceCacheKey(server: "https://collector.invalid", principal: AudienceCacheKey.anonymousPrincipal,
                               audience: .publicAudience, authorizationRevision: "public")
    let store = StationStore()
    try store.apply(ObserversSnapshot(receivedAt: Date(), scope: key, serverTime: envelope.time, payload: payload), cached: false)
    #expect(try #require(store.currentLastSeenAge(for: "rx-sbf-1")) >= 12)
    #expect(try #require(store.currentLastSeenAge(for: "rx-ubx-1")) >= 3)
    #expect(store.currentLastSeenAge(for: "rx-silent") == nil)
    #expect(store.health(for: "rx-sbf-1") == .unknown) // conditions not yet reconciled
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
