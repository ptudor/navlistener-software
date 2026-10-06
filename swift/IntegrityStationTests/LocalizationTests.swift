import Foundation
import SwiftUI
import Testing
#if os(macOS)
import AppKit
#endif
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
        AnyView(StationAssuranceView(assessment:try JSONDecoder().decode(StationAssessment.self,from:Data(#"{"state":"inconsistent","score":0,"unassured_domains":["position"],"spoofing_indicated":false,"engine_version":1,"config_hash":"sha256:00ff","mode":"mobile","max_speed_mps":30,"checks":[{"check":"motion_bound","domain":"position","state":"unassured","candidate":"assured","recovering_since":1,"metrics":{"speed_mps":41.5},"thresholds":{"max_speed_mps":30},"reasons":["motion_bound"]}]}"#.utf8)))),
        AnyView(VStack {forEachHealth})
    ]
    #if os(iOS)
    views.append(AnyView(ObserverSetupView()))
    ESPObserverTransport().disconnect()
    #endif
    for view in views {
        let content = view.environment(controller).frame(width:800,height:1000)
        #if os(macOS)
        // ImageRenderer cannot flatten AppKit-backed controls (the settings tab
        // view, text fields), and on macOS 27 it traps on them. A hosting view
        // draws them as AppKit does.
        let host = NSHostingView(rootView:content)
        host.frame = NSRect(x:0,y:0,width:800,height:1000)
        host.layoutSubtreeIfNeeded()
        let bitmap = try #require(host.bitmapImageRepForCachingDisplay(in:host.bounds))
        host.cacheDisplay(in:host.bounds,to:bitmap)
        #expect(bitmap.pixelsWide > 0 && bitmap.pixelsHigh > 0)
        #else
        let renderer = ImageRenderer(content:content)
        #expect(renderer.cgImage != nil)
        #endif
    }
}

@MainActor private var forEachHealth: some View {
    VStack {
        HealthLabel(state:.unknown);HealthLabel(state:.offline)
        HealthLabel(state:.critical);HealthLabel(state:.warning);HealthLabel(state:.ok)
    }
}
