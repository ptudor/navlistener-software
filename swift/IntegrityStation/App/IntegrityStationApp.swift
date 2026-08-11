import SwiftUI

@main
struct IntegrityStationApp: App {
    var body: some Scene {
        WindowGroup {
            AppRootView()
        }

        #if os(macOS)
        MenuBarExtra {
            MenuBarStatusView()
        } label: {
            Label(String(localized: "menu_bar.title"), systemImage: "circle.dotted")
        }
        .menuBarExtraStyle(.window)

        Settings {
            SettingsView()
        }
        #endif
    }
}
