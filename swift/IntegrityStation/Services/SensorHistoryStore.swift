import Foundation
import Observation

@MainActor @Observable
final class SensorHistoryStore {
    private(set) var samples: [SensorHistorySample] = []
    private(set) var page: SensorHistoryPage?
    private(set) var nextRequest: SensorHistoryRequest?
    private(set) var isLoading = false
    private(set) var errorMessage: String?
    @ObservationIgnored private var generation = 0
    @ObservationIgnored private var session: ReadSession?

    func reset() {
        generation += 1; session = nil; isLoading = false
        samples = []; page = nil; nextRequest = nil; errorMessage = nil
    }

    func load(session: ReadSession, request: SensorHistoryRequest, append: Bool = false,
              fetch: (ReadSession, SensorHistoryRequest) async throws -> SensorHistoryPage) async throws {
        if append {
            guard self.session == session, nextRequest == request, samples.count < 5000, !isLoading
            else { throw FeedError.invalidResponse }
        } else {
            reset(); self.session = session
        }
        generation += 1
        let generation = self.generation
        isLoading = true; errorMessage = nil
        defer { if generation == self.generation { isLoading = false } }
        do {
            let result = try await fetch(session, request)
            try Task.checkCancellation()
            guard generation == self.generation, self.session == session else { return }
            try result.validate(request: request, session: session)
            if append, let last = samples.last, let first = result.samples.first,
               let lastTime = WireDate.parse(last.receivedAt), let firstTime = WireDate.parse(first.receivedAt),
               firstTime < lastTime { throw FeedError.invalidResponse }
            samples = append ? samples + result.samples : result.samples
            page = result
            nextRequest = samples.count < 5000 ? request.continuation(result) : nil
        } catch {
            guard generation == self.generation else { return }
            samples = []; page = nil; nextRequest = nil
            if let failure = error as? FeedError, case .http(409) = failure {
                errorMessage = String(localized: "history.changed")
            } else if !(error is CancellationError) { errorMessage = error.localizedDescription }
            throw error
        }
    }
}
