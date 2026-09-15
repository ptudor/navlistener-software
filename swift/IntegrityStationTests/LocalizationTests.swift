import Foundation
import SwiftUI
import Testing
@testable import IntegrityStation

private final class LocalizationBundleMarker: NSObject {}

@Test func everyCatalogKeyResolvesInTheApplicationBundle() throws {
    let application = Bundle(for: AppController.self)
    #expect(application.bundleURL.pathExtension == "app")
    let fixtures = Bundle(for: LocalizationBundleMarker.self)
    let url = try #require(fixtures.url(forResource: "LocalizationExpected", withExtension: "json"))
    let expected = try JSONDecoder().decode([String:String].self, from: Data(contentsOf: url))
    #expect(expected.count >= 125)
    for (key, english) in expected {
        // Exact equality checks every translated string and its format
        // placeholders, through the default table used by both UI targets.
        #expect(application.localizedString(forKey:key,value:nil,table:nil) == english, "Default-table lookup: \(key)")
    }
}

@MainActor @Test func localizedViewsRenderOnThisPlatform() throws {
    let suite = "LocalizationSmoke.\(UUID())"
    let defaults = try #require(UserDefaults(suiteName:suite))
    defer {defaults.removePersistentDomain(forName:suite)}
    let store = StationStore()
    let controller = AppController(store:store,settings:AppSettings(defaults:defaults))
    let payload = try JSONDecoder().decode(ObserversPayload.self,from:Data(#"{"schema":"2.0","audience":"public","observers":[{"id":"roof","remark":"User's unchanged label","last_seen_s":2,"svs":{"G07@0":{"name":"G07","gnssid":0,"azi_deg":45,"elev_deg":35,"cn0_db_hz":42}}}]}"#.utf8))
    let key = AudienceCacheKey(server:"https://collector.invalid",principal:"anonymous",audience:.publicAudience,authorizationRevision:"public")
    try store.apply(ObserversSnapshot(receivedAt:Date(),scope:key,serverTime:nil,payload:payload),cached:false)
    var views: [AnyView] = [
        AnyView(OnboardingView()), AnyView(SettingsView()),
        AnyView(StationDetailView(stationID:"roof")),
        AnyView(SkyPlotView(signals:payload.observers?.first?.svs ?? [:])),
        AnyView(VStack {forEachHealth})
    ]
    #if os(iOS)
    views.append(AnyView(ObserverSetupView()))
    ESPObserverTransport().disconnect()
    #endif
    for view in views {
        let renderer = ImageRenderer(content:view.environment(controller).frame(width:800,height:1000))
        #expect(renderer.cgImage != nil)
    }
}

@MainActor private var forEachHealth: some View {
    VStack {
        HealthLabel(state:.unknown);HealthLabel(state:.offline)
        HealthLabel(state:.critical);HealthLabel(state:.warning);HealthLabel(state:.ok)
    }
}
