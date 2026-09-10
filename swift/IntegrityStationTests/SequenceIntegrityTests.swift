import Foundation
import Testing
@testable import IntegrityStation

/// Serves one fixed SSE body and records whether the transport was cancelled.
private final class ScriptedSSEProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var body = ""
    private static let lock = NSLock()
    nonisolated(unsafe) private static var wasCancelled = false
    static var cancelled: Bool {
        get { lock.withLock { wasCancelled } }
        set { lock.withLock { wasCancelled = newValue } }
    }
    private var producer: Task<Void, Never>?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let body = Self.body
        producer = Task {
            client?.urlProtocol(self, didReceive: HTTPURLResponse(
                url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: [:])!,
                cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: Data(body.utf8))
            // Hold the connection open so the stream ends only via a decode
            // failure, never by the body simply running out.
            do {
                for _ in 0..<2000 {
                    try Task.checkCancellation()
                    try await Task.sleep(for: .milliseconds(5))
                }
            } catch {}
            client?.urlProtocolDidFinishLoading(self)
        }
    }
    override func stopLoading() { Self.cancelled = true; producer?.cancel() }
}

private actor FailureFlag {
    var failed = false
    func mark() { failed = true }
}

/// regression fix / regression fix — one sequence-integrity contract. A client's
/// `Last-Event-ID` may only ever name a transition it actually applied. Two
/// separate holes broke that: `EventStream` silently discarded a recognized
/// `gnss`/`resolved` event that would not decode while leaving transport
/// healthy, and `StationStore` advanced and persisted the cursor *before*
/// applying the event it named.
@Suite(.serialized)
struct SequenceIntegrityTests {
    private func session() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [ScriptedSSEProtocol.self]
        return URLSession(configuration: config)
    }
    private func readSession() throws -> ReadSession {
        try #require(ReadSession(baseURL: URL(string: "https://sequence.invalid")!,
                                 principalID: nil, audience: .publicAudience,
                                 authorizationRevision: "a", token: nil))
    }
    private func frame(id: Int, event: String, data: String) -> String {
        "id: \(id)\nevent: \(event)\ndata: \(data)\n\n"
    }
    private var validEvent: String {
        #"{"id":1,"sv":"roof","type":"jamming_detected","new_value":"jammed","severity":1}"#
    }

    /// A recognized state-bearing event that cannot be decoded must fail the
    /// stream, run the failure callback once, cancel the transport, and never
    /// deliver the later valid event whose higher id would move the cursor past
    /// the transition that was skipped.
    @Test(arguments: [
        // wrong types for fields the model declares
        #"{"id":"not-a-number","sv":"roof","type":"jamming_detected","severity":1}"#,
        // not JSON at all
        "{ this is not json",
        // valid JSON, wrong shape entirely
        "[1,2,3]",
    ])
    func malformedRecognizedEventFailsTheStream(payload: String) async throws {
        ScriptedSSEProtocol.cancelled = false
        ScriptedSSEProtocol.body =
            frame(id: 1, event: "gnss", data: validEvent)
            + frame(id: 2, event: "gnss", data: payload)
            + frame(id: 3, event: "gnss", data: validEvent)
        let network = session(); defer { network.invalidateAndCancel() }
        let failure = FailureFlag()
        let updates = try EventStream(session: network).updates(
            session: readSession(), lastEventID: nil, onFailure: { await failure.mark() })

        var delivered: [String?] = []
        var thrown: Error?
        do {
            for try await update in updates {
                switch update {
                case .event(_, let cursor), .resolved(_, let cursor): delivered.append(cursor)
                case .status: continue
                }
            }
            Issue.record("stream completed despite an undecodable recognized event")
        } catch { thrown = error }

        #expect(thrown is FeedError)
        if case .malformedEvent = thrown as? FeedError {} else {
            Issue.record("expected FeedError.malformedEvent, got \(String(describing: thrown))")
        }
        // The first event is delivered; the malformed one and everything after it
        // are not — so no later cursor can be accepted past the gap.
        #expect(delivered == ["1"])
        #expect(await failure.failed)
        for _ in 0..<100 where !ScriptedSSEProtocol.cancelled {
            try await Task.sleep(for: .milliseconds(2))
        }
        #expect(ScriptedSSEProtocol.cancelled)
    }

    /// Unknown event names stay ignorable for forward compatibility: only
    /// recognized state-bearing events are integrity-critical.
    @Test func unknownEventNamesRemainIgnorable() async throws {
        ScriptedSSEProtocol.cancelled = false
        ScriptedSSEProtocol.body =
            frame(id: 1, event: "gnss", data: validEvent)
            + frame(id: 2, event: "some_future_event", data: "{\"whatever\":true}")
            + frame(id: 3, event: "gnss", data: validEvent)
        let network = session(); defer { network.invalidateAndCancel() }
        let updates = try EventStream(session: network).updates(
            session: readSession(), lastEventID: nil)

        var delivered: [String?] = []
        for try await update in updates {
            switch update {
            case .event(_, let cursor), .resolved(_, let cursor): delivered.append(cursor)
            case .status: continue
            }
            if delivered.count == 2 { break }
        }
        #expect(delivered == ["1", "3"])
    }
}

/// regression fix, at the level the reordering actually governs: the durable
/// cursor must never name an event that was not applied. `ConditionState.apply`
/// throwing is the reachable failure (`FeedError.inputLimit` at the condition
/// cap), and before the fix the cursor had already been advanced and written to
/// the scoped cache by then.
@Suite(.serialized)
struct CursorOrderingTests {
    private func event(id: Int, sv: String, value: String) throws -> GNSSAPIEvent {
        try JSONDecoder().decode(GNSSAPIEvent.self, from: Data(
            "{\"id\":\(id),\"sv\":\"\(sv)\",\"type\":\"jamming_detected\",\"new_value\":\"\(value)\",\"severity\":1}".utf8))
    }

    /// Applying the same event id twice is harmless, which is what makes
    /// apply-then-record's at-least-once replay window safe.
    @Test func applicationIsIdempotentByEventID() throws {
        var state = ConditionState()
        let e = try event(id: 7, sv: "roof", value: "jammed")
        #expect(try state.apply(e))
        let afterFirst = state.active.count
        // A replayed duplicate must not add a second active condition, and must
        // report no transition, so notifications cannot fire twice for it.
        #expect(try state.apply(e) == false)
        #expect(state.active.count == afterFirst)
    }

    /// A condition-cap overflow throws, and must do so without the transition
    /// having been half-applied — the caller then reconciles rather than
    /// acknowledging the id.
    @Test func conditionCapOverflowThrowsRatherThanSilentlyDropping() throws {
        var state = ConditionState()
        var threw: Error?
        var accepted = 0
        do {
            // One past the cap: ids start at 1 because apply's floor rejects 0.
            for i in 1...(NetworkLimits.conditions + 1) {
                if try state.apply(try event(id: i, sv: "roof-\(i)", value: "jammed")) { accepted += 1 }
            }
        } catch { threw = error }
        #expect(accepted == NetworkLimits.conditions,
                "the cap must admit exactly NetworkLimits.conditions distinct conditions")
        #expect(threw as? FeedError == .inputLimit,
                "the condition cap must surface as an error the stream loop can reconcile from")
        // And it must have marked state unknown, so the loop reconciles rather
        // than continuing to present a partial projection as complete.
        #expect(state.isKnown == false)
    }
}
