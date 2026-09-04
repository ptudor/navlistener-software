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
    func updates(session readSession: ReadSession, lastEventID: String?) throws -> AsyncThrowingStream<EventStreamUpdate, Error> {
        let url = try CollectorEndpoint.url(baseURL: readSession.baseURL, path: "gnss/events")
        let session = session

        return AsyncThrowingStream { continuation in
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
                    guard let http = response as? HTTPURLResponse else { throw FeedError.invalidResponse }
                    guard (200...299).contains(http.statusCode) else {
                        throw FeedClient.responseError(status: http.statusCode)
                    }

                    var accumulator = SSEAccumulator()
                    var line: [UInt8] = []
                    let decoder = JSONDecoder()
                    for try await byte in bytes {
                        try Task.checkCancellation()
                        guard byte == 0x0A else {
                            line.append(byte)
                            continue
                        }
                        let text = String(decoding: line, as: UTF8.self)
                        line.removeAll(keepingCapacity: true)
                        guard let frame = accumulator.consume(text) else { continue }

                        switch frame.event {
                        case "gnss":
                            if let event = Self.decodeEvent(frame.data, decoder: decoder) {
                                continuation.yield(.event(event, cursor: frame.id))
                            }
                        case "resolved":
                            if let event = Self.decodeEvent(frame.data, decoder: decoder) {
                                continuation.yield(.resolved(event, cursor: frame.id))
                            }
                        case "status":
                            let status = (try? decoder.decode(StreamStatus.self, from: Data(frame.data.utf8)))?.status
                            continuation.yield(.status(status ?? frame.data))
                        default:
                            continue
                        }
                    }
                    continuation.finish(throwing: FeedError.streamEnded)
                } catch is CancellationError {
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in task.cancel() }
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
