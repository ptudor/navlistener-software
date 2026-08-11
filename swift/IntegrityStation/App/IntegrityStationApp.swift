import SwiftUI

@main
struct IntegrityStationApp: App {
    @State private var controller = AppController()

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
