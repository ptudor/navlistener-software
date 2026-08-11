import Foundation
import Testing
@testable import IntegrityStation

@MainActor
@Test
func appSettingsPersistStationsLabelsAndAppearance() throws {
    let suiteName = "IntegrityStationTests.\(UUID().uuidString)"
    let defaults = try #require(UserDefaults(suiteName: suiteName))
    defer { defaults.removePersistentDomain(forName: suiteName) }

    let settings = AppSettings(defaults: defaults)
    #expect(settings.appearance == .dark)
    #expect(settings.notifyOffline)

    settings.serverURLString = "https://collector.invalid"
    settings.stationIDs = ["rx-observer16.example.invalid"]
    settings.labelsByStationID = ["rx-observer16.example.invalid": "Roof"]
    settings.appearance = .system

    let reloaded = AppSettings(defaults: defaults)
    #expect(reloaded.serverURLString == "https://collector.invalid")
    #expect(reloaded.stationIDs == ["rx-observer16.example.invalid"])
    #expect(reloaded.label(for: "rx-observer16.example.invalid") == "Roof")
    #expect(reloaded.appearance == .system)
}

@MainActor
@Test
func controllerValidatesCollectorAndOpaqueStationIdentifiers() {
    #expect(AppController.validServerURL(" https://collector.invalid/site ")?.host == "collector.invalid")
    #expect(AppController.validServerURL("http://192.168.1.20:8080") != nil)
    #expect(AppController.validServerURL("file:///tmp/feed") == nil)
    #expect(AppController.validServerURL("collector.invalid") == nil)

    #expect(AppController.isValidStationID("rx-observer16.example.invalid"))
    #expect(AppController.isValidStationID("00-04-a3-ff-fe-12-34-56"))
    #expect(!AppController.isValidStationID("station/path"))
    #expect(!AppController.isValidStationID(""))
}
