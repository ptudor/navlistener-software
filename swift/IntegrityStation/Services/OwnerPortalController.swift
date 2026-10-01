import Foundation
import Observation

@MainActor @Observable final class OwnerPortalController {
    private(set) var connection: PortalConnection?
    private(set) var identity: PortalIdentity?
    private(set) var isConnecting = false
    private(set) var epoch = 0
    var error: String?
    @ObservationIgnored private let secure: any SecureConnectionStoring
    @ObservationIgnored let client: OwnerPortalClient
    @ObservationIgnored private var intent = 0
    @ObservationIgnored private var started = false
    @ObservationIgnored private var writes: Task<Void, Error>?
    init(secure: any SecureConnectionStoring = KeychainConnectionStore(service: "net.intsat.station.owner-portal.v1"),
         client: OwnerPortalClient = OwnerPortalClient()) { self.secure = secure; self.client = client }

    private func current(_ intent: Int) throws { guard self.intent == intent, !Task.isCancelled else { throw CancellationError() } }
    private func write(_ intent: Int?, action: @escaping @MainActor @Sendable () async throws -> Void) -> Task<Void, Error> {
        let previous = writes
        let task = Task { @MainActor [weak self] in
            _ = await previous?.result
            if let intent { guard let self else { throw CancellationError() }; try self.current(intent) }
            try await action()
        }
        writes = task; return task
    }
    func restore() async {
        guard !started else { return }; started = true
        intent += 1; let intent = intent
        do {
            guard let base = try await secure.lastServerURL(), let data = try await secure.token(forServer: base)?.data(using: .utf8) else { return }
            let saved = try JSONDecoder().decode(PortalConnection.self, from: data)
            try current(intent)
            guard try PortalInstallation(base) == saved.installation, PortalAuthorization.validToken(saved.token), saved.expiresAt > .now else {
                _ = try await write(intent) { [secure] in try await secure.deleteToken(forServer: base) }.value
                return
            }
            connection = saved
            await refresh()
        } catch is CancellationError {} catch { if self.intent == intent { self.error = PortalErrorMessage.describe(error) } }
    }
    func connect(to text: String, browser: (URL) async throws -> URL) async {
        intent += 1; let intent = intent; isConnecting = true; error = nil
        defer { if self.intent == intent { isConnecting = false } }
        do {
            let authorization = try PortalAuthorization(installation: PortalInstallation(text))
            let callback = try await browser(authorization.url)
            try current(intent)
            let (candidate, identity) = try await client.exchange(authorization, callback: callback)
            do {
                try current(intent)
                let encoded = String(decoding: try JSONEncoder().encode(candidate), as: UTF8.self)
                try await write(intent) { [secure] in
                    try await secure.saveToken(encoded, forServer: candidate.installation.url.absoluteString)
                    try await secure.saveLastServerURL(candidate.installation.url.absoluteString)
                }.value
                try current(intent)
            } catch {
                // A retired save must not resurrect a connection after sign-out.
                // Compare inside the serialized write queue so a newer account wins.
                _ = try? await write(nil) { [secure] in
                    let base = candidate.installation.url.absoluteString
                    if let text = try await secure.token(forServer: base), let data = text.data(using: .utf8),
                       (try? JSONDecoder().decode(PortalConnection.self, from: data)) == candidate {
                        try await secure.deleteToken(forServer: base)
                    }
                }.value
                try? await client.revoke(candidate)
                throw error
            }
            connection = candidate; self.identity = identity; epoch += 1
        } catch is CancellationError {} catch { if self.intent == intent { self.error = PortalErrorMessage.describe(error) } }
    }
    func refresh() async {
        guard let connection else { return }
        let epoch = epoch
        do {
            let identity = try await client.identity(connection)
            guard self.connection == connection, self.epoch == epoch, !Task.isCancelled else { return }
            self.identity = identity; error = nil
        } catch {
            guard self.connection == connection, self.epoch == epoch, !Task.isCancelled else { return }
            await failed(error)
        }
    }
    func suspend() { if identity != nil { identity = nil; epoch += 1 } }
    func failed(_ failure: Error) async {
        if case FeedError.unauthorized = failure { await signOut(revoke: false); error = String(localized: "portal.sign_in_again") }
        else if case FeedError.forbidden = failure { suspend(); error = String(localized: "portal.access_changed") }
        else if !(failure is CancellationError) { error = PortalErrorMessage.describe(failure) }
    }
    func signOut(revoke: Bool = true) async {
        let old = connection
        intent += 1; connection = nil; identity = nil; epoch += 1; isConnecting = false; error = nil
        guard let old else { return }
        let intent = intent
        do { try await write(nil) { [secure] in try await secure.deleteToken(forServer: old.installation.url.absoluteString) }.value }
        catch { if self.intent == intent { self.error = PortalErrorMessage.describe(error) } }
        if revoke {
            do { try await client.revoke(old) }
            catch { if self.intent == intent { self.error = String(localized: "portal.revoke_unconfirmed") } }
        }
    }
}
