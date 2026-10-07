import SwiftUI

struct UpdateControlsView: View {
    @Environment(AppController.self) private var controller
    let observerID: String
    @State private var access: UpdateAccess?
    @State private var unavailable: String?
    @State private var busy = false
    @State private var message: String?
    @State private var operation: Task<Void, Never>?
    @State private var operationID = UUID()

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
            } else if let unavailable {
                Label(unavailable, systemImage: "exclamationmark.circle")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .task(id: controller.store.activeSession) {
            access = nil
            unavailable = nil
            guard let session = controller.store.activeSession, session.audience.isPrivate else { return }
            do {
                access = try await controller.feedClient.updateAccess(session: session, observer: observerID)
            } catch is CancellationError {
            } catch let error as URLError where error.code == .cancelled {
            } catch {
                unavailable = Self.loadFailureMessage(error)
            }
        }
        .onDisappear { operation?.cancel() }
    }

    /// What stands in for the controls when the access query did not return a
    /// record. A denied update grant reads as such with the collector's own
    /// reason; anything else ("update controls are not configured", a
    /// transport failure) reads as unavailable with its reason, so the three
    /// cases are never confused with each other or with silence.
    nonisolated static func loadFailureMessage(_ error: Error) -> String {
        if case FeedError.updateDenied = error {
            return String(format: String(localized: "update.denied"), error.localizedDescription)
        }
        return String(format: String(localized: "update.unavailable"), error.localizedDescription)
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
            let client = controller.feedClient
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
