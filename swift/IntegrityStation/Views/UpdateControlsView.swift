import SwiftUI

struct UpdateControlsView: View {
    @Environment(AppController.self) private var controller
    let observerID: String
    @State private var access: UpdateAccess?
    @State private var busy = false
    @State private var message: String?
    @State private var operation: Task<Void, Never>?
    @State private var operationID = UUID()
    private let client = FeedClient()

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let access {
                if let advisory = access.choice?.advisory, !advisory.isEmpty { Text(advisory).font(.caption) }
                HStack {
                    Group {
                        Button("update.check_now") { run("check") }
                        Button("update.download") { run("download") }.disabled(access.choice == nil)
                        Button("update.install") { run("install") }
                            .disabled(access.choice == nil || access.record.status?.stagedRelease != access.choice?.release)
                        Button("update.update_now") { run("update") }
                    }.disabled(busy)
                    Button("update.cancel") { run("cancel") }
                }
                if busy { ProgressView() }
                if let message { Text(message).font(.caption).foregroundStyle(.secondary) }
            }
        }
        .task(id: controller.store.activeSession) {
            access = nil
            guard let session = controller.store.activeSession, session.audience.isPrivate else { return }
            access = try? await client.updateAccess(session: session, observer: observerID)
        }
        .onDisappear { operation?.cancel() }
    }

    private func run(_ action: String) {
        guard (!busy || action == "cancel"), let session = controller.store.activeSession else { return }
        operation?.cancel()
        let identifier = UUID()
        operationID = identifier
        busy = true
        message = nil
        operation = Task { @MainActor in
            defer { if operationID == identifier { busy = false } }
            do {
                let actions = action == "update" ? ["check", "download", "install"] : [action]
                for step in actions {
                    try Task.checkCancellation()
                    guard controller.store.activeSession == session else { throw CancellationError() }
                    let current = try await client.updateAccess(session: session, observer: observerID)
                    if step == "download", let choice = current.choice,
                       current.record.status?.runningRelease == choice.release { access = current; break }
                    let request = try await client.updateAccess(session: session, observer: observerID, action: step, choice: current.choice)
                    access = request
                    message = String(localized: "update.requested")
                    if step == "install" || step == "cancel" { continue }
                    var finished = false
                    for _ in 0..<600 {
                        try await Task.sleep(for: .seconds(2))
                        guard controller.store.activeSession == session else { throw CancellationError() }
                        let result = try await client.updateAccess(session: session, observer: observerID)
                        access = result
                        guard let reported = result.record.status,
                              let accepted = reported.lastCommand.flatMap(UInt64.init),
                              let requested = UInt64(request.record.command.commandID), accepted >= requested else { continue }
                        if reported.state == "failed" || reported.state == "rolled-back" {
                            throw FeedError.server(code: nil, message: reported.error ?? String(localized: "update.failed_release"))
                        }
                        if step == "download" ? reported.stagedRelease == current.choice?.release
                            : ["idle", "available", "staged", "confirmed"].contains(reported.state ?? "") {
                            finished = true; break
                        }
                    }
                    guard finished else { throw URLError(.timedOut) }
                }
                message = String(localized: "update.watch_status")
            } catch is CancellationError {
                message = nil
            } catch {
                message = error.localizedDescription
            }
        }
    }
}
