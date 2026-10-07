import Foundation
import SwiftUI
import Testing
@testable import IntegrityStation

private let historySession = ReadSession(baseURL: URL(string: "https://collector.example.invalid")!,
                                         principalID: "reader", audience: ReadAudience("organization:example")!,
                                         authorizationRevision: "grant-1", token: "test-read-token")!

private func historyRequest() throws -> SensorHistoryRequest {
    try SensorHistoryRequest(observer: "receiver: 001/東京", metric: .temperature, hours: 1,
                             now: WireDate.parse("2026-09-30T12:00:00Z")!)
}

private func historyDocument(_ changes: [String: Any] = [:]) -> [String: Any] {
    var result: [String: Any] = [
        "schema": "2.0", "audience": "organization:example", "observer": "receiver: 001/東京", "kind": "environment",
        "since": "2026-09-30T11:00:00Z", "until": "2026-09-30T12:00:00Z", "visible_since": "2026-09-30T10:00:00Z",
        "effective_since": "2026-09-30T11:00:00Z", "history_limited": false, "revision": "history-1", "offset": 0,
        "limit": 500, "has_more": false, "pagination_limited": false, "next_offset": NSNull(),
        "samples": [["received_at": "2026-09-30T11:00:00Z", "sample_time": NSNull(), "session": "a",
                     "sequence": "18446744073709551615", "hardware_trust": "trusted",
                     "details": ["environment": ["mcp9808_c": 0, "humidity_percent": NSNull()]]]]
    ]
    result.merge(changes, uniquingKeysWith: { _, new in new })
    return result
}

private func historyPage(_ changes: [String: Any] = [:]) throws -> SensorHistoryPage {
    try JSONDecoder().decode(SensorHistoryPage.self, from: JSONSerialization.data(withJSONObject: historyDocument(changes)))
}

@Test func historyKeepsOpaqueIDsNullsZeroAndFullWidthSequence() throws {
    let request = try historyRequest(), page = try historyPage()
    try page.validate(request: request, session: historySession)
    #expect(request.query.first { $0.name == "observer" }?.value == "receiver: 001/東京")
    #expect(page.samples[0].sequence == String(UInt64.max))
    #expect(page.samples[0].sampleTime == nil)
    #expect(SensorMetric.temperature.value(in: page.samples[0]) == 0)
    #expect(SensorMetric.humidity.value(in: page.samples[0]) == nil)
    #expect(page.samples[0].hardwareTrust == "trusted")
}

@Test func historyRejectsInvalidBoundariesAndContinuations() throws {
    let request = try historyRequest()
    // A minor schema bump is additive and accepted; another major is not.
    try historyPage(["schema": "2.1"]).validate(request: request, session: historySession)
    for change: [String: Any] in [
        ["schema": "3.0"], ["schema": "2"],
        ["audience": "organization:other"], ["observer": "other"], ["kind": "timing"],
        ["since": "2026-09-30T10:00:00Z"], ["until": "2026-09-30T13:00:00Z"], ["history_limited": true],
        ["revision": ""], ["offset": 1], ["limit": 501], ["next_offset": 1],
        ["has_more": true, "next_offset": 0], ["pagination_limited": true],
        ["has_more": true, "samples": [], "next_offset": 1],
        ["samples": [["received_at": "2026-09-30T10:59:59Z"]]],
        ["samples": [["received_at": "2026-09-30T11:00:00Z", "sequence": "18446744073709551616"]]]
    ] {
        #expect(throws: FeedError.invalidResponse) { try historyPage(change).validate(request: request, session: historySession) }
    }
    let page = try historyPage(["has_more": true, "next_offset": 1])
    try page.validate(request: request, session: historySession)
    let next = try #require(request.continuation(page))
    #expect(next.offset == 1 && next.revision == "history-1" && next.until == page.until)
    #expect(throws: FeedError.invalidResponse) {
        try historyPage(["offset": 1, "revision": "history-2"]).validate(request: next, session: historySession)
    }
}

@Test func historyPolicyBoundaryAfterWindowIsAnExplicitEmptyResult() throws {
    let page = try historyPage(["visible_since": "2026-09-30T13:00:00Z", "effective_since": "2026-09-30T13:00:00Z",
                                "history_limited": true, "samples": []])
    try page.validate(request: historyRequest(), session: historySession)
    #expect(page.historyLimited && page.samples.isEmpty)
}

@Test func nativeHistoryBreaksNullsBootsAndReceiptGaps() throws {
    let samples: [[String: Any]] = [
        ["received_at": "2026-09-30T11:00:00Z", "session": "a", "details": ["environment": ["mcp9808_c": 0]]],
        ["received_at": "2026-09-30T11:00:30Z", "session": "a", "details": ["environment": ["mcp9808_c": 1]]],
        ["received_at": "2026-09-30T11:01:00Z", "session": "a"],
        ["received_at": "2026-09-30T11:01:30Z", "session": "a", "details": ["environment": ["mcp9808_c": 2]]],
        ["received_at": "2026-09-30T11:02:00Z", "session": "b", "details": ["environment": ["mcp9808_c": 3]]],
        ["received_at": "2026-09-30T11:20:00Z", "session": "b", "details": ["environment": ["mcp9808_c": 4]]]
    ]
    let points = SensorHistoryPoint.make(samples: try historyPage(["samples": samples]).samples, metric: .temperature)
    #expect(points.map(\.value) == [0, 1, 2, 3, 4])
    #expect(points.map(\.segment) == [0, 0, 1, 2, 3])
}

@MainActor @Test func nativeHistoryClearsOnDenialAndIgnoresRetiredReads() async throws {
    let store = SensorHistoryStore(), request = try historyRequest(), page = try historyPage()
    try await store.load(session: historySession, request: request) { _, _ in page }
    #expect(store.samples.count == 1)
    do { try await store.load(session: historySession, request: request) { _, _ in throw FeedError.forbidden(nil) } }
    catch { #expect(store.samples.isEmpty && store.page == nil && store.errorMessage != nil) }
    let gate = HistoryReadGate()
    let task = Task { try await store.load(session: historySession, request: request) { _, _ in await gate.wait(); return page } }
    await gate.started()
    store.reset()
    await gate.resume()
    try await task.value
    #expect(store.samples.isEmpty && store.page == nil && !store.isLoading)
}

private actor HistoryReadGate {
    private var continuation: CheckedContinuation<Void, Never>?
    private var observer: CheckedContinuation<Void, Never>?
    func wait() async {
        await withCheckedContinuation { continuation in
            self.continuation = continuation; observer?.resume(); observer = nil
        }
    }
    func started() async {
        if continuation != nil { return }
        await withCheckedContinuation { observer = $0 }
    }
    func resume() { continuation?.resume(); continuation = nil }
}

@MainActor @Test func nativeHistoryChartRendersWithGapsAndLocalPalette() throws {
    var samples: [[String: Any]] = []
    let start = WireDate.parse("2026-09-30T11:00:00Z")!
    for index in 0..<30 {
        let value: Any = (12...15).contains(index) ? NSNull() : 24 + sin(Double(index) / 3)
        let environment: [String: Any] = ["mcp9808_c": value]
        samples.append(["received_at": start.addingTimeInterval(Double(index) * 60).ISO8601Format(),
                        "session": index < 22 ? "a" : "b", "details": ["environment": environment]])
    }
    let page = try historyPage(["samples": samples])
    #expect(SensorHistoryPoint.make(samples: page.samples, metric: .temperature).count == 26)
    let renderer = ImageRenderer(content: InstrumentCard("history.title", systemImage: "chart.xyaxis.line") {
        SensorHistoryChart(samples: page.samples, metric: .temperature, hours: 1)
    }.environment(\.colorScheme, .dark).frame(width: 700).padding(24))
    let image = try #require(renderer.cgImage)
    #expect(image.width > 600 && image.height > 200)
    #if os(macOS)
    let bitmap = NSBitmapImageRep(cgImage: image)
    let png = try #require(bitmap.representation(using: .png, properties: [:]))
    try png.write(to: FileManager.default.temporaryDirectory.appending(path: "navlistener-history-review.png"))
    #endif
}

@Suite(.serialized) struct SensorHistoryNetworkingTests {
    /// Opaque ids may carry any punctuation. The collector decodes the query
    /// with url.ParseQuery, where a bare `+` is a space, so `+` (and `&`, `=`,
    /// `%`) must reach the wire percent-encoded or the page comes back for a
    /// different observer and fails validation.
    @Test(arguments: ["receiver: 001/東京", "rx+1 a&b=c%d"])
    func historyScopesBothDiscoveryChecksAndEscapesOpaqueReceiver(observer: String) async throws {
        let client = makeClient()
        nonisolated(unsafe) var paths: [String] = []
        nonisolated(unsafe) var rawQueries: [String] = []
        HistoryURLProtocol.handler = { request in
            paths.append(request.url!.path)
            #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer test-read-token")
            if request.url!.path.hasSuffix("audiences") { return Self.discovery(revision: "grant-1") }
            #expect(request.value(forHTTPHeaderField: "X-GNSS-Audience") == "organization:example")
            rawQueries.append(request.url!.query(percentEncoded: true) ?? "")
            let query = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems
            #expect(query?.first { $0.name == "observer" }?.value == observer)
            #expect(!request.url!.absoluteString.contains("test-read-token"))
            return historyDocument(["observer": observer])
        }
        defer { HistoryURLProtocol.handler = nil }
        let request = try SensorHistoryRequest(observer: observer, metric: .temperature, hours: 1,
                                               now: WireDate.parse("2026-09-30T12:00:00Z")!)
        let result = try await client.fetchSensorHistory(session: historySession, request: request)
        #expect(result.samples.count == 1)
        #expect(paths.map { $0.components(separatedBy: "/").last! } == ["audiences", "observer-samples", "audiences"])
        let raw = try #require(rawQueries.first)
        #expect(!raw.contains("+"))
        if observer.contains("+") {
            #expect(raw.contains("observer=rx%2B1%20a%26b%3Dc%25d&"))
        }
    }

    @Test func queryItemsEscapeWhatTheCollectorWouldOtherwiseDecode() throws {
        var components = try #require(URLComponents(string: "https://collector.invalid/gnss/api/v2/updates"))
        let observer = "rx+1 a&b=c%d:東京"
        components.percentEncodedQueryItems = try CollectorEndpoint.percentEncodedQueryItems(
            [URLQueryItem(name: "observer_id", value: observer)])
        #expect(components.url?.query(percentEncoded: true) == "observer_id=rx%2B1%20a%26b%3Dc%25d:%E6%9D%B1%E4%BA%AC")
        #expect(components.queryItems?.first?.value == observer)
        #expect(try CollectorEndpoint.percentEncodedQueryItems([URLQueryItem(name: "flag", value: nil)]).first?.value == nil)
    }

    @Test func grantChangeDiscardsHistoryAndPublicHistoryIsNeverRequested() async throws {
        let client = makeClient()
        nonisolated(unsafe) var count = 0
        HistoryURLProtocol.handler = { request in
            if request.url!.path.hasSuffix("audiences") {
                count += 1
                return Self.discovery(revision: count == 1 ? "grant-1" : "changed")
            }
            return historyDocument()
        }
        defer { HistoryURLProtocol.handler = nil }
        // The grant is intact but its revision moved: history access changed,
        // the owner re-discovers and the view reloads under the new session.
        await #expect(throws: FeedError.revisionChanged) {
            try await client.fetchSensorHistory(session: historySession, request: historyRequest())
        }
        // A withdrawn grant is a genuine loss even under the original revision.
        HistoryURLProtocol.handler = { request in
            if request.url!.path.hasSuffix("audiences") {
                count += 1
                return ["schema": "2.0", "principal": "reader", "revision": "grant-1", "audiences": ["public"]]
            }
            return historyDocument()
        }
        await #expect(throws: FeedError.audienceLost) {
            try await client.fetchSensorHistory(session: historySession, request: historyRequest())
        }
        let publicSession = ReadSession(baseURL: historySession.baseURL, principalID: nil, audience: .publicAudience,
                                        authorizationRevision: "public", token: nil)!
        await #expect(throws: FeedError.forbidden(nil)) {
            try await client.fetchSensorHistory(session: publicSession, request: historyRequest())
        }
        #expect(count == 3)
    }

    private func makeClient() -> FeedClient {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [HistoryURLProtocol.self]
        return FeedClient(session: URLSession(configuration: config))
    }
    private static func discovery(revision: String) -> [String: Any] {
        ["schema": "2.0", "principal": "reader", "revision": revision, "audiences": ["public", "organization:example"]]
    }
}

private final class HistoryURLProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handler: ((URLRequest) -> [String: Any])?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let body = try! JSONSerialization.data(withJSONObject: ["ok": true, "data": Self.handler!(request)])
        client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: nil,
                                                            headerFields: ["Content-Type": "application/json"])!, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body); client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}
