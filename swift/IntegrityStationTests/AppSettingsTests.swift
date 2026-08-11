import Foundation
import Testing
@testable import IntegrityStation

@MainActor
@Test
func appSettingsPartitionsStationsAndLabelsByReadScope() throws {
    let suiteName = "IntegrityStationTests.\(UUID().uuidString)"
    let defaults = try #require(UserDefaults(suiteName: suiteName))
    defer { defaults.removePersistentDomain(forName: suiteName) }

    let settings = AppSettings(defaults: defaults)
    #expect(settings.appearance == .dark)
    #expect(settings.notifyOffline)

    let organization = try #require(ReadAudience("organization:customer-a"))
    let organizationKey = AudienceCacheKey(
        server: "https://collector.invalid",
        principal: "viewer-a",
        audience: organization,
        authorizationRevision: "grant-v1"
    )
    let publicKey = AudienceCacheKey(
        server: "https://collector.invalid",
        principal: AudienceCacheKey.anonymousPrincipal,
        audience: .publicAudience,
        authorizationRevision: "public"
    )

    settings.activateScope(organizationKey)
    settings.stationIDs = ["rx-observer16.example.invalid"]
    settings.labelsByStationID = ["rx-observer16.example.invalid": "Roof"]
    settings.appearance = .system
    settings.setPreferredAudience(organization, forServer: organizationKey.server)

    settings.activateScope(publicKey)
    #expect(settings.stationIDs.isEmpty)
    settings.stationIDs = ["public-station"]

    let reloaded = AppSettings(defaults: defaults)
    #expect(reloaded.serverURLString.isEmpty)
    reloaded.activateScope(organizationKey)
    #expect(reloaded.stationIDs == ["rx-observer16.example.invalid"])
    #expect(reloaded.label(for: "rx-observer16.example.invalid") == "Roof")
    #expect(reloaded.appearance == .system)
    #expect(reloaded.preferredAudience(forServer: organizationKey.server) == organization)

    reloaded.activateScope(publicKey)
    #expect(reloaded.stationIDs == ["public-station"])
    #expect(reloaded.label(for: "rx-observer16.example.invalid") == nil)
}

@MainActor
@Test
func controllerValidatesCollectorAndOpaqueStationIdentifiers() {
    #expect(AppController.validServerURL(" https://collector.invalid/site ")?.host == "collector.invalid")
    #expect(AppController.validServerURL("http://192.168.1.20:8080") != nil)
    #expect(AppController.validServerURL("file:///tmp/feed") == nil)
    #expect(AppController.validServerURL("collector.invalid") == nil)
    #expect(AppController.validServerURL("https://user:secret@collector.invalid") == nil)
    #expect(AppController.validServerURL("https://collector.invalid/?token=secret") == nil)

    #expect(AppController.isValidStationID("rx-observer16.example.invalid"))
    #expect(AppController.isValidStationID("00-04-a3-ff-fe-12-34-56"))
    #expect(!AppController.isValidStationID("station/path"))
    #expect(!AppController.isValidStationID(""))
}
