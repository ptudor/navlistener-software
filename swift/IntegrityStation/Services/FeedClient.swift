import Foundation

enum FeedError: Error, Equatable, LocalizedError, Sendable {
    case invalidBaseURL
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

    var errorDescription: String? {
        switch self {
        case .invalidBaseURL: String(localized: "error.invalid_base_url")
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
