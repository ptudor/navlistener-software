import XCTest
import Network

// Local read-only collector fixture. It exercises the shipped UI and request
// path without an account, receiver, or external service.
private final class SelectionCollector: @unchecked Sendable {
    let listener: NWListener
    init(ready: XCTestExpectation) throws {
        listener = try NWListener(using: .tcp, on: .any)
        listener.stateUpdateHandler = { state in if case .ready = state { ready.fulfill() } }
        listener.newConnectionHandler = { connection in
            connection.start(queue: .global())
            Self.read(connection, pending: Data())
        }
        listener.start(queue: .global())
    }
    deinit { listener.cancel() }
    private static func read(_ connection: NWConnection, pending: Data) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 8192) { data, _, _, error in
            guard error == nil, let data else { connection.cancel(); return }
            let all = pending + data
            guard all.count <= 16384 else { connection.cancel(); return }
            let request = String(decoding: all, as: UTF8.self)
            guard request.contains("\r\n\r\n") else { read(connection, pending: all); return }
            let body: String
            if request.contains("/audiences ") {
                body = #"{"ok":true,"data":{"schema":"2.0","revision":"ui","audiences":["public"]}}"#
            } else if request.contains("/observers ") {
                body = #"{"ok":true,"data":{"schema":"2.0","audience":"public","observers":[{"id":"roof_1","remark":"First roof","last_seen_s":1},{"id":"roof:1","remark":"Second roof","last_seen_s":2}]}}"#
            } else if request.contains("/conditions ") {
                body = #"{"ok":true,"data":{"schema":"2.0","audience":"public","complete":true,"epoch":"ui","cursor":0,"events":[]}}"#
            } else if request.contains("/gnss/events ") {
                body = "event: status\ndata: {\"status\":\"connected\"}\n\n"
            } else {
                body = #"{"ok":true,"data":{"schema":"2.0","audience":"public","events":[]}}"#
            }
            let bytes = Data("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: \(body.utf8.count)\r\nConnection: close\r\n\r\n\(body)".utf8)
            connection.send(content: bytes, completion: .contentProcessed { _ in connection.cancel() })
        }
    }
}

@MainActor final class StationSelectionUITests: XCTestCase {
    func testAddTwoDiscoveredAndOneManualStation() throws {
        let ready = expectation(description: "collector ready")
        let fixture = try SelectionCollector(ready: ready)
        wait(for: [ready], timeout: 5)
        let port = try XCTUnwrap(fixture.listener.port)
        let app = XCUIApplication()
        let suite = "net.intsat.station.ui-test.\(UUID())"
        app.launchEnvironment["INTEGRITY_STATION_UI_TEST_SUITE"] = suite
        defer {
            UserDefaults(suiteName: suite)?.removePersistentDomain(forName: suite)
            try? FileManager.default.removeItem(at: FileManager.default.temporaryDirectory.appending(path: suite))
        }
        app.launch()
        defer { app.terminate() }
        let url = app.textFields["collector.url"]
        XCTAssertTrue(url.waitForExistence(timeout: 10))
        url.clickOrTap()
        #if os(macOS)
        url.typeKey("a", modifierFlags: .command)
        #else
        if let text = url.value as? String, !text.isEmpty { url.typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: text.count)) }
        #endif
        url.typeText("http://127.0.0.1:\(port.rawValue)")
        app.buttons["collector.connect"].clickOrTap()
        let first = app.buttons["station.discovered.roof_1"]
        XCTAssertTrue(first.waitForExistence(timeout: 10))
        first.clickOrTap()
        XCTAssertTrue(app.buttons["station.add"].waitForExistence(timeout: 5))
        app.buttons["station.add"].clickOrTap()
        let second = app.buttons["station.discovered.roof:1"]
        XCTAssertTrue(second.waitForExistence(timeout: 5))
        second.clickOrTap()
        XCTAssertFalse(app.buttons["station.discovered.roof_1"].isEnabled)
        XCTAssertFalse(second.isEnabled)
        let manual = app.textFields["station.manual"]
        manual.clickOrTap(); manual.typeText("manual:third")
        app.buttons["station.addManual"].clickOrTap()
        app.buttons["station.addDone"].clickOrTap()
        XCTAssertTrue(app.staticTexts["roof_1"].waitForExistence(timeout: 5))
        XCTAssertTrue(app.staticTexts["roof:1"].exists)
        XCTAssertTrue(app.staticTexts["manual:third"].exists)
        XCTAssertTrue(app.staticTexts["First roof"].exists)
        XCTAssertTrue(app.staticTexts["Second roof"].exists)
        fixture.listener.cancel()
    }
}

private extension XCUIElement {
    func clickOrTap() {
        #if os(macOS)
        click()
        #else
        tap()
        #endif
    }
}
