import Testing
@testable import IntegrityStation

@Test
func uptimeAndAgeFormattingStayCompact() {
    #expect(StationFormat.uptime(seconds: nil) == "—")
    #expect(StationFormat.uptime(seconds: 59) == "59s")
    #expect(StationFormat.uptime(seconds: 3_661) == "1h 1m")
    #expect(StationFormat.uptime(seconds: 90_061) == "1d 1h")
    #expect(StationFormat.age(seconds: 4) == "now")
    #expect(StationFormat.age(seconds: 90) == "1m ago")
}

@Test
func measurementFormattingPreservesUnknown() {
    #expect(StationFormat.clockDrift(nanoseconds: nil) == "—")
    #expect(StationFormat.clockDrift(nanoseconds: 123.46) == "123.5 ns")
    #expect(StationFormat.clockDrift(nanoseconds: -1_250) == "-1.2 µs")
    #expect(StationFormat.carrierToNoise(nil) == "—")
    #expect(StationFormat.carrierToNoise(47) == "47 dB-Hz")
}

@Test func oversizedFormattingNeverTraps() {
    for value in [1e30, Double.greatestFiniteMagnitude, Double.infinity, -Double.infinity, Double.nan, -1] {
        #expect(StationFormat.uptime(seconds: value) == StationFormat.unknown)
        #expect(StationFormat.age(seconds: value) == StationFormat.unknown)
    }
    let boundary = Double(Int.max) // rounds UP, and cannot itself become Int
    #expect(StationFormat.uptime(seconds: boundary) == StationFormat.unknown)
    #expect(StationFormat.uptime(seconds: boundary.nextUp) == StationFormat.unknown)
    #expect(StationFormat.uptime(seconds: boundary.nextDown) != StationFormat.unknown)
    let daysBoundary = boundary * 86_400
    #expect(StationFormat.age(seconds: daysBoundary) == StationFormat.unknown)
    #expect(StationFormat.age(seconds: daysBoundary.nextUp) == StationFormat.unknown)
    #expect(StationFormat.age(seconds: daysBoundary.nextDown) != StationFormat.unknown)
    #expect(StationFormat.age(seconds: 86_399) == "23h ago")
    #expect(StationFormat.age(seconds: 86_400) == "1d ago")
    #expect(StationFormat.uptime(seconds: 86_400) == "1d 0h")
}
