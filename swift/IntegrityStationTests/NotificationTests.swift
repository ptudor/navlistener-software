import Foundation
import Testing
@testable import IntegrityStation

private actor NoticeCenter: StationNotificationCenter {
    var allowed: NotificationPermission = .authorized
    var added: [StationNotice] = []
    var pending: [StationNotice] = []
    var permissionGate: ConnectionBarrier?
    var addGate: ConnectionBarrier?
    func setPermission(_ value: NotificationPermission) { allowed = value }
    func setPermissionGate(_ gate: ConnectionBarrier) { permissionGate = gate }
    func setAddGate(_ gate: ConnectionBarrier) { addGate = gate }
    func permission() async -> NotificationPermission { await permissionGate?.holdOnce(); return allowed }
    func requestPermission() -> Bool { allowed == .authorized }
    func add(_ notice: StationNotice) async throws { await addGate?.holdOnce(); added.append(notice);pending.append(notice) }
    func remove(prefix: String) { pending.removeAll {$0.id.hasPrefix(prefix)} }
}

@MainActor @Suite struct NotificationTests {
    private func event(_ id: Int64, type: String = "jamming_detected", active: Bool, severity: Int = 1) throws -> GNSSAPIEvent {
        let value = active ? (type == "station_offline" ? "offline" : type == "capability_impossible" ? "impossible" : "jammed") : "ok"
        return try JSONDecoder().decode(GNSSAPIEvent.self,from:Data("{\"id\":\(id),\"sv\":\"roof\",\"type\":\"\(type)\",\"new_value\":\"\(value)\",\"severity\":\(severity),\"message\":\"Server-authored transition\"}".utf8))
    }
    private func fixture() throws -> (StationNotifications,AppSettings,NoticeCenter,ReadSession,String) {
        let suite = "Notices.\(UUID())"
        let defaults = try #require(UserDefaults(suiteName:suite))
        let settings = AppSettings(defaults:defaults);settings.stationIDs = ["roof"]
        let center = NoticeCenter();let service = StationNotifications(settings:settings,center:center)
        let session = try #require(ReadSession(baseURL:URL(string:"https://notice.invalid")!,principalID:"reader",audience:ReadAudience("organization:org")!,authorizationRevision:"a",token:"test-read-token"))
        service.activate(session);service.setForeground(true)
        return (service,settings,center,session,suite)
    }
    private func settle() async throws { try await Task.sleep(for:.milliseconds(50)) }

    @Test func acceptedRaisesAndRecoveriesNotifyOnce() async throws {
        let (service,_,center,session,suite) = try fixture()
        defer {UserDefaults(suiteName:suite)?.removePersistentDomain(forName:suite)}
        var state = ConditionState()
        let initial = try event(1,active:false)
        #expect(try state.install(ConditionsPayload(schema:"2.0",audience:session.audience.rawValue,complete:true,epoch:"a",cursor:1,events:[initial])))
        let raised = try event(2,active:true), recovered = try event(3,active:false,severity:0)
        for event in [initial,raised,raised,recovered,recovered,raised] {
            let old = state.active.first
            if try state.apply(event),state.isKnown {service.transition(event,previous:old,active:event.isActiveStationCondition == true,session:session)}
        }
        try await settle()
        #expect(await center.added.count == 2)
        #expect(await center.added.allSatisfy {$0.body == "Server-authored transition"})
        state.invalidate()
        let replay = try event(4,active:true)
        if try state.apply(replay),state.isKnown {service.transition(replay,previous:nil,active:true,session:session)}
        try await settle();#expect(await center.added.count == 2)
    }

    @Test(arguments:["offline","rf","critical"])
    func categoryAndPermissionGates(category:String) async throws {
        let (service,settings,center,session,suite) = try fixture()
        defer {UserDefaults(suiteName:suite)?.removePersistentDomain(forName:suite)}
        settings.notifyOffline = false;settings.notifyRF = false;settings.notifyCritical = false
        let type = category == "offline" ? "station_offline" : category == "rf" ? "jamming_detected" : "capability_impossible"
        let raised = try event(1,type:type,active:true,severity:category == "critical" ? 2 : 1)
        service.transition(raised,previous:nil,active:true,session:session)
        try await settle();#expect(await center.added.isEmpty)
        switch category {case "offline":settings.notifyOffline = true;case "rf":settings.notifyRF = true;default:settings.notifyCritical = true}
        await center.setPermission(.denied)
        service.transition(raised,previous:nil,active:true,session:session)
        try await settle();#expect(await center.added.isEmpty)
        await center.setPermission(.authorized)
        service.transition(raised,previous:nil,active:true,session:session)
        try await settle();#expect(await center.added.count == 1)
        service.setForeground(false)
        service.transition(try event(2,type:type,active:true,severity:2),previous:nil,active:true,session:session)
        try await settle();#expect(await center.added.count == 1)
        #expect(await center.pending.isEmpty)
    }

    @Test(arguments:["permission","add"])
    func retiredPrivateSessionCancelsDelayedWork(boundary:String) async throws {
        let (service,_,center,session,suite) = try fixture()
        defer {UserDefaults(suiteName:suite)?.removePersistentDomain(forName:suite)}
        let gate = ConnectionBarrier()
        if boundary == "permission" {await center.setPermissionGate(gate)} else {await center.setAddGate(gate)}
        service.transition(try event(1,active:true),previous:nil,active:true,session:session)
        await gate.waitUntilEntered()
        service.retire()
        let next = try #require(ReadSession(baseURL:session.baseURL,principalID:nil,audience:.publicAudience,authorizationRevision:"public",token:nil))
        service.activate(next)
        await gate.release();try await settle()
        #expect(await center.pending.isEmpty)
        for notice in await center.added {#expect(!service.canPresent(notice.id))}
        if boundary == "permission" {#expect(await center.added.isEmpty)}
    }
}
