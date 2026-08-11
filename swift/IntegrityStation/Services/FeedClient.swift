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

        let (data, response) = try await self.session.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw FeedError.invalidResponse }
        guard (200...299).contains(http.statusCode) else {
            throw Self.responseError(status: http.statusCode, data: data)
        }

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
        request.setValue(session.audience.rawValue, forHTTPHeaderField: "X-GNSS-Audience")
        if let token = session.token {
            try apply(token: token, to: &request)
            request.cachePolicy = .reloadIgnoringLocalCacheData
        }
    }

    static func apply(token: String, to request: inout URLRequest) throws {
        guard ReadSession.isValidToken(token) else { throw FeedError.invalidCredential }
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
    }
}

private struct APIErrorEnvelope: Decodable {
    let error: String?
}
