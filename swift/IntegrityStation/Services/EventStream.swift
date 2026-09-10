import Foundation

enum EventStreamUpdate: Sendable {
    case event(GNSSAPIEvent, cursor: String?)
    case resolved(GNSSAPIEvent, cursor: String?)
    case status(String)
}

struct EventStream: Sendable {
    private let session: URLSession

    init(session: URLSession = EventStream.failFastSession()) {
        self.session = session
    }

    /// Opens docs/OUTPUT.md §3's SSE stream. The caller persists and supplies
    /// the audience-scoped Last-Event-ID cursor on reconnect.
    func updates(session readSession: ReadSession, lastEventID: String?, onFailure: @escaping @Sendable () async -> Void = {}) throws -> AsyncThrowingStream<EventStreamUpdate, Error> {
        let url = try CollectorEndpoint.url(baseURL: readSession.baseURL, path: "gnss/events")
        let session = session

        return AsyncThrowingStream(bufferingPolicy: .bufferingOldest(NetworkLimits.pendingEvents)) { continuation in
            let task = Task {
                do {
                    var request = URLRequest(url: url)
                    request.timeoutInterval = 75
                    request.setValue("text/event-stream", forHTTPHeaderField: "Accept")
                    request.setValue("IntegrityStation/0.1", forHTTPHeaderField: "User-Agent")
                    try ReadRequestHeaders.apply(session: readSession, to: &request)
                    if let lastEventID, !lastEventID.isEmpty {
                        guard lastEventID.utf8.count <= 4_096,
                              !lastEventID.contains("\r"),
                              !lastEventID.contains("\n")
                        else { throw FeedError.invalidResponse }
                        request.setValue(lastEventID, forHTTPHeaderField: "Last-Event-ID")
                    }

                    let (bytes, response) = try await session.bytes(for: request, delegate: CredentialRedirectGuard(request: request))
                    defer { bytes.task.cancel() }
                    guard let http = response as? HTTPURLResponse else { throw FeedError.invalidResponse }
                    guard (200...299).contains(http.statusCode) else {
                        let data: Data
                        do { data = try await NetworkLimits.body(bytes, maximum: NetworkLimits.errorBytes) }
                        catch FeedError.inputLimit where http.statusCode == 401 || http.statusCode == 403 {
                            throw FeedClient.responseError(status: http.statusCode)
                        }
                        throw FeedClient.responseError(status: http.statusCode, data: data)
                    }

                    var accumulator = SSEAccumulator()
                    var line: [UInt8] = []
                    let decoder = JSONDecoder()
                    for try await byte in bytes {
                        try Task.checkCancellation()
                        guard byte == 0x0A else {
                            guard line.count < NetworkLimits.lineBytes else { throw FeedError.inputLimit }
                            line.append(byte)
                            continue
                        }
                        let text = String(decoding: line, as: UTF8.self)
                        line.removeAll(keepingCapacity: true)
                        guard let frame = try accumulator.consume(text) else { continue }

                        switch frame.event {
                        // a recognized state-bearing event that will
                        // not decode fails the stream. Silently skipping it (the
                        // previous `if let`) left transport healthy while the client
                        // accepted a later cursor, stepping permanently past a durable
                        // transition it never applied and continuing to present
                        // conditions as known. Throwing here reaches the outer catch,
                        // which runs onFailure and terminates the transport — the same
                        // path bounded-queue and transport failures already take, so
                        // the store invalidates and reconciles before any later cursor
                        // is accepted.
                        case "gnss":
                            guard let event = Self.decodeEvent(frame.data, decoder: decoder) else {
                                throw FeedError.malformedEvent(id: frame.id)
                            }
                            try Self.deliver(.event(event, cursor: frame.id), to: continuation)
                        case "resolved":
                            guard let event = Self.decodeEvent(frame.data, decoder: decoder) else {
                                throw FeedError.malformedEvent(id: frame.id)
                            }
                            try Self.deliver(.resolved(event, cursor: frame.id), to: continuation)
                        case "status":
                            let status = (try? decoder.decode(StreamStatus.self, from: Data(frame.data.utf8)))?.status
                            try Self.deliver(.status(status ?? frame.data), to: continuation)
                        default:
                            // Unknown event names stay ignorable for forward
                            // compatibility; only recognized state-bearing ones
                            // are integrity-critical.
                            continue
                        }
                    }
                    await onFailure()
                    continuation.finish(throwing: FeedError.streamEnded)
                } catch is CancellationError {
                    continuation.finish()
                } catch {
                    await onFailure()
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    // Overflow terminates transport and surfaces an error after the bounded
    // queue drains. The store marks conditions unknown and reconciles on retry.
    static func deliver(_ update: EventStreamUpdate, to continuation: AsyncThrowingStream<EventStreamUpdate, Error>.Continuation) throws {
        switch continuation.yield(update) {
        case .enqueued: return
        case .dropped: throw FeedError.inputLimit
        case .terminated: throw CancellationError()
        @unknown default: throw FeedError.inputLimit
        }
    }

    private static func decodeEvent(_ payload: String, decoder: JSONDecoder) -> GNSSAPIEvent? {
        try? decoder.decode(GNSSAPIEvent.self, from: Data(payload.utf8))
    }

    private static func failFastSession() -> URLSession {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.waitsForConnectivity = false
        configuration.timeoutIntervalForRequest = 10
        configuration.timeoutIntervalForResource = 90
        return URLSession(configuration: configuration)
    }
}

private struct StreamStatus: Decodable {
    let status: String
}
