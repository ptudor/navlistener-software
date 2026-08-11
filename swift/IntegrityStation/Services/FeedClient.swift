import Foundation

enum FeedError: Error, Equatable, LocalizedError, Sendable {
    case invalidBaseURL
    case invalidResponse
    case http(Int)
    case server(code: Int?, message: String)
    case missingData
    case streamEnded

    var errorDescription: String? {
        switch self {
        case .invalidBaseURL: String(localized: "error.invalid_base_url")
        case .invalidResponse: String(localized: "error.invalid_response")
        case .http(let code): String(format: String(localized: "error.http"), code)
        case .server(_, let message): message
        case .missingData: String(localized: "error.missing_data")
        case .streamEnded: String(localized: "error.stream_ended")
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

    func fetchObservers(baseURL: URL) async throws -> APIEnvelope<ObserversPayload> {
        try await fetch(baseURL: baseURL, path: "gnss/api/v2/observers")
    }

    func fetchEvents(baseURL: URL, since: Date? = nil) async throws -> APIEnvelope<EventsPayload> {
        let endpoint = try CollectorEndpoint.url(baseURL: baseURL, path: "gnss/api/events")
        var components = URLComponents(url: endpoint, resolvingAgainstBaseURL: false)
        if let since {
            components?.queryItems = [URLQueryItem(name: "since", value: since.ISO8601Format())]
        }
        guard let url = components?.url else { throw FeedError.invalidBaseURL }
        return try await fetch(url: url)
    }

    private func fetch<Payload: Codable & Sendable>(
        baseURL: URL,
        path: String
    ) async throws -> APIEnvelope<Payload> {
        try await fetch(url: CollectorEndpoint.url(baseURL: baseURL, path: path))
    }

    private func fetch<Payload: Codable & Sendable>(url: URL) async throws -> APIEnvelope<Payload> {
        var request = URLRequest(url: url)
        request.timeoutInterval = 10
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.setValue("IntegrityStation/0.1", forHTTPHeaderField: "User-Agent")

        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw FeedError.invalidResponse }
        guard (200...299).contains(http.statusCode) else { throw FeedError.http(http.statusCode) }

        let envelope = try JSONDecoder().decode(APIEnvelope<Payload>.self, from: data)
        guard envelope.ok else {
            throw FeedError.server(
                code: envelope.code,
                message: envelope.error ?? String(localized: "error.server")
            )
        }
        guard envelope.data != nil else { throw FeedError.missingData }
        return envelope
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
