import Foundation
import Testing
@testable import IntegrityStation

/// Views that make their own collector requests use the controller's single
/// FeedClient. A FeedClient stored on a View struct is re-created, URLSession
/// and all, on every body evaluation, and detail screens re-evaluate at 1 Hz.
@MainActor @Test func viewsShareTheControllersFeedClient() throws {
    let network = URLSession(configuration: .ephemeral)
    defer { network.invalidateAndCancel() }
    let injected = FeedClient(session: network)
    let suite = "SharedFeedClientTests.\(UUID())"
    let defaults = try #require(UserDefaults(suiteName: suite))
    defer { defaults.removePersistentDomain(forName: suite) }
    let controller = AppController(store: StationStore(), settings: AppSettings(defaults: defaults),
                                   secureStore: RaceCredentials(), feedClient: injected)
    #expect(controller.feedClient == injected)
    let other = URLSession(configuration: .ephemeral)
    defer { other.invalidateAndCancel() }
    #expect(controller.feedClient != FeedClient(session: other))
    #expect(FeedClient(session: network) == injected)
}
