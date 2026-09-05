import Foundation
import Observation
import UserNotifications

enum NotificationPermission: Sendable { case unknown, notDetermined, denied, authorized }
struct StationNotice: Sendable {
    let id: String
    let title: String
    let body: String
}
protocol StationNotificationCenter: Sendable {
    func permission() async -> NotificationPermission
    func requestPermission() async throws -> Bool
    func add(_ notice: StationNotice) async throws
    func remove(prefix: String) async
}

@MainActor @Observable final class StationNotifications {
    private(set) var deliveryError: String?
    private(set) var permission: NotificationPermission = .unknown
    private let center: any StationNotificationCenter
    private let settings: AppSettings
    private var session: ReadSession?
    private var foreground = false
    private var generation = UUID().uuidString
    private var jobs: [String: Task<Void,Never>] = [:]

    init(settings: AppSettings, center: (any StationNotificationCenter)? = nil) {
        self.settings = settings
        let system = center == nil ? SystemStationNotificationCenter() : nil
        self.center = center ?? system!
        system?.canPresent = { [weak self] id in self?.canPresent(id) ?? false }
    }

    func refreshPermission() async { permission = await center.permission() }
    func requestPermission() async {
        _ = try? await center.requestPermission()
        await refreshPermission()
    }

    func activate(_ session: ReadSession) { retire(); self.session = session }
    func retire() {
        let prefix = generation + ":"
        generation = UUID().uuidString
        session = nil
        deliveryError = nil
        for job in jobs.values { job.cancel() }
        jobs.removeAll()
        let center = center
        Task { await center.remove(prefix: prefix) }
    }
    func setForeground(_ active: Bool) {
        guard active != foreground else { return }
        let current = session
        retire()
        session = current
        foreground = active
    }
    func canPresent(_ id: String) -> Bool {
        foreground && session != nil && id.hasPrefix(generation + ":")
    }

    // Only accepted live transitions after authoritative reconciliation enter
    // here. First snapshots, reconnect replay, stale history and duplicates do
    // not create a notice. Critical preference is an additional severity gate.
    func transition(_ event: GNSSAPIEvent, previous: GNSSAPIEvent?, active: Bool, session: ReadSession) {
        guard foreground, self.session == session,
              let station = event.stationID, settings.stationIDs.contains(station),
              let eventID = event.id,
              (previous != nil) != active || (active && previous?.severity != event.severity),
              eligible(event, previous: previous) else { return }
        guard jobs.count < 64 else { deliveryError = String(localized: "notification.queue_full"); return }
        let id = generation + ":" + String(eventID)
        guard jobs[id] == nil else { return }
        let generation = generation
        let center = center
        let label = settings.label(for: station) ?? station
        let notice = StationNotice(id: id,
            title: String(format: String(localized: active ? "notification.raised" : "notification.recovered"), label),
            body: event.message ?? event.type ?? String(localized: "events.unknown_type"))
        jobs[id] = Task { [weak self] in
            guard let self else { return }
            defer { jobs.removeValue(forKey: id) }
            let permission = await center.permission()
            guard current(session, generation: generation), !Task.isCancelled else { return }
            self.permission = permission
            guard permission == .authorized, settings.stationIDs.contains(station),
                  eligible(event, previous: previous) else { return }
            do { try await center.add(notice) }
            catch {
                if current(session, generation: generation) { deliveryError = error.localizedDescription }
                return
            }
            // Some centers complete add asynchronously after cancellation. The
            // old generation remains identifiable and cannot present in-app.
            if !current(session, generation: generation) || Task.isCancelled {
                await center.remove(prefix: generation + ":")
            }
        }
    }

    private func current(_ session: ReadSession, generation: String) -> Bool {
        foreground && self.session == session && self.generation == generation
    }
    private func eligible(_ event: GNSSAPIEvent, previous: GNSSAPIEvent?) -> Bool {
        let offline = event.type == "station_offline"
        let rf = ["jamming_detected","spoofing_suspected","station_rf_degraded","antenna_fault"].contains(event.type ?? "")
        return (offline && settings.notifyOffline) || (rf && settings.notifyRF) ||
            ((event.severity == .critical || previous?.severity == .critical) && settings.notifyCritical)
    }
}

private final class SystemStationNotificationCenter: NSObject, StationNotificationCenter, UNUserNotificationCenterDelegate, @unchecked Sendable {
    private let center = UNUserNotificationCenter.current()
    @MainActor var canPresent: ((String)->Bool)?
    override init() { super.init(); center.delegate = self }
    func permission() async -> NotificationPermission {
        switch await center.notificationSettings().authorizationStatus {
        case .notDetermined: .notDetermined
        case .authorized, .provisional, .ephemeral: .authorized
        default: .denied
        }
    }
    func requestPermission() async throws -> Bool { try await center.requestAuthorization(options:[.alert,.sound]) }
    func add(_ notice: StationNotice) async throws {
        let content = UNMutableNotificationContent()
        content.title = notice.title; content.body = notice.body; content.sound = .default
        try await center.add(UNNotificationRequest(identifier:notice.id,content:content,trigger:nil))
    }
    func remove(prefix: String) async {
        let pending = await center.pendingNotificationRequests().map(\.identifier).filter {$0.hasPrefix(prefix)}
        center.removePendingNotificationRequests(withIdentifiers:pending)
        let delivered = await center.deliveredNotifications().map(\.request.identifier).filter {$0.hasPrefix(prefix)}
        center.removeDeliveredNotifications(withIdentifiers:delivered)
    }
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        let id = notification.request.identifier
        return await MainActor.run { canPresent?(id) == true ? [.banner,.sound] : [] }
    }
}
