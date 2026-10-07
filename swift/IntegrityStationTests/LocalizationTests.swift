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

/// Every key the application source names must exist in the catalog and
/// resolve in the built bundle, and every catalog key must be named by the
/// source: a key missing from the catalog renders verbatim (a notification
/// body once read "events.unknown_type"), and an orphan lingers untranslated.
/// The expected-strings check above only proves listed keys resolve.
@Test func sourceAndCatalogNameTheSameKeys() throws {
    let application = Bundle(for: AppController.self)
    let sources = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
        .appending(path: "IntegrityStation", directoryHint: .isDirectory)
    let catalog = try #require(JSONSerialization.jsonObject(
        with: Data(contentsOf: sources.appending(path: "Resources/Localizable.xcstrings"))) as? [String: Any])
    let catalogKeys = Set(try #require(catalog["strings"] as? [String: Any]).keys)

    var referenced: [String: Set<String>] = [:]
    let enumerator = try #require(FileManager.default.enumerator(at: sources, includingPropertiesForKeys: nil))
    for case let url as URL in enumerator where url.pathExtension == "swift" {
        for key in try LocalizationKeyScan.keys(in: try String(contentsOf: url, encoding: .utf8)) {
            referenced[key, default: []].insert(url.lastPathComponent)
        }
    }
    #expect(referenced.count >= 300)

    let missing = referenced.filter { !catalogKeys.contains($0.key) }
    #expect(missing.isEmpty, "named in source, absent from the catalog: \(missing)")
    let unresolved = referenced.keys.filter { application.localizedString(forKey: $0, value: nil, table: nil) == $0 }
    #expect(unresolved.isEmpty, "resolve to themselves in the application bundle: \(unresolved.sorted())")
    let orphans = catalogKeys.subtracting(referenced.keys)
    #expect(orphans.isEmpty, "in the catalog, named by no source: \(orphans.sorted())")
}

/// Finds catalog keys (lowercase dotted identifiers) in the constructs this
/// code base uses to name them. A key named through a construct not listed
/// here surfaces as a catalog orphan in the test above, which is the cue to
/// extend the scan rather than the cue to drop the key.
enum LocalizationKeyScan {
    private static let key = #""([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)""#
    /// Each form captures, in group 1, the text that holds its key literals.
    private static let forms = [
        // String(localized: "key") and String(localized: flag ? "a" : "b"), the
        // argument possibly carrying one level of parentheses.
        #"String\(localized:\s*((?:[^()]|\([^()]*\))*)\)"#,
        // SwiftUI initializers and helpers whose first argument is a
        // LocalizedStringKey literal.
        #"\b(?:Text|Button|InstrumentCard|DisclosureGroup|Section|TextField|SecureField|Picker|Label|Link|Toggle|ProgressView|LabeledContent|note|navigationTitle)\(\s*("[^"]+")"#,
        // label: "key", title: "key", prompt: "key" arguments.
        #"\b(?:label|title|prompt):\s*("[^"]+")"#,
        // let key: LocalizedStringKey = switch ... { case .x: "key" ... }
        #"LocalizedStringKey\s*=\s*switch[^{]*\{([^}]*)\}"#,
        // return "key" and return flag ? "key" : "key" from LocalizedStringKey
        // functions; a literal followed by anything else (a path, a call) is
        // not a key.
        #"\breturn\s+((?:[^\n"]*\?\s*)?"[^"\n]+"(?:\s*:\s*"[^"\n]+")?)\s*\}?\s*$"#
    ]

    static func keys(in text: String) throws -> Set<String> {
        let literal = try Regex(key)
        var found: Set<String> = []
        for form in forms {
            let pattern = try Regex(form).anchorsMatchLineEndings()
            for match in text.matches(of: pattern) {
                guard let range = match.output[1].range else { continue }
                let span = text[range]
                // SF Symbol names look like keys; they never share a span with one.
                if span.contains("systemName:") || span.contains("systemImage:") { continue }
                for hit in span.matches(of: literal) {
                    if let keyRange = hit.output[1].range { found.insert(String(text[keyRange])) }
                }
            }
        }
        return found
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
