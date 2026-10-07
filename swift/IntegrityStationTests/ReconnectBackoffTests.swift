import Foundation
import Testing
@testable import IntegrityStation

/// Deterministic generator so the jitter bounds can be checked exhaustively.
private struct SplitMix64: RandomNumberGenerator {
    var state: UInt64
    mutating func next() -> UInt64 {
        state &+= 0x9E37_79B9_7F4A_7C15
        var z = state
        z = (z ^ (z >> 30)) &* 0xBF58_476D_1CE4_E5B9
        z = (z ^ (z >> 27)) &* 0x94D0_49BB_1331_11EB
        return z ^ (z >> 31)
    }
}

private func seconds(_ duration: Duration) -> Double {
    let parts = duration.components
    return Double(parts.seconds) + Double(parts.attoseconds) / 1e18
}

@Test func reconnectDelaysAreJitteredSoClientsDoNotReconnectInLockstep() {
    // Two clients losing the collector together must not pick the same delay.
    let delays = (0..<16).map { _ in seconds(ReconnectBackoff.delay(seconds: 4)) }
    #expect(Set(delays).count > 1)
    var generator = SplitMix64(state: 7)
    let first = seconds(ReconnectBackoff.delay(seconds: 8, using: &generator))
    let second = seconds(ReconnectBackoff.delay(seconds: 8, using: &generator))
    #expect(first != second)
}

@Test func reconnectDelaysStayWithinJitterAndCap() {
    var generator = SplitMix64(state: 42)
    for base in [1, 2, 4, 8, 16, 30] {
        for _ in 0..<200 {
            let delay = seconds(ReconnectBackoff.delay(seconds: base, using: &generator))
            #expect(delay >= Double(base) * (1 - ReconnectBackoff.jitter), "base \(base)")
            #expect(delay <= min(Double(base) * (1 + ReconnectBackoff.jitter), Double(ReconnectBackoff.maximumSeconds)), "base \(base)")
        }
    }
}

@Test func reconnectScheduleDoublesToTheCapAndResets() {
    #expect(ReconnectBackoff.initialSeconds == 1)
    #expect(ReconnectBackoff.next(after: 1) == 2)
    #expect(ReconnectBackoff.next(after: 16) == 30)
    #expect(ReconnectBackoff.next(after: 30) == 30)
    #expect(ReconnectBackoff.maximumSeconds == 30)
}
