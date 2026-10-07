import Foundation

struct OwnerPortalClient: Sendable {
    /// Largest portal document accepted, bounded on received bytes.
    static let responseBytes = 8 * 1024 * 1024
    /// Idle watchdog between bytes, and a total bound that admits a document
    /// at the response limit over a slow link (8 MiB in 120 s is about
    /// 560 kbit/s); the former 20 s could never complete one.
    static let idleTimeout: TimeInterval = 15
    static let resourceTimeout: TimeInterval = 120

    let session: URLSession
    init(session: URLSession? = nil) {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.httpShouldSetCookies = false
        configuration.urlCache = nil
        configuration.timeoutIntervalForRequest = Self.idleTimeout
        configuration.timeoutIntervalForResource = Self.resourceTimeout
        self.session = session ?? URLSession(configuration: configuration)
    }
    static func decoder() -> JSONDecoder { let decoder = JSONDecoder(); decoder.keyDecodingStrategy = .convertFromSnakeCase; return decoder }
    func request<T: Decodable & Sendable>(_ type: T.Type, installation: PortalInstallation, token: String? = nil,
                                         path: String, query: [URLQueryItem] = [], form: [String: String]? = nil) async throws -> T {
        var parts = URLComponents(url: installation.endpoint(path), resolvingAgainstBaseURL: false)!
        if !query.isEmpty { parts.queryItems = query }
        var request = URLRequest(url: parts.url!)
        request.cachePolicy = .reloadIgnoringLocalCacheData
        request.httpShouldHandleCookies = false
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        if let token { try ReadRequestHeaders.apply(token: token, to: &request) }
        if let form {
            request.httpMethod = "POST"
            request.setValue("application/x-www-form-urlencoded", forHTTPHeaderField: "Content-Type")
            var fields = URLComponents()
            fields.queryItems = form.sorted(by: { $0.key < $1.key }).map { URLQueryItem(name: $0.key, value: $0.value) }
            request.httpBody = fields.percentEncodedQuery?.replacingOccurrences(of: "+", with: "%2B").data(using: .utf8)
        }
        let (bytes, response) = try await session.bytes(for: request, delegate: PortalRedirectGuard())
        defer { bytes.task.cancel() }
        guard let http = response as? HTTPURLResponse else { throw FeedError.invalidResponse }
        // Check denial before reading its body, even if that body is oversized or never completes.
        if http.statusCode == 401 { throw FeedError.unauthorized(nil) }
        if http.statusCode == 403 { throw FeedError.forbidden(nil) }
        guard http.statusCode == 200 else { throw FeedError.http(http.statusCode) }
        guard http.mimeType == "application/json", response.expectedContentLength <= Self.responseBytes else { throw FeedError.invalidResponse }
        let data = try await NetworkLimits.body(bytes, maximum: Self.responseBytes)
        do { return try Self.decoder().decode(type, from: data) } catch { throw FeedError.invalidResponse }
    }
    func get<T: Decodable & Sendable>(_ type: T.Type, connection: PortalConnection, path: String,
                                     query: [URLQueryItem] = []) async throws -> T {
        try await request(type, installation: connection.installation, token: connection.token, path: path, query: query)
    }
    func identity(_ connection: PortalConnection) async throws -> PortalIdentity {
        let identity = try await get(PortalIdentity.self, connection: connection, path: "api/v1/me/")
        guard identity.version == 1, identity.sessionId == connection.sessionID,
              identity.user.id == connection.accountID, identity.fleets.count <= 10000,
              Set(identity.fleets.map(\.id)).count == identity.fleets.count,
              identity.fleets.allSatisfy({ ["organization", "collection"].contains($0.kind) }) else { throw FeedError.invalidResponse }
        return identity
    }
    func exchange(_ authorization: PortalAuthorization, callback: URL) async throws -> (PortalConnection, PortalIdentity) {
        let code = try authorization.code(from: callback)
        let result = try await request(PortalToken.self, installation: authorization.installation, path: "api/v1/native/token/",
            form: ["client_id": PortalAuthorization.clientID, "redirect_uri": PortalAuthorization.callback,
                   "grant_type": "authorization_code", "code": code, "code_verifier": authorization.verifier])
        guard result.issuer == authorization.installation.url.absoluteString, result.tokenType == "Bearer", result.scope == "stations:read",
              PortalAuthorization.validToken(result.accessToken), result.expiresIn > 0, result.expiresIn <= 30 * 86400
        else { throw FeedError.invalidResponse }
        let identity = try await request(PortalIdentity.self, installation: authorization.installation, token: result.accessToken, path: "api/v1/me/")
        let connection = PortalConnection(installation: authorization.installation, token: result.accessToken,
                                          sessionID: result.sessionId, accountID: identity.user.id,
                                          expiresAt: Date().addingTimeInterval(Double(result.expiresIn)))
        guard identity.version == 1, identity.sessionId == result.sessionId, !identity.user.id.isEmpty,
              identity.fleets.count <= 10000, Set(identity.fleets.map(\.id)).count == identity.fleets.count,
              identity.fleets.allSatisfy({ ["organization", "collection"].contains($0.kind) }) else { throw FeedError.invalidResponse }
        return (connection, identity)
    }
    func revoke(_ connection: PortalConnection) async throws {
        struct Result: Decodable, Sendable { let revoked: Bool }
        let result = try await request(Result.self, installation: connection.installation, token: connection.token,
                                       path: "api/v1/native/revoke/", form: [:])
        guard result.revoked else { throw FeedError.invalidResponse }
    }
}

// Portal sessions are installation-specific. Even a same-origin redirect can
// leave the configured application prefix; never forward credentials or PKCE forms.
final class PortalRedirectGuard: NSObject, URLSessionTaskDelegate, Sendable {
    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                    newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void) {
        completionHandler(nil)
    }
}
