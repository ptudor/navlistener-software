import Foundation

/// Reconnect schedule shared by the live event stream and re-discovery:
/// exponential 1, 2, 4 … s capped at 30 s, with ±25 % jitter so a fleet of
/// clients that lost the collector together does not reconnect in lockstep
/// against its connection cap. The base resets to one second on success.
enum ReconnectBackoff {
    static let initialSeconds = 1
    static let maximumSeconds = 30
    static let jitter = 0.25

    static func next(after seconds: Int) -> Int {
        min(seconds * 2, maximumSeconds)
    }

    static func delay(seconds: Int) -> Duration {
        var generator = SystemRandomNumberGenerator()
        return delay(seconds: seconds, using: &generator)
    }

    /// The jittered delay never exceeds the cap, so at the cap the spread is
    /// downward only; clients still disperse.
    static func delay(seconds: Int, using generator: inout some RandomNumberGenerator) -> Duration {
        let factor = Double.random(in: (1 - jitter)...(1 + jitter), using: &generator)
        let jittered = min(Double(seconds) * factor, Double(maximumSeconds))
        return .seconds(jittered)
    }
}
