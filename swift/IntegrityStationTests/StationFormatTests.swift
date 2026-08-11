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
