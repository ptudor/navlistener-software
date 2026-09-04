import Foundation
import Observation

@MainActor
@Observable
final class AppController {
    let store: StationStore
    let settings: AppSettings

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

    init(
        store: StationStore = StationStore(),
        settings: AppSettings = AppSettings(),
        secureStore: any SecureConnectionStoring = KeychainConnectionStore(),
        feedClient: FeedClient = FeedClient()
    ) {
        self.store = store
        self.settings = settings
        self.secureStore = secureStore
        self.feedClient = feedClient
    }

    var serverURL: URL? {
        store.activeSession?.baseURL ?? Self.validServerURL(settings.serverURLString)
    }

    func start() async {
        guard !hasStarted else { return }
        hasStarted = true
        do {
            let secureURL = try await secureStore.lastServerURL()
            let candidate = secureURL ?? settings.serverURLString
            guard !candidate.isEmpty else { return }
            try await connect(to: candidate)
        } catch {
            connectionError = error.localizedDescription
        }
    }

    /// Discovers grants before selecting an audience. `readToken == nil` reuses
    /// this server's Keychain credential; user-entered audience ids are never
    /// accepted by this API.
    func connect(to input: String, readToken: String? = nil) async throws {
        try await connect(to: input, readToken: readToken, useStoredCredential: true)
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
        guard let baseURL = serverURL else { return }
        do {
            try await secureStore.deleteToken(forServer: baseURL.absoluteString)
            await store.clearPrivateCaches(
                forServer: baseURL.absoluteString,
                principal: authenticatedPrincipalID
            )
            await store.disconnect(clearCachedScope: true)
            activeToken = nil
            authenticatedPrincipalID = nil
            authenticatedRevision = nil
            hasStoredCredential = false
            try await connect(
                to: baseURL.absoluteString,
                readToken: nil,
                useStoredCredential: false
            )
        } catch {
            connectionError = error.localizedDescription
        }
    }

    func addStation(id: String) {
        let id = id.trimmingCharacters(in: .whitespacesAndNewlines)
        guard Self.isValidStationID(id), !settings.stationIDs.contains(id) else { return }
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

    static func isValidStationID(_ value: String) -> Bool {
        !value.isEmpty && value.count <= 256 && value.allSatisfy {
            $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "." || $0 == "-")
        }
    }

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
        useStoredCredential: Bool
    ) async throws {
        guard let url = Self.validServerURL(input) else { throw FeedError.invalidBaseURL }
        if let readToken, ReadSession.normalizedToken(readToken) == nil {
            throw FeedError.invalidCredential
        }

        isConnecting = true
        defer { isConnecting = false }
        let token: String?
        if let readToken {
            token = ReadSession.normalizedToken(readToken)
        } else if useStoredCredential {
            token = try await secureStore.token(forServer: url.absoluteString)
        } else {
            token = nil
        }

        if token != nil { try ReadRequestHeaders.requireSecureTransport(url) }
        let envelope = try await feedClient.fetchAudiences(baseURL: url, token: token)
        guard let discovery = envelope.data?.validated() else { throw FeedError.invalidResponse }
        if (token == nil) != (discovery.principalID == nil) { throw FeedError.invalidResponse }

        let anonymousDiscovery: AudienceDiscovery
        if token == nil {
            anonymousDiscovery = discovery
        } else {
            let publicEnvelope = try await feedClient.fetchAudiences(baseURL: url, token: nil)
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
                }
                await store.disconnect(clearCachedScope: true)
            }
        }
        if let readToken, let normalized = ReadSession.normalizedToken(readToken) {
            try await secureStore.saveToken(normalized, forServer: url.absoluteString)
        }
        try await secureStore.saveLastServerURL(url.absoluteString)
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
        settings.setPreferredAudience(selected, forServer: url.absoluteString)
        settings.activateScope(session.cacheKey)
        store.start(session: session, stationIDs: settings.stationIDs)
        connectionError = nil
    }
}
