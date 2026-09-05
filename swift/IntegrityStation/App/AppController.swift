import Foundation
import Observation

@MainActor
@Observable
final class AppController {
    let store: StationStore
    let settings: AppSettings
    let notifications: StationNotifications

    private(set) var availableAudiences: [ReadAudience] = []
    private(set) var selectedAudience: ReadAudience = .publicAudience
    private(set) var principalID = AudienceCacheKey.anonymousPrincipal
    private(set) var authorizationRevision = "public"
    private(set) var isConnecting = false
    private(set) var hasStoredCredential = false
    private(set) var connectionError: String?

    @ObservationIgnored private let secureStore: any SecureConnectionStoring
    @ObservationIgnored private let feedClient: FeedClient
    @ObservationIgnored private var activeToken: String?
    @ObservationIgnored private var authenticatedPrincipalID: String?
    @ObservationIgnored private var authenticatedRevision: String?
    @ObservationIgnored private var publicRevision: String?
    @ObservationIgnored private var hasStarted = false
    @ObservationIgnored private var operationGeneration: UInt64 = 0
    @ObservationIgnored private var pendingServerURL: URL?
    @ObservationIgnored private var credentialMutation: Task<Void, Error>?
    @ObservationIgnored private var removedCredentialServers: Set<String> = []
    private(set) var credentialDeletionError: String?

    private func beginIntent() -> UInt64 {
        operationGeneration += 1
        isConnecting = false
        return operationGeneration
    }

    private func requireCurrent(_ operation: UInt64) throws {
        guard operationGeneration == operation, !Task.isCancelled else { throw CancellationError() }
    }

    // Keychain actors can suspend in test/back-end implementations. Preserve
    // mutation order even across logout and reverse-completing connections.
    private func enqueueCredentialMutation(
        operation: UInt64?,
        _ action: @escaping @MainActor @Sendable () async throws -> Void
    ) -> Task<Void, Error> {
        let preceding = credentialMutation
        let task = Task { @MainActor [weak self] in
            _ = await preceding?.result
            if let operation {
                guard let self else { throw CancellationError() }
                try self.requireCurrent(operation)
            }
            try await action()
        }
        credentialMutation = task
        return task
    }

    init(
        store: StationStore = StationStore(),
        settings: AppSettings = AppSettings(),
        secureStore: any SecureConnectionStoring = KeychainConnectionStore(),
        feedClient: FeedClient = FeedClient()
    ) {
        self.store = store
        self.settings = settings
        self.notifications = StationNotifications(settings: settings)
        store.notifications = notifications
        if let session = store.activeSession { notifications.activate(session) }
        self.secureStore = secureStore
        self.feedClient = feedClient
    }

    var serverURL: URL? {
        store.activeSession?.baseURL ?? Self.validServerURL(settings.serverURLString)
    }

    func start() async {
        guard !hasStarted else { return }
        hasStarted = true
        let operation = beginIntent()
        do {
            let secureURL = try await secureStore.lastServerURL()
            try requireCurrent(operation)
            let candidate = secureURL ?? settings.serverURLString
            guard !candidate.isEmpty else { return }
            try await connect(to: candidate, readToken: nil, useStoredCredential: true, operation: operation)
        } catch {
            guard operationGeneration == operation else { return }
            connectionError = error.localizedDescription
        }
    }

    /// Discovers grants before selecting an audience. `readToken == nil` reuses
    /// this server's Keychain credential; user-entered audience ids are never
    /// accepted by this API.
    func connect(to input: String, readToken: String? = nil) async throws {
        try await connect(to: input, readToken: readToken, useStoredCredential: true, operation: beginIntent())
    }

    func refresh() async {
        await store.refresh()
    }

    func selectAudience(_ audience: ReadAudience) async throws {
        guard availableAudiences.contains(audience), let baseURL = serverURL else {
            throw FeedError.audienceLost
        }
        let revision = audience.isPrivate ? authenticatedRevision : publicRevision
        guard let session = ReadSession(
            baseURL: baseURL,
            principalID: audience.isPrivate ? authenticatedPrincipalID : nil,
            audience: audience,
            authorizationRevision: revision,
            token: audience.isPrivate ? activeToken : nil
        ) else { throw FeedError.invalidCredential }

        _ = beginIntent()
        pendingServerURL = nil
        selectedAudience = audience
        principalID = session.principalID
        authorizationRevision = session.authorizationRevision
        settings.setPreferredAudience(audience, forServer: baseURL.absoluteString)
        settings.activateScope(session.cacheKey)
        store.start(session: session, stationIDs: settings.stationIDs)
        connectionError = nil
    }

    /// Deletes the read token and the active private payload/cursor, then falls
    /// back to the anonymous public audience for the same collector.
    func logout() async {
        let baseURL = pendingServerURL ?? serverURL
        let oldServer = serverURL
        let oldPrincipal = authenticatedPrincipalID
        let operation = beginIntent()
        let targets = Set([baseURL?.absoluteString, oldServer?.absoluteString].compactMap { $0 })
        removedCredentialServers.formUnion(targets)
        pendingServerURL = nil
        activeToken = nil
        authenticatedPrincipalID = nil
        authenticatedRevision = nil
        hasStoredCredential = false
        availableAudiences = []
        selectedAudience = .publicAudience
        principalID = AudienceCacheKey.anonymousPrincipal
        authorizationRevision = "public"
        credentialDeletionError = nil
        // Queue deletion before any suspension so subsequent saves necessarily
        // follow it. This also drains an already-running obsolete token save.
        let deletion = enqueueCredentialMutation(operation: nil) { [secureStore] in
            var failure: Error?
            for server in targets {
                do { try await secureStore.deleteToken(forServer: server) }
                catch { failure = failure ?? error }
            }
            if let failure { throw failure }
        }
        await store.disconnect(clearCachedScope: true)
        guard operationGeneration == operation else { return }
        for server in targets {
            await store.clearPrivateCaches(forServer: server, principal: oldPrincipal)
            guard operationGeneration == operation else { return }
        }
        var deletionError: String?
        do { try await deletion.value } catch { deletionError = error.localizedDescription }
        guard operationGeneration == operation else { return }
        credentialDeletionError = deletionError
        guard let baseURL else { connectionError = deletionError; return }
        do {
            try await connect(to: baseURL.absoluteString, readToken: nil,
                              useStoredCredential: false, operation: operation)
            connectionError = deletionError
        } catch {
            guard operationGeneration == operation else { return }
            connectionError = deletionError ?? error.localizedDescription
        }
    }

    func addStation(id: String, for session: ReadSession? = nil) throws {
        if let session, store.activeSession != session { throw FeedError.audienceLost }
        guard Self.isValidStationID(id) else { throw FeedError.invalidStationID }
        guard !settings.stationIDs.contains(id) else { return }
        settings.stationIDs.append(id)
        store.selectedStationIDs = settings.stationIDs
    }

    func removeStation(id: String) {
        settings.stationIDs.removeAll { $0 == id }
        settings.labelsByStationID.removeValue(forKey: id)
        store.selectedStationIDs = settings.stationIDs
    }

    func setLabel(_ label: String, for stationID: String) {
        let trimmed = label.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { settings.labelsByStationID.removeValue(forKey: stationID) }
        else { settings.labelsByStationID[stationID] = trimmed }
    }

    // Selection consumes opaque API identities, not certificate enrollment
    // names. A configured local/token observer with an explicit enrollment can
    // contain punctuation, Unicode or whitespace; preserve its exact value.
    static func isValidStationID(_ value: String) -> Bool { !value.isEmpty }

    static func validServerURL(_ value: String) -> URL? {
        let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
        guard var components = URLComponents(string: trimmed),
              let scheme = components.scheme?.lowercased(),
              scheme == "http" || scheme == "https",
              let host = components.host, !host.isEmpty,
              components.user == nil,
              components.password == nil,
              components.query == nil,
              components.fragment == nil,
              components.port.map({ (1...65_535).contains($0) }) ?? true
        else { return nil }

        components.scheme = scheme
        components.host = host.lowercased()
        while components.path.count > 1 && components.path.hasSuffix("/") {
            components.path.removeLast()
        }
        return components.url
    }

    private func connect(
        to input: String,
        readToken: String?,
        useStoredCredential: Bool,
        operation: UInt64
    ) async throws {
        guard let url = Self.validServerURL(input) else { throw FeedError.invalidBaseURL }
        if let readToken, ReadSession.normalizedToken(readToken) == nil {
            throw FeedError.invalidCredential
        }

        try requireCurrent(operation)
        pendingServerURL = url
        isConnecting = true
        defer {
            if operationGeneration == operation { isConnecting = false; pendingServerURL = nil }
        }
        let token: String?
        if let readToken {
            token = ReadSession.normalizedToken(readToken)
        } else if useStoredCredential && !removedCredentialServers.contains(url.absoluteString) {
            _ = await credentialMutation?.result
            try requireCurrent(operation)
            token = try await secureStore.token(forServer: url.absoluteString)
            try requireCurrent(operation)
        } else {
            token = nil
        }

        if token != nil { try ReadRequestHeaders.requireSecureTransport(url) }
        let envelope = try await feedClient.fetchAudiences(baseURL: url, token: token)
        try requireCurrent(operation)
        guard let discovery = envelope.data?.validated() else { throw FeedError.invalidResponse }
        if (token == nil) != (discovery.principalID == nil) { throw FeedError.invalidResponse }

        let anonymousDiscovery: AudienceDiscovery
        if token == nil {
            anonymousDiscovery = discovery
        } else {
            let publicEnvelope = try await feedClient.fetchAudiences(baseURL: url, token: nil)
            try requireCurrent(operation)
            guard let publicDiscovery = publicEnvelope.data?.validated(),
                  publicDiscovery.principalID == nil,
                  publicDiscovery.audiences == [.publicAudience]
            else { throw FeedError.invalidResponse }
            anonymousDiscovery = publicDiscovery
        }

        let preferred = settings.preferredAudience(forServer: url.absoluteString)
        let selected: ReadAudience
        if let preferred, discovery.audiences.contains(preferred) {
            selected = preferred
        } else if token != nil,
                  let privateAudience = discovery.audiences.first(where: \.isPrivate) {
            selected = privateAudience
        } else {
            selected = .publicAudience
        }

        let selectedRevision = selected.isPrivate
            ? discovery.authorizationRevision
            : anonymousDiscovery.authorizationRevision

        guard let session = ReadSession(
            baseURL: url,
            principalID: selected.isPrivate ? discovery.principalID : nil,
            audience: selected,
            authorizationRevision: selectedRevision,
            token: selected.isPrivate ? token : nil
        ) else { throw FeedError.invalidResponse }

        if let oldSession = store.activeSession {
            let serverChanged = oldSession.baseURL.absoluteString != url.absoluteString
            let newPrincipal = discovery.principalID ?? AudienceCacheKey.anonymousPrincipal
            let oldPrincipal = authenticatedPrincipalID ?? AudienceCacheKey.anonymousPrincipal
            let principalChanged = oldPrincipal != newPrincipal
            let newRevision = discovery.authorizationRevision
            let oldRevision = authenticatedRevision ?? publicRevision ?? ""
            let authorizationChanged = oldRevision != newRevision
            let audienceWasLost = !discovery.audiences.contains(oldSession.audience)
            if serverChanged || principalChanged || authorizationChanged || audienceWasLost {
                if serverChanged || principalChanged || authorizationChanged {
                    await store.clearPrivateCaches(
                        forServer: oldSession.baseURL.absoluteString,
                        principal: serverChanged || authenticatedPrincipalID == nil
                            ? nil
                            : authenticatedPrincipalID
                    )
                    try requireCurrent(operation)
                }
                await store.disconnect(clearCachedScope: true)
                try requireCurrent(operation)
            }
        }
        let normalized = readToken.flatMap(ReadSession.normalizedToken)
        let mutation = enqueueCredentialMutation(operation: operation) { [self, secureStore] in
            if let normalized {
                try await secureStore.saveToken(normalized, forServer: url.absoluteString)
                if operationGeneration != operation {
                    // The newer intent's mutation is queued behind this one.
                    // Remove a token whose save was already in flight when superseded.
                    try await secureStore.deleteToken(forServer: url.absoluteString)
                    throw CancellationError()
                }
            }
            try requireCurrent(operation)
            try await secureStore.saveLastServerURL(url.absoluteString)
        }
        try await mutation.value
        try requireCurrent(operation)
        if normalized != nil { removedCredentialServers.remove(url.absoluteString) }
        settings.markServerURLMigrated()

        activeToken = token
        authenticatedPrincipalID = discovery.principalID
        authenticatedRevision = discovery.principalID == nil ? nil : discovery.authorizationRevision
        publicRevision = anonymousDiscovery.authorizationRevision
        hasStoredCredential = token != nil
        availableAudiences = discovery.audiences
        principalID = session.principalID
        authorizationRevision = session.authorizationRevision
        selectedAudience = selected
        settings.serverURLString = url.absoluteString
        if useStoredCredential || readToken != nil { settings.setPreferredAudience(selected, forServer: url.absoluteString) }
        settings.activateScope(session.cacheKey)
        store.start(session: session, stationIDs: settings.stationIDs)
        connectionError = nil
    }
}
