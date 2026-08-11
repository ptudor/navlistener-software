import Testing
@testable import IntegrityStation

@Test
func sseAccumulatorParsesNamedMultilineEventAndCursor() throws {
    var parser = SSEAccumulator()
    #expect(parser.consume("id: 42") == nil)
    #expect(parser.consume("event: gnss") == nil)
    #expect(parser.consume("data: {\"message\":") == nil)
    #expect(parser.consume("data: \"clock jump\"}") == nil)
    let dispatched = parser.consume("")
    let frame = try #require(dispatched)

    #expect(frame.id == "42")
    #expect(frame.event == "gnss")
    #expect(frame.data == "{\"message\":\n\"clock jump\"}")
    #expect(parser.lastEventID == "42")
}

@Test
func sseAccumulatorHandlesResolvedEventsAndIgnoresComments() throws {
    var parser = SSEAccumulator()
    #expect(parser.consume(": heartbeat") == nil)
    #expect(parser.consume("event: resolved") == nil)
    #expect(parser.consume("data: {\"id\":42}") == nil)
    let dispatched = parser.consume("\r")
    let frame = try #require(dispatched)
    #expect(frame == SSEEventFrame(id: nil, event: "resolved", data: "{\"id\":42}"))
}

@Test
func sseAccumulatorUsesMessageDefaultAndRejectsNulCursor() throws {
    var parser = SSEAccumulator()
    _ = parser.consume("id: bad\0cursor")
    _ = parser.consume("data")
    let dispatched = parser.consume("")
    let frame = try #require(dispatched)
    #expect(frame.id == nil)
    #expect(frame.event == "message")
    #expect(frame.data == "")
    #expect(parser.lastEventID == nil)
}
