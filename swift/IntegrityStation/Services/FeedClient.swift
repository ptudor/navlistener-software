import Foundation

enum FeedError: Error, Equatable, LocalizedError, Sendable {
    case invalidBaseURL
    case invalidStationID
    case invalidCredential
    case invalidResponse
    case http(Int)
    case unauthorized(String?)
    case forbidden(String?)
    case audienceLost
    // The collector's discovery revision moved under a session whose audience is
    // still granted: a restart or an ingest-policy change (docs/OUTPUT.md §0.1).
    // The partition is erased and discovery re-run; the credential is not lost.
    case revisionChanged
    case server(code: Int?, message: String)
    case missingData
    case inputLimit
    case streamEnded
    // a recognized state-bearing event ("gnss"/"resolved") that
    // cannot be decoded is a stream-integrity failure, not a frame to skip.
    // Skipping it let the client accept a later cursor and step permanently past
    // a durable transition while still presenting conditions as known.
    case malformedEvent(id: String?)
    // Update control (gnss/api/v2/updates) answers outside the v2 envelope
    // with plain-text reasons. A denial concerns the update grant for one
    // observer, never the read credential, so it is not an authorization
    // loss; any other rejection carries the collector's reason and status.
    case updateDenied(String?)
    case updateRejected(status: Int, message: String)

    var errorDescription: String? {
        switch self {
        case .invalidBaseURL: String(localized: "error.invalid_base_url")
        case .invalidStationID: String(localized: "onboarding.station.invalid")
        case .invalidCredential: String(localized: "error.invalid_credential")
        case .invalidResponse: String(localized: "error.invalid_response")
        case .http(let code): String(format: String(localized: "error.http"), code)
        case .unauthorized(let message): message ?? String(localized: "error.unauthorized")
        case .forbidden(let message): message ?? String(localized: "error.forbidden")
        case .audienceLost: String(localized: "error.audience_lost")
        case .revisionChanged: String(localized: "error.revision_changed")
        case .server(_, let message): message
        case .missingData: String(localized: "error.missing_data")
        case .inputLimit: String(localized: "error.input_limit")
        case .streamEnded: String(localized: "error.stream_ended")
        case .malformedEvent: String(localized: "error.malformed_event")
        case .updateDenied(let message): message ?? String(localized: "error.update_denied")
        case .updateRejected(_, let message): message
        }
    }

    var isAuthorizationLoss: Bool {
        switch self {
        case .unauthorized, .forbidden, .audienceLost: true
        default: false
        }
    }
}

enum CollectorEndpoint {
    // Only aliases of the same collector may receive an existing read token.
    static func secondaryURL(_ url: URL) -> URL? {
        guard url.scheme?.lowercased() == "https", url.user == nil, url.password == nil,
              let host = url.host?.lowercased().trimmingCharacters(in: CharacterSet(charactersIn: "."))
        else { return nil }
        let pairs = [
            ["in.intsat.net", "in.intsat.space"],
            ["klax1-navlistener.intsat.net", "klax1-navlistener.intsat.space"]
        ]
        for pair in pairs {
            guard let index = pair.firstIndex(of: host) else { continue }
            var components = URLComponents(url: url, resolvingAgainstBaseURL: false)
            components?.host = pair[1 - index]
            return components?.url
        }
        return nil
    }

    /// A certificate or TLS failure on the primary is shown, never routed
    /// around: an expired certificate or an interception on one host must not
    /// be masked by a successful alias, and the alias must not be tried with
    /// the credential after the primary's identity could not be verified.
    static let transportSecurityCodes: Set<URLError.Code> = [
        .secureConnectionFailed, .serverCertificateHasBadDate, .serverCertificateUntrusted,
        .serverCertificateHasUnknownRoot, .serverCertificateNotYetValid, .clientCertificateRejected,
        .clientCertificateRequired, .appTransportSecurityRequiresSecureConnection
    ]

    static func canRetry(_ error: Error) -> Bool {
        if let error = error as? URLError {
            return error.code != .cancelled && error.code != .userAuthenticationRequired
                && error.code != .userCancelledAuthentication && error.code != .badURL
                && !transportSecurityCodes.contains(error.code)
        }
        if case FeedError.http(let status) = error { return isRetryableStatus(status) }
        if case FeedError.updateRejected(let status, _) = error { return isRetryableStatus(status) }
        return false
    }

    static func isRetryableStatus(_ status: Int) -> Bool {
        status == 408 || status == 421 || status == 429 || (500...599).contains(status)
    }

    static func stream(session: URLSession, request: URLRequest) async throws -> (URLSession.AsyncBytes, URLResponse) {
        func open(_ request: URLRequest) async throws -> (URLSession.AsyncBytes, URLResponse) {
            let result = try await session.bytes(for: request, delegate: CredentialRedirectGuard(request: request))
            if let http = result.1 as? HTTPURLResponse, canRetry(FeedError.http(http.statusCode)) {
                result.0.task.cancel()
                throw FeedError.http(http.statusCode)
            }
            return result
        }
        do { return try await open(request) }
        catch {
            guard canRetry(error), let url = request.url, let secondary = secondaryURL(url) else { throw error }
            try Task.checkCancellation()
            var backup = request
            backup.url = secondary
            return try await open(backup)
        }
    }

    static func url(baseURL: URL, path: String) throws -> URL {
        guard let scheme = baseURL.scheme?.lowercased(),
              scheme == "http" || scheme == "https",
              baseURL.host != nil
        else { throw FeedError.invalidBaseURL }
        return baseURL.appending(path: path.trimmingCharacters(in: CharacterSet(charactersIn: "/")))
    }

    /// Characters a query name or value may carry unescaped. URLComponents
    /// leaves `+`, `&`, `=` and `%` alone in query items, but the collector
    /// parses the query with Go's url.ParseQuery, which decodes a bare `+` as
    /// a space and splits on `&`/`=`; opaque observer ids may contain any
    /// punctuation, so those are escaped as well.
    static let queryAllowed = CharacterSet.urlQueryAllowed.subtracting(CharacterSet(charactersIn: "+&=%"))

    /// Items ready for `URLComponents.percentEncodedQueryItems`.
    static func percentEncodedQueryItems(_ items: [URLQueryItem]) throws -> [URLQueryItem] {
        try items.map { item in
            guard let name = item.name.addingPercentEncoding(withAllowedCharacters: queryAllowed)
            else { throw FeedError.invalidBaseURL }
            let value = try item.value.map { raw -> String in
                guard let encoded = raw.addingPercentEncoding(withAllowedCharacters: queryAllowed)
                else { throw FeedError.invalidBaseURL }
                return encoded
            }
            return URLQueryItem(name: name, value: value)
        }
    }
}

struct FeedClient: Sendable, Equatable {
    private let session: URLSession

    init(session: URLSession = FeedClient.failFastSession()) {
        self.session = session
    }

    /// Two clients are the same client when they share a URLSession; the
    /// shared app client must be handed around, never re-created.
    static func == (lhs: FeedClient, rhs: FeedClient) -> Bool { lhs.session === rhs.session }

    func fetchAudiences(baseURL: URL, token: String?) async throws -> APIEnvelope<AudienceDiscoveryPayload> {
        try await fetch(baseURL: baseURL, path: "gnss/api/v2/audiences", token: token)
    }

    func updateAccess(session: ReadSession, observer: String, action: String? = nil,
                      choice: UpdateAccess.Choice? = nil, requestID: String = UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased()) async throws -> UpdateAccess {
        guard session.audience.isPrivate, session.token != nil else { throw FeedError.updateDenied(nil) }
        let endpoint = try CollectorEndpoint.url(baseURL: session.baseURL, path: "gnss/api/v2/updates")
        var request = URLRequest(url: endpoint)
        request.timeoutInterval = 15
        request.cachePolicy = .reloadIgnoringLocalCacheData
        try ReadRequestHeaders.apply(session: session, to: &request)
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        if let action {
            request.httpMethod = "POST"
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            let selects = action == "download" || action == "install"
            request.httpBody = try JSONEncoder().encode([
                "observer_id": observer, "request_id": requestID, "action": action,
                "generation": selects ? choice?.generation ?? "0" : "0",
                "release": selects ? choice?.release ?? "0" : "0"
            ])
        } else {
            var components = URLComponents(url: endpoint, resolvingAgainstBaseURL: false)
            components?.percentEncodedQueryItems = try CollectorEndpoint.percentEncodedQueryItems(
                [URLQueryItem(name: "observer_id", value: observer)])
            request.url = components?.url
        }
        func once(_ request: URLRequest) async throws -> UpdateAccess {
            let (bytes, response) = try await self.session.bytes(for: request, delegate: CredentialRedirectGuard(request: request))
            defer { bytes.task.cancel() }
            guard let http = response as? HTTPURLResponse else { throw FeedError.invalidResponse }
            guard (200...299).contains(http.statusCode) else {
                // The reason is a short plain-text line; an oversized body
                // carries none worth keeping.
                let body: Data?
                do { body = try await NetworkLimits.body(bytes, maximum: NetworkLimits.errorBytes) }
                catch FeedError.inputLimit { body = nil }
                throw Self.updateResponseError(status: http.statusCode, mimeType: http.mimeType, data: body)
            }
            guard response.expectedContentLength <= NetworkLimits.responseBytes else { throw FeedError.inputLimit }
            let data = try await NetworkLimits.body(bytes, maximum: NetworkLimits.responseBytes)
            return try JSONDecoder().decode(UpdateAccess.self, from: data)
        }
        do { return try await once(request) }
        catch {
            guard CollectorEndpoint.canRetry(error), let url = request.url,
                  let backup = CollectorEndpoint.secondaryURL(url) else { throw error }
            try Task.checkCancellation()
            // The same durable request ID and exact body make a lost POST
            // response safe to retry through the collector's other hostname.
            request.url = backup
            return try await once(request)
        }
    }

    func fetchObservers(session: ReadSession) async throws -> APIEnvelope<ObserversPayload> {
        try await fetch(
            baseURL: session.baseURL,
            path: "gnss/api/v2/observers",
            session: session
        )
    }

    func fetchConditions(session: ReadSession) async throws -> APIEnvelope<ConditionsPayload> {
        try await fetch(baseURL: session.baseURL, path: "gnss/api/events/conditions", session: session)
    }

    func fetchEvents(session: ReadSession, since: Date? = nil) async throws -> APIEnvelope<EventsPayload> {
        let endpoint = try CollectorEndpoint.url(baseURL: session.baseURL, path: "gnss/api/events")
        var components = URLComponents(url: endpoint, resolvingAgainstBaseURL: false)
        if let since {
            components?.percentEncodedQueryItems = try CollectorEndpoint.percentEncodedQueryItems(
                [URLQueryItem(name: "since", value: since.ISO8601Format())])
        }
        guard let url = components?.url else { throw FeedError.invalidBaseURL }
        return try await fetch(url: url, session: session)
    }

    func fetchSensorHistory(session: ReadSession, request: SensorHistoryRequest) async throws -> SensorHistoryPage {
        guard session.audience.isPrivate else { throw FeedError.forbidden(nil) }
        func validateGrant() async throws {
            let envelope = try await fetchAudiences(baseURL: session.baseURL, token: session.token)
            guard let discovery = envelope.data?.validated(),
                  discovery.principalID == session.principalID,
                  discovery.audiences.contains(session.audience) else { throw FeedError.audienceLost }
            // A moved revision with the grant intact means "history access
            // changed": the owner re-discovers and the view reloads under the
            // new session rather than losing the credential.
            guard discovery.authorizationRevision == session.authorizationRevision else {
                throw FeedError.revisionChanged
            }
        }
        try await validateGrant()
        let endpoint = try CollectorEndpoint.url(baseURL: session.baseURL, path: "gnss/api/v2/observer-samples")
        var components = URLComponents(url: endpoint, resolvingAgainstBaseURL: false)
        components?.percentEncodedQueryItems = try CollectorEndpoint.percentEncodedQueryItems(request.query)
        guard let url = components?.url else { throw FeedError.invalidBaseURL }
        let envelope: APIEnvelope<SensorHistoryPage> = try await fetch(url: url, session: session)
        guard let page = envelope.data else { throw FeedError.missingData }
        try page.validate(request: request, session: session)
        try await validateGrant()
        return page
    }

    private func fetch<Payload: Codable & Sendable>(
        baseURL: URL,
        path: String,
        token: String? = nil,
        session: ReadSession? = nil
    ) async throws -> APIEnvelope<Payload> {
        try await fetch(
            url: CollectorEndpoint.url(baseURL: baseURL, path: path),
            token: token,
            session: session
        )
    }

    private func fetch<Payload: Codable & Sendable>(
        url: URL,
        token: String? = nil,
        session: ReadSession? = nil
    ) async throws -> APIEnvelope<Payload> {
        do { return try await fetchOnce(url: url, token: token, session: session) }
        catch {
            guard CollectorEndpoint.canRetry(error), let secondary = CollectorEndpoint.secondaryURL(url) else { throw error }
            try Task.checkCancellation()
            return try await fetchOnce(url: secondary, token: token, session: session)
        }
    }

    private func fetchOnce<Payload: Codable & Sendable>(
        url: URL,
        token: String?,
        session: ReadSession?
    ) async throws -> APIEnvelope<Payload> {
        var request = URLRequest(url: url)
        request.timeoutInterval = 10
        // Feed documents are this client's clock source. An answer from the
        // URL cache within the public feed's max-age would be stamped with the
        // current fetch time and understate every age, so even anonymous
        // discovery goes to the origin.
        request.cachePolicy = .reloadIgnoringLocalCacheData
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.setValue("IntegrityStation/0.1", forHTTPHeaderField: "User-Agent")
        if let session {
            try ReadRequestHeaders.apply(session: session, to: &request)
        } else if let token {
            try ReadRequestHeaders.apply(token: token, to: &request)
        }

        let (bytes, response) = try await self.session.bytes(for: request, delegate: CredentialRedirectGuard(request: request))
        defer { bytes.task.cancel() }
        guard let http = response as? HTTPURLResponse else { throw FeedError.invalidResponse }
        let success = (200...299).contains(http.statusCode)
        let maximum = success ? NetworkLimits.responseBytes : NetworkLimits.errorBytes
        let data: Data
        do {
            guard response.expectedContentLength <= maximum else { throw FeedError.inputLimit }
            data = try await NetworkLimits.body(bytes, maximum: maximum)
        } catch FeedError.inputLimit where http.statusCode == 401 || http.statusCode == 403 {
            // An oversized denial body cannot postpone credential withdrawal.
            throw Self.responseError(status: http.statusCode)
        }
        guard success else { throw Self.responseError(status: http.statusCode, data: data) }

        var envelope: APIEnvelope<Payload>
        do {
            envelope = try JSONDecoder().decode(APIEnvelope<Payload>.self, from: data)
        } catch {
            throw FeedError.invalidResponse
        }
        guard envelope.ok else {
            throw FeedError.server(
                code: envelope.code,
                message: envelope.error ?? String(localized: "error.server")
            )
        }
        guard envelope.data != nil else { throw FeedError.missingData }
        envelope.cacheAge = Self.cacheAge(of: http)
        return envelope
    }

    static func responseError(status: Int, data: Data? = nil) -> FeedError {
        let message = data.flatMap { try? JSONDecoder().decode(APIErrorEnvelope.self, from: $0).error }
        return switch status {
        case 401: .unauthorized(message)
        case 403: .forbidden(message)
        default: .http(status)
        }
    }

    /// Longest server reason shown to the user.
    static let reasonCharacters = 256

    /// The update-control endpoint answers outside the v2 envelope with
    /// http.Error text ("update grant required for this enrolled observer",
    /// the 409 conflict reason, "update controls are not configured"). That
    /// text, bounded, is the message; a JSON `error` field is honoured should
    /// one ever appear; any other content (a proxy's HTML page) carries no
    /// reason. 401 and 403 here concern the update credential and grant for
    /// one observer, so they never read as a withdrawn read authorization.
    static func updateResponseError(status: Int, mimeType: String?, data: Data?) -> FeedError {
        let reason = data.flatMap { reason(mimeType: mimeType, data: $0) }
        return switch status {
        case 401, 403: .updateDenied(reason)
        default: reason.map { .updateRejected(status: status, message: $0) } ?? .http(status)
        }
    }

    private static func reason(mimeType: String?, data: Data) -> String? {
        let text: String? = switch mimeType {
        case "application/json": try? JSONDecoder().decode(APIErrorEnvelope.self, from: data).error
        case "text/plain": String(data: data, encoding: .utf8)
        default: nil
        }
        guard let trimmed = text?.trimmingCharacters(in: .whitespacesAndNewlines), !trimmed.isEmpty else { return nil }
        return String(trimmed.prefix(reasonCharacters))
    }

    /// RFC 9111 §5.1: a recipient reads an `Age` above 2^31 seconds as 2^31.
    static let maximumCacheAge: TimeInterval = 2_147_483_648

    /// Seconds a shared cache (a reverse proxy, a CDN) held the response before
    /// forwarding it. The local URL cache is bypassed, but a proxy answering
    /// within the public feed's max-age still serves an older document than
    /// its `time` suggests; the store moves its fetch instant back by this much.
    /// `Age` is delta-seconds: a non-negative integer. Anything else is no age.
    static func cacheAge(of response: HTTPURLResponse) -> TimeInterval {
        guard let value = response.value(forHTTPHeaderField: "Age")?
                .trimmingCharacters(in: .whitespaces),
              !value.isEmpty, value.utf8.allSatisfy({ (48...57).contains($0) })
        else { return 0 }
        guard let seconds = UInt64(value) else { return maximumCacheAge }
        return min(TimeInterval(seconds), maximumCacheAge)
    }

    /// A short idle watchdog between bytes, and a total bound sized to the
    /// 32 MiB response limit: 120 s admits a full-size fleet document at about
    /// 2.2 Mbit/s, where the former 15 s could never complete one and every
    /// poll of a large fleet on a slow link failed with a timeout.
    static let idleTimeout: TimeInterval = 10
    static let resourceTimeout: TimeInterval = 120

    static func failFastSession() -> URLSession {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.waitsForConnectivity = false
        configuration.timeoutIntervalForRequest = idleTimeout
        configuration.timeoutIntervalForResource = resourceTimeout
        configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
        return URLSession(configuration: configuration)
    }
}

enum ReadRequestHeaders {
    static func apply(session: ReadSession, to request: inout URLRequest) throws {
        if session.audience.isPrivate { try requireSecureTransport(request.url) }
        request.setValue(session.audience.rawValue, forHTTPHeaderField: "X-GNSS-Audience")
        // Every audience's requests go to the origin: the public feed is served
        // with max-age=30, and a cached answer would be presented as a fresh poll.
        request.cachePolicy = .reloadIgnoringLocalCacheData
        if let token = session.token {
            try apply(token: token, to: &request)
        }
    }

    static func apply(token: String, to request: inout URLRequest) throws {
        guard ReadSession.isValidToken(token) else { throw FeedError.invalidCredential }
        try requireSecureTransport(request.url)
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
    }

    static func requireSecureTransport(_ url: URL?) throws {
        guard url?.scheme?.lowercased() == "https", url?.host != nil else {
            throw FeedError.invalidBaseURL
        }
    }
}

// Task-level delegation also protects injected sessions and every redirect hop.
// A credential's authority is confined to its original HTTPS origin.
final class CredentialRedirectGuard: NSObject, URLSessionTaskDelegate, Sendable {
    private let originalURL: URL?
    private let authenticated: Bool

    init(request: URLRequest) {
        originalURL = request.url
        authenticated = request.value(forHTTPHeaderField: "Authorization") != nil
    }

    func allows(_ url: URL?) -> Bool {
        guard authenticated else { return true }
        guard let url, let originalURL else { return false }
        return url.scheme?.lowercased() == "https"
            && originalURL.scheme?.lowercased() == "https"
            && url.host?.lowercased() == originalURL.host?.lowercased()
            && (url.port ?? 443) == (originalURL.port ?? 443)
            && url.user == nil && url.password == nil
    }

    func urlSession(_ session: URLSession, task: URLSessionTask,
                    willPerformHTTPRedirection response: HTTPURLResponse,
                    newRequest request: URLRequest,
                    completionHandler: @escaping @Sendable (URLRequest?) -> Void) {
        completionHandler(allows(request.url) ? request : nil)
    }
}

private struct APIErrorEnvelope: Decodable {
    let error: String?
}
