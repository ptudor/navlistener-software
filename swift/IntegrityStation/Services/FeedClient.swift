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
    case server(code: Int?, message: String)
    case missingData
    case inputLimit
    case streamEnded
    // a recognized state-bearing event ("gnss"/"resolved") that
    // cannot be decoded is a stream-integrity failure, not a frame to skip.
    // Skipping it let the client accept a later cursor and step permanently past
    // a durable transition while still presenting conditions as known.
    case malformedEvent(id: String?)

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
        case .server(_, let message): message
        case .missingData: String(localized: "error.missing_data")
        case .inputLimit: String(localized: "error.input_limit")
        case .streamEnded: String(localized: "error.stream_ended")
        case .malformedEvent: String(localized: "error.malformed_event")
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

    static func canRetry(_ error: Error) -> Bool {
        if let error = error as? URLError {
            return error.code != .cancelled && error.code != .userAuthenticationRequired
                && error.code != .userCancelledAuthentication && error.code != .badURL
        }
        if case FeedError.http(let status) = error {
            return status == 408 || status == 421 || status == 429 || (500...599).contains(status)
        }
        return false
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
}

struct FeedClient: Sendable {
    private let session: URLSession

    init(session: URLSession = FeedClient.failFastSession()) {
        self.session = session
    }

    func fetchAudiences(baseURL: URL, token: String?) async throws -> APIEnvelope<AudienceDiscoveryPayload> {
        try await fetch(baseURL: baseURL, path: "gnss/api/v2/audiences", token: token)
    }

    func updateAccess(session: ReadSession, observer: String, action: String? = nil,
                      choice: UpdateAccess.Choice? = nil, requestID: String = UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased()) async throws -> UpdateAccess {
        guard session.audience.isPrivate, session.token != nil else { throw FeedError.forbidden(nil) }
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
            components?.queryItems = [URLQueryItem(name: "observer_id", value: observer)]
            request.url = components?.url
        }
        func once(_ request: URLRequest) async throws -> UpdateAccess {
            let (bytes, response) = try await self.session.bytes(for: request, delegate: CredentialRedirectGuard(request: request))
            defer { bytes.task.cancel() }
            guard let http = response as? HTTPURLResponse else { throw FeedError.invalidResponse }
            guard (200...299).contains(http.statusCode) else { throw Self.responseError(status: http.statusCode) }
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
            components?.queryItems = [URLQueryItem(name: "since", value: since.ISO8601Format())]
        }
        guard let url = components?.url else { throw FeedError.invalidBaseURL }
        return try await fetch(url: url, session: session)
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

        let envelope: APIEnvelope<Payload>
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

    private static func failFastSession() -> URLSession {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.waitsForConnectivity = false
        configuration.timeoutIntervalForRequest = 10
        configuration.timeoutIntervalForResource = 15
        configuration.requestCachePolicy = .useProtocolCachePolicy
        return URLSession(configuration: configuration)
    }
}

enum ReadRequestHeaders {
    static func apply(session: ReadSession, to request: inout URLRequest) throws {
        if session.audience.isPrivate { try requireSecureTransport(request.url) }
        request.setValue(session.audience.rawValue, forHTTPHeaderField: "X-GNSS-Audience")
        if let token = session.token {
            try apply(token: token, to: &request)
            request.cachePolicy = .reloadIgnoringLocalCacheData
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
