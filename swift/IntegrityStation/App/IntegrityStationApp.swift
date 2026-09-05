import SwiftUI

@main
struct IntegrityStationApp: App {
    @State private var controller: AppController

    init() {
        #if DEBUG
        if let suite = ProcessInfo.processInfo.environment["INTEGRITY_STATION_UI_TEST_SUITE"],
           suite.hasPrefix("net.intsat.station.ui-test."), let defaults = UserDefaults(suiteName: suite) {
            defaults.removePersistentDomain(forName: suite)
            let cache = SnapshotCache(directory: FileManager.default.temporaryDirectory.appending(path: suite))
            _controller = State(initialValue: AppController(store: StationStore(cache: cache), settings: AppSettings(defaults: defaults), secureStore: UITestConnectionStore()))
            return
        }
        #endif
        _controller = State(initialValue: AppController())
    }

    var body: some Scene {
        WindowGroup {
            AppRootView()
                .environment(controller)
        }

        #if os(macOS)
        MenuBarExtra {
            MenuBarStatusView()
                .environment(controller)
        } label: {
            Label(
                controller.store.rollupHealth.localizedName,
                systemImage: controller.store.rollupHealth.systemImage
            )
        }
        .menuBarExtraStyle(.window)

        Settings {
            SettingsView()
                .environment(controller)
                .preferredColorScheme(controller.settings.appearance.colorScheme)
        }
        #endif
    }
}

#if DEBUG
// Explicit UI-test launches use isolated preferences/cache and an ephemeral
// credential store, so exercising onboarding cannot alter an operator's setup.
private actor UITestConnectionStore: SecureConnectionStoring {
    var server: String?
    var tokens: [String:String] = [:]
    func lastServerURL() -> String? { server }
    func saveLastServerURL(_ value: String) { server = value }
    func token(forServer server: String) -> String? { tokens[server] }
    func saveToken(_ token: String, forServer server: String) { tokens[server] = token }
    func deleteToken(forServer server: String) { tokens.removeValue(forKey: server) }
}
#endif
